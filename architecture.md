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
│   └── analyzer.go       — Reference analysis (Soft Object Paths, FSoftObjectPath)
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
1. For each `.uasset` / `.umap` file, read the binary content and search for byte
   sequences matching the pattern `/Game/<path>` (Unreal's Soft Object Path format).
2. Scan `Source/` Go-style with `regexp` for the C++ token `FSoftObjectPath`.
3. Build a `referenced map[string]bool` keyed on asset path.
4. Any asset in the known universe that has zero entries in `referenced` is flagged as unreferenced.

**False-positive risk:** Game data files (DataTables, PrimaryAssetLabels, config-driven
assets) are loaded at runtime by string path and will appear unreferenced in a static
scan. The report must annotate these entries with a `"VerifyManually": true` flag and
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
