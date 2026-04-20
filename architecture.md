# GoPurge — Architecture

## Overview

GoPurge is a read-only CLI diagnostic tool written in Go. It scans an Unreal Engine
project directory and produces a report identifying three categories of waste:
**duplicate assets**, **unreferenced assets**, and **large files**. It never deletes
anything — the report is always the final output, and the user decides what to remove
after reviewing it in the Unreal Editor.

---

## Package Structure

```
GoPurge/
├── GoPurge.go            — Entry point, CLI flag parsing, pipeline orchestration
├── preflight/
│   └── preflight.go      — Pre-flight validation (uproject, editor process, Content/ access)
├── discovery/
│   └── discovery.go      — Recursive directory walk, symlink handling, Windows MAX_PATH
├── scanner/
│   ├── scanner.go        — Orchestrates duplicate + large-file detection
│   ├── duplicates.go     — Multi-stage duplicate detection (size → header → SHA256)
│   └── largefile.go      — Large file threshold filtering
├── analyzer/
│   ├── analyzer.go       — Reference analysis orchestration and raw-scan fallback
│   └── uasset.go         — Structured UAsset Package File Summary parser
├── reporter/
│   └── reporter.go       — JSON / CSV report writer, stdout summary
└── model/
    └── model.go          — Shared data types (FileEntry, FileGroup, Report)
```

---

## Core Data Types (`model`)

All packages share these types. Defining them up-front prevents import cycles.

```go
// FileEntry represents a single discovered asset file.
type FileEntry struct {
    Path           string // absolute, \\?\-prefixed on Windows if > 260 chars
    Size           int64  // bytes
    SHA256         string // populated only after hashing; empty until then
    VerifyManually bool   // true if this entry may be a false positive (e.g. DataTable)
}

// FileGroup is a set of files confirmed to be identical (same SHA256).
type FileGroup struct {
    Hash  string
    Files []FileEntry
}

// Report is the top-level output structure written to disk.
type Report struct {
    GeneratedAt     time.Time
    ProjectDir      string
    Duplicates      []FileGroup  // groups of 2+ identical files
    LargeFiles      []FileEntry  // files exceeding the size threshold
    Unreferenced    []FileEntry  // assets with zero inbound references
    TotalWasteBytes int64        // sum of reclaimable bytes across all categories
    Warnings        []string     // non-fatal issues encountered during the scan
}
```

---

## Component Responsibilities

### `main`
- Parses CLI flags: `-project-dir`, `-output` (json|csv, default json), `-workers` (default 4), `-large-threshold` (default 100MB).
- Calls each stage in sequence, passing results forward.
- Handles early-exit paths (validation failure, no assets found).
- Does **not** own any business logic — it is a pure orchestrator.

### `preflight`
Validates the environment before any I/O-intensive work begins:
1. A `.uproject` file exists directly inside `-project-dir`.
2. The Unreal Editor process (`UE4Editor.exe` / `UnrealEditor.exe`) is **not** running.
3. The `Content/` subdirectory exists and is readable.

Returns a typed error for each failure so `main` can print a targeted hint.

### `discovery`
Walks `Content/` using `filepath.WalkDir`. Rules:
- Skip directories named `Intermediate`, `DerivedDataCache`, `Binaries`, `Saved`.
- Call `os.Lstat` on every entry — skip (log warning) if it is a symlink or junction.
- Prepend `\\?\` to paths exceeding 260 characters on Windows.
- Collect only files with extensions `.uasset` and `.umap`.

Returns `[]model.FileEntry` with `Path` and `Size` populated. `SHA256` is empty at this stage.

### `scanner`

#### Duplicate Detection (multi-stage, worker pool)
To avoid hashing every file, detection is staged:

1. **Stage A — Size Grouping:** Build `map[int64][]FileEntry`. Discard any size bucket with only one entry.
2. **Stage B — Header Sampling:** Read the first 1 KB of each remaining file. Discard entries whose 1 KB header is unique within its size bucket.
3. **Stage C — Full SHA256:** Hash surviving files in full. Uses a fan-out/fan-in worker pool.

**Concurrency model (fan-out/fan-in):**

```
main goroutine
  │
  ├─ feeds FileEntry into  jobs chan FileEntry   (buffered, cap = workers)
  │
  ├─ N worker goroutines each:
  │     read job from jobs
  │     stream file through sha256.New() via io.Copy   ← keeps RAM bounded
  │     send result/error to results chan hashResult
  │
  └─ collector goroutine
        reads from results chan until closed
        groups by SHA256
        returns []FileGroup where len(Files) >= 2
```

`hashResult` carries either a populated `FileEntry` or an `error`. Workers that hit
"access denied" send the error; the collector logs the warning and continues — it
does not abort the entire run.

#### Large File Detection
Simple single-pass filter over the full asset list: retain entries where `Size >= threshold`.
No concurrency needed — this is CPU-cheap.

### `analyzer`
Receives the **full** asset list from Discovery as its "known universe".

Reference scanning strategy:
1. **Structured parse (primary):** For each `.uasset` / `.umap`, `uasset.go` validates
   the magic number, walks the version-dependent `PackageFileSummary` header fields to
   locate the Name Map, and extracts every `/Game/...` path directly from it. The Name
   Map is an uncompressed flat array that resides before any compressed payload, so this
   eliminates false matches from random binary data. Handles all header variants:
   UE4 (`LegacyFileVersion` −4 through −7), UE5 pre-5.6 (−8, `FileVersionUE5` < 1016),
   and UE5.6+ (`FileVersionUE5` ≥ 1016, with `SavedHash` + `SectionSixOffset` inserted
   before the `CustomVersionContainer`).
2. **Raw scan fallback:** If header parsing fails (e.g. encrypted or obfuscated assets),
   the file is scanned with a raw `/Game/<path>` byte-pattern search and a warning is
   appended to `Warnings`.
3. Scan `Source/` with `regexp` for the C++ token `FSoftObjectPath`.
4. Build a `referenced map[string]bool` keyed on asset path.
5. Any asset in the known universe with zero inbound references is flagged as unreferenced.

**False-positive risk:** Game data files (DataTables, PrimaryAssetLabels, config-driven
assets) are loaded at runtime by string path and will appear unreferenced in a static
scan. The report annotates these entries with a `"VerifyManually": true` flag and
a note explaining the risk.

### `reporter`
Accepts the fully-populated `model.Report` and writes it to disk.

- **JSON (default):** `encoding/json` with `MarshalIndent` for human readability.
- **CSV:** One row per flagged file; columns: `Category`, `Path`, `SizeBytes`, `SHA256`, `VerifyManually`, `Notes`.
- Prints a one-page summary to stdout:
  ```
  GoPurge scan complete
  ─────────────────────────────────────
  Duplicates:    42 files in 18 groups   (1.2 GB reclaimable)
  Large files:   7 files                 (3.4 GB)
  Unreferenced:  130 files               (890 MB)
  ─────────────────────────────────────
  Total reclaimable:  ~5.5 GB
  Report written to: ./gopurge_report.json

  ⚠ Never delete automatically. Review in Unreal Editor first.
  ```

---

## Error Handling Strategy

| Scenario | Behaviour |
|---|---|
| `.uproject` not found | Fatal — exit 1 with hint |
| Unreal Editor is running | Fatal — exit 1 with hint to close editor |
| `Content/` not accessible | Fatal — exit 1 |
| No assets found after walk | Graceful exit 0 — "nothing to do" |
| File access denied during scan | Log warning to stderr, skip file, continue |
| Symlink / junction encountered | Log warning to stderr, skip, continue |
| Path > 260 chars (Windows) | Apply `\\?\` prefix, continue |
| Worker hash error | Log warning to stderr, skip file, continue |

Fatal errors abort before any expensive I/O. Non-fatal errors are collected and
included in the report under a top-level `Warnings []string` field.

---

## Key Constraints

- **Never delete files.** GoPurge is a read-only diagnostic tool.
- **RAM cap ~50 MB** during hashing — enforced by streaming via `io.Copy`.
- **Must run with Unreal Editor closed** — enforced by pre-flight check.
- **No external dependencies** beyond the Go standard library (at least initially).

---

## UAsset Package File Summary Structure

The binary layout parsed by `analyzer/uasset.go`. The header is not fixed-size — its
layout varies by engine version. We walk fields sequentially to locate `NameCount` and
`NameOffset`, then seek directly to the Name Map.

### Fixed Header Prefix (all versions)

| Offset | Size | Type | Field | Notes |
|--------|------|------|-------|-------|
| 0 | 4 | `uint32` | `Magic` | Must equal `0x9E2A83C1`; file rejected on mismatch |
| 4 | 4 | `int32` | `LegacyFileVersion` | Negative for all UE4/UE5 assets (`-4` through `-8`); determines variant |
| 8 | 4 | `int32` | `LegacyUE3Version` | **Absent** when `LegacyFileVersion == -4`; otherwise present and skipped |
| 8 or 12 | 4 | `int32` | `FileVersionUE4` | Always present; ≥ 504 means Name Map entries carry a 4-byte hash suffix |
| +4 | 4 | `int32` | `FileVersionUE5` | **Present only** when `LegacyFileVersion ≤ -8`; ≥ 1016 means UE5.6+ layout |
| +4 or +8 | 4 | `int32` | `FileVersionLicenseeUE` | Always present; skipped |

### Version-Dependent Middle Section

After `FileVersionLicenseeUE` the layout forks based on `FileVersionUE5`:

#### Branch A — UE5.6+ (`FileVersionUE5 ≥ 1016`, `PACKAGE_SAVED_HASH`)

| Rel. offset | Size | Type | Field | Notes |
|-------------|------|------|-------|-------|
| +0 | 20 | `FIoHash` | `SavedHash` | SHA-based package content hash; skipped entirely |
| +20 | 4 | `int32` | `SectionSixOffset` | Offset to payload section 6; skipped |
| +24 | 4 | `int32` | `CustomVersions count` | Present if `LegacyFileVersion ≤ -2` |
| +28 | count × 20 | `Optimized[]` | `CustomVersions entries` | `FGuid` (16 bytes) + `int32` version; skipped |

#### Branch B — UE5.0–5.5 and all UE4 (`FileVersionUE5 < 1016`)

| Rel. offset | Size | Type | Field | Notes |
|-------------|------|------|-------|-------|
| +0 | 4 | `int32` | `CustomVersions count` | Present if `LegacyFileVersion ≤ -2` |
| +4 | varies | `CustomVersion[]` | `CustomVersions entries` | Entry size depends on `LegacyFileVersion` — see table below |
| after | 4 | `int32` | `SectionSixOffset` | Skipped |

##### CustomVersion entry sizes by `LegacyFileVersion`

| `LegacyFileVersion` | Format | Entry size |
|---------------------|--------|------------|
| `-2` | `FEnumCustomVersion` — `int32 Tag` + `int32 Version` | 8 bytes |
| `-3`, `-4`, `-5` | `FGuidCustomVersion` — `FGuid` (16) + `int32` (4) + `FString` (var) | variable |
| `≤ -6` | Optimized — `FGuid` (16) + `int32 Version` (4) | 20 bytes |

### Fixed Fields After the Version Fork (all versions)

| Size | Type | Field | Notes |
|------|------|-------|-------|
| variable | `FString` | `FolderName` | Mount point string; skipped |
| 4 | `uint32` | `PackageFlags` | Asset flags bitmask; skipped |
| **4** | **`int32`** | **`NameCount`** | **Number of Name Map entries — read and retained** |
| **4** | **`int32`** | **`NameOffset`** | **Byte offset to the Name Map — read and retained** |
| varies | … | *(remaining header fields)* | Skipped — we seek directly to `NameOffset` |

### Name Map Entry Format (repeated `NameCount` times, starting at `NameOffset`)

| Size | Type | Field | Notes |
|------|------|-------|-------|
| variable | `FString` | `Name` | ANSI when length `> 0`; UTF-16 LE when length `< 0` |
| 4 | `2 × uint16` | Hash suffix | **Present only** when `FileVersionUE4 ≥ 504`; `NonCasePreservingHash` + `CasePreservingHash`; discarded |

#### `FString` encoding rules

| Serialised `int32` length | Encoding | Body size |
|---------------------------|----------|-----------|
| `0` | Empty string | 0 bytes (nothing follows) |
| `> 0` | ANSI / Latin-1, null-terminated | `length` bytes |
| `< 0` | UTF-16 LE, null-terminated | `|length| × 2` bytes |
