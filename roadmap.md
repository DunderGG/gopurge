# GoPurge — Roadmap

This document tracks planned improvements, known limitations, and future feature ideas.
Items are grouped by theme and loosely ordered by priority within each section.

---

## 🔧 Correctness & Robustness

- [x] **Pre-flight validation**
  Verify that a `.uproject` exists, that `Content/` is accessible, and that the Unreal
  Editor process is not running before any expensive I/O begins.

- [x] **Symlink and NTFS junction guard**
  Use `os.Lstat` on every discovered entry to detect and skip symbolic links and
  junctions, preventing infinite loops and double-counting of physical bytes.

- [x] **Windows MAX_PATH handling**
  Automatically prepend the `\\?\` extended-path prefix for paths exceeding 260
  characters so the tool works on deep Unreal project trees without user intervention.

- [x] **`VerifyManually` false-positive annotation**
  Assets whose names match known runtime-loaded patterns (DataTable, CurveTable,
  PrimaryAssetLabel, GameData) are flagged with `VerifyManually: true` in the report
  rather than silently omitted or treated as definitive waste.

- [x] **Non-fatal error collection**
  Access-denied errors, failed stats, and skipped symlinks are collected into a
  `Warnings []string` field on the report rather than aborting the scan.

- [ ] **Improve reference analysis accuracy**
  The current binary scan searches for raw `/Game/` strings, which can produce false matches
  in compressed or encrypted asset sections. Investigate parsing the UAsset serialisation
  header to locate the Name Map and Import Table before scanning for references.

- [ ] **Handle redirectors (`.uasset` with `ObjectRedirector` class)**
  Unreal creates redirector assets when content is moved. These appear unreferenced but
  are often still needed. Detect and annotate them separately in the report rather than
  flagging them as unreferenced.

- [ ] **Respect `.gitignore` / custom ignore files**
  Add a `-ignore` flag that accepts a path to a file of glob patterns (similar to
  `.gitignore`) so users can exclude specific directories or asset types from the scan.

- [ ] **Cross-platform process detection hardening**
  The current `ps`-based check on Unix can miss editor instances launched in unusual ways.
  Consider `/proc` scanning on Linux or `pgrep` as a more reliable alternative.

- [ ] **Handle UE5 Virtual Assets and World Partition chunks**
  UE5 projects using World Partition generate large numbers of auto-generated chunk assets
  that should never be flagged as waste. Add detection for these patterns.

---

## ⚡ Performance

- [x] **Multi-stage duplicate detection to minimise hashing**
  Files are first grouped by size, then by 1 KB header, and only the surviving
  candidates are fully hashed — avoiding SHA-256 computation on the vast majority
  of assets in a typical project.

- [x] **Fan-out/fan-in worker pool for SHA-256 hashing**
  A configurable number of goroutines hash files concurrently via a `jobs` /
  `results` channel pair, keeping all CPU cores busy during the most expensive stage.

- [x] **Streaming SHA-256 via `io.Copy`**
  File contents are streamed through `sha256.New()` in fixed-size chunks rather than
  loaded fully into memory, capping RAM usage at ~50 MB regardless of asset size.

- [ ] **Persist a hash cache between runs**
  Write a `.gopurge_cache` file (keyed by path + modification time) so unchanged files
  do not need to be re-hashed on subsequent scans. This would make incremental scans on
  large projects significantly faster.

- [ ] **Parallel reference analysis**
  The current `AnalyzeReferences` pass is single-threaded. Apply the same fan-out/fan-in
  worker pool pattern used in the scanner to read and scan asset binaries concurrently.

- [ ] **Memory-mapped file I/O for large assets**
  For files above a configurable threshold (e.g. 500 MB), consider `mmap` instead of
  `io.Copy` to avoid repeated system call overhead during sequential reads.

- [ ] **Progress reporting**
  Long scans give no feedback. Add a lightweight progress ticker (files scanned / total,
  current stage) printed to stderr so users know the tool hasn't stalled.

---

## 📊 Reporting

- [x] **JSON report output**
  The full `model.Report` is serialised with `json.MarshalIndent` into a
  human-readable `.json` file containing all three waste categories and warnings.
  Available via `-output=json`.

- [x] **CSV report output**
  An alternative flat `.csv` format is available via `-output=csv`, with one row per
  flagged file and columns for Category, Path, SizeBytes, SHA256, VerifyManually,
  and Notes.

- [x] **`computeTotalWaste` de-duplication**
  Reclaimable bytes are calculated across all three categories without double-counting
  files that appear in more than one category (e.g. a large file that is also a
  duplicate).

- [x] **Stdout summary on completion**
  A concise one-page summary (duplicate groups, large files, unreferenced count, total
  reclaimable GB) is always printed to stdout regardless of the chosen output format.

- [ ] **Per-category reclaimable size breakdown in JSON**
  The current `TotalWasteBytes` is a single aggregate. Add `DuplicateWasteBytes`,
  `LargeFileBytes`, and `UnreferencedBytes` fields to `model.Report` so tooling can
  process each category independently.

- [x] **HTML report output (default)**
  Generates a self-contained dark/light-theme HTML dashboard with Chart.js charts
  (waste-by-category doughnut, top large files bar), DataTables for sortable/searchable
  file lists, and an accordion for duplicate groups — easier to share with non-technical
  team members than raw JSON or CSV.

  - [ ] **Portable app icon**
    Embed `appicon.png` into the HTML report as a base64 data URI so the icon is visible
    regardless of where the `.html` file is opened. Requires passing the PNG bytes through
    the reporter pipeline and typing the field as `template.URL` to bypass Go's
    `html/template` sanitiser.

- [ ] **Diff mode — compare two reports**
  Add a `gopurge diff report-a.json report-b.json` subcommand that shows which waste
  items were resolved and which are newly introduced between two scan runs.

- [ ] **Exit code contract**
  Document and enforce a consistent exit code contract:
  `0` = clean scan, `1` = fatal error, `2` = waste found. This allows GoPurge to be
  used as a gate in CI pipelines.

---

## 🧪 Testing

- [ ] **Unit tests for all packages**
  Each package (`discovery`, `scanner`, `analyzer`, `reporter`) should have a `_test.go`
  file with table-driven tests using fixture data (synthetic directory trees and minimal
  `.uasset` stubs).

- [ ] **Integration test with a minimal fake UE project**
  Create a `testdata/` directory containing a minimal fake Unreal project structure
  (`.uproject`, `Content/`, `Source/`) used by an end-to-end test that runs the full
  pipeline and validates the report output.

- [ ] **Fuzz testing for the binary path extractor**
  The `extractSoftObjectPaths` function in the analyzer reads arbitrary binary data.
  Add a `FuzzExtractSoftObjectPaths` fuzz target to catch panics or incorrect behaviour
  on malformed or adversarial input.

- [ ] **Benchmark suite for the scanner**
  Add `Benchmark*` functions for `groupBySize`, `groupByHeader`, and `hashCandidates`
  to track performance regressions as the codebase evolves.

---

## 🖥️ UX & CLI

- [x] **Short flag aliases**
  Every flag has a single-character alias (`-p`, `-o`, `-w`, `-l`, `-r`) for
  convenient use in shell scripts and terminal workflows.

- [x] **Startup banner with active configuration**
  GoPurge prints the resolved project path, output file, worker count, and large-file
  threshold at startup so users can verify the resolved settings at a glance.

- [ ] **Subcommand structure**
  As the feature set grows, migrate from a flat flag model to a subcommand model
  (e.g. `gopurge scan`, `gopurge diff`, `gopurge cache clear`) using a lightweight
  CLI library or a hand-rolled dispatcher.

- [ ] **`--dry-run` flag**
  Add a flag that runs all stages but skips writing the report file, printing only the
  stdout summary. Useful for a quick sanity check before committing to a full scan.

- [ ] **Coloured terminal output**
  Use ANSI escape codes (with an auto-detect for TTY / `NO_COLOR` env var) to colour
  the summary output — e.g. red for large counts, yellow for `VerifyManually` warnings.

- [ ] **Machine-readable stdout mode**
  Add a `-quiet` flag that suppresses all human-readable stdout output so GoPurge can
  be piped cleanly in shell scripts without needing to redirect stderr separately.

---

## 🔒 Security & Safety

- [x] **Strictly read-only by design**
  GoPurge contains no delete, move, or write operations on project files. The only
  file write is the report output, which goes to a user-specified path outside the
  project tree by default.

- [ ] **Report output path traversal guard**
  Validate that the resolved `-report-path` does not point outside the working directory
  or into the scanned project tree to prevent accidental overwrites.

- [ ] **Checksum the report file after writing**
  Write a `.sha256` sidecar file alongside the report so users (and CI systems) can
  verify the report has not been tampered with or partially written.

---

## 📦 Distribution

- [ ] **GitHub Actions release pipeline**
  Add a `.github/workflows/release.yml` that cross-compiles for `windows/amd64`,
  `darwin/amd64`, `darwin/arm64`, and `linux/amd64` and attaches the binaries to a
  GitHub Release on version tags.

- [ ] **Homebrew formula**
  Publish a Homebrew tap so macOS/Linux users can install GoPurge with
  `brew install <tap>/gopurge`.

- [ ] **Versioning**
  Embed a build-time version string (via `-ldflags "-X main.version=..."`) and expose
  it via a `gopurge -version` flag.
