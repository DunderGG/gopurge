# <img width="64" height="64" alt="appicon" src="https://github.com/user-attachments/assets/5613e671-41d8-4f8f-bae0-b320d777ce7b" /> GoPurge

GoPurge is a high-performance, read-only CLI diagnostic tool written in Go for Unreal Engine developers. It scans your project's `Content/` and `Source/` directories to identify redundant assets, unreferenced files, and large "waste" that bloats Git LFS and slows down backups.

**GoPurge is strictly read-only.** It generates a comprehensive report but never modifies or deletes your files.

## Features

- **Multi-Stage Duplicate Detection:** Efficiently finds byte-for-byte identical assets by comparing sizes, then headers, and finally full SHA-256 hashes using a parallel worker pool.
- **Reference Analysis:** Scans `.uasset`/`.umap` binaries for Soft Object Paths and searches C++ source files for `FSoftObjectPath` to identify assets that are not being used in your project's dependency graph.
- **Large File Scanning:** Quickly flags assets exceeding a configurable size threshold.
- **OS Optimized:** Handles Windows `MAX_PATH` limits (>260 characters) using the `\\?\` prefix and safely skips symbolic links/junctions.
- **Low Memory Footprint:** Streams file contents during hashing to keep RAM usage under ~50MB regardless of project size.
- **Non-Fatal Warnings:** Access-denied errors and skipped symlinks are collected and included in the report rather than aborting the scan.

## Installation

```bash
# Windows
go build -o gopurge.exe .

# macOS / Linux
go build -o gopurge .
```

## Usage

Run GoPurge from your terminal, pointing it to the root of your Unreal Engine project.

```bash
# Windows
gopurge.exe -project-dir="C:/Projects/MyUnrealProject"

# macOS / Linux
./gopurge -project-dir="/home/user/MyUnrealProject"
```

### Command Line Options

Each flag also has a short alias.

| Flag | Short | Description | Default |
| :--- | :--- | :--- | :--- |
| `-project-dir` | `-p` | **(Required)** Path to the Unreal Engine project root. | |
| `-output` | `-o` | Report format: `json` or `csv`. | `json` |
| `-workers` | `-w` | Number of concurrent goroutines for hashing. | `4` |
| `-large-threshold` | `-l` | Size in MB above which a file is flagged as "large". | `100` |
| `-report-path` | `-r` | Custom output path for the report file. | `./gopurge_report.json` |

## How it Works

1. **Pre-flight Checks:** Verifies that a `.uproject` file exists in the target directory, that the `Content/` folder is accessible, and that the Unreal Editor is not currently running (to avoid file locks).
2. **Discovery:** Recursively walks the `Content/` folder, skipping `Intermediate/`, `DerivedDataCache/`, `Saved/`, and `Binaries/`.
3. **Scan:**
   - **Duplicates:** Groups files by size → 1KB header → Full SHA-256 hash (parallel worker pool).
   - **Large Files:** Simple single-pass size-based filter.
   - **References:** Parses binary assets for Soft Object Path strings like `/Game/Path/To/Asset` and scans C++ source for `FSoftObjectPath`.
4. **Report:** Assembles findings into a formatted report, prints a summary of "Total Reclaimable" space, and lists any non-fatal warnings (e.g. skipped symlinks or access-denied files) encountered during the scan.

## Important Note

GoPurge identifies "potential" waste. Certain assets (like DataTables or assets loaded purely via dynamic string paths in C++) may appear unreferenced even if they are used at runtime — these entries are flagged with a `VerifyManually` warning in the report. **Always review the generated report and verify assets in the Unreal Editor before manually deleting them.**

---

*Built with Go. Optimized for Unreal Engine.*
