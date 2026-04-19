// GoPurge scans an Unreal Engine project directory to identify
// duplicate assets, unreferenced files, and large "waste" files.
//
// It helps developers maintain a lean project for faster backups and
// Git LFS management.
//
// Usage:
//
//	gopurge -project-dir=<path> [-output=html|json|csv] [-workers=N] [-large-threshold=100]
//
// Flags:
//
//	-project-dir, p      Path to the root of the Unreal Engine project (required).
//	-output, o           Report format: "html" (default), "json", or "csv".
//	-workers, w          Number of goroutines used for SHA-256 hashing (default 4).
//	-large-threshold, l  Size in MB above which a file is considered "large" (default 100).
//	-report-path, r      Output path for the report file (default: gopurge_report.<ext>).
//
// GoPurge is read-only — it never modifies or deletes any files.
// Always run it while the Unreal Editor is closed.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"GoPurge/analyzer"
	"GoPurge/discovery"
	"GoPurge/model"
	"GoPurge/preflight"
	"GoPurge/reporter"
	"GoPurge/scanner"
)

func main() {
	// ── 0. CLI flags ───────────────────────────────────────────────────────
	var (
		projectDir       string
		outputFormat     string
		workers          int
		largeThresholdMB int64
		reportPath       string
	)
	flag.StringVar(&projectDir, "project-dir", "", "Path to the Unreal Engine project root (required)")
	flag.StringVar(&projectDir, "p", "", "Path to the Unreal Engine project root (required)")
	flag.StringVar(&outputFormat, "output", reporter.FormatHTML, `Report format: "html" (default), "json", or "csv"`)
	flag.StringVar(&outputFormat, "o", reporter.FormatHTML, `Report format: "html" (default), "json", or "csv"`)
	flag.IntVar(&workers, "workers", 4, "Number of goroutines for SHA-256 hashing")
	flag.IntVar(&workers, "w", 4, "Number of goroutines for SHA-256 hashing")
	flag.Int64Var(&largeThresholdMB, "large-threshold", 100, "File size in MB above which a file is flagged as large")
	flag.Int64Var(&largeThresholdMB, "l", 100, "File size in MB above which a file is flagged as large")
	flag.StringVar(&reportPath, "report-path", "", `Output path for the report file (default: gopurge_report.<ext>)`)
	flag.StringVar(&reportPath, "r", "", `Output path for the report file (default: gopurge_report.<ext>)`)
	flag.Parse()

	if projectDir == "" {
		fmt.Fprintln(os.Stderr, "error: -project-dir is required")
		flag.Usage()
		os.Exit(1)
	}

	// Resolve default report path if not specified.
	if reportPath == "" {
		ext := outputFormat
		switch ext {
		case reporter.FormatJSON, reporter.FormatCSV:
			// use ext as-is
		default:
			ext = reporter.FormatHTML
		}
		reportPath = filepath.Join(".", "gopurge_report."+ext)
	}

	largeThresholdBytes := largeThresholdMB * 1024 * 1024

	// SetFlags(0) to disable timestamps and other prefixes in log output and
	// take "full control", since all warnings are collected in the report and
	// printed in a summary at the end.
	log.SetFlags(0)
	log.SetPrefix("gopurge: ")

	fmt.Println("🧹 GoPurge is ready to scan...")
	fmt.Printf("   Project:   %s\n", projectDir)
	fmt.Printf("   Output:    %s (%s)\n", reportPath, outputFormat)
	fmt.Printf("   Workers:   %d\n", workers)
	fmt.Printf("   Large ≥:   %d MB\n\n", largeThresholdMB)

	// ── 1. Pre-flight validation ───────────────────────────────────────────
	fmt.Println("→ Running pre-flight checks...")
	if err := preflight.Validate(projectDir); err != nil {
		log.Fatalf("pre-flight failed: %v", err)
	}
	fmt.Println("  ✓ Pre-flight checks passed.")

	// ── 2. Project discovery ───────────────────────────────────────────────
	//	Throughout the code, we use "assets" as the full list of discovered files,
	//  which is treated as the "known universe" for reference analysis.
	fmt.Println("→ Discovering assets...")
	var warnings []string
	assets, err := discovery.Walk(projectDir, &warnings)
	if err != nil {
		log.Fatalf("discovery failed: %v", err)
	}
	if len(assets) == 0 {
		fmt.Println("  No assets found — nothing to do.")
		os.Exit(0)
	}
	fmt.Printf("  ✓ Found %d assets.\n", len(assets))

	// ── 3. Duplicate detection ─────────────────────────────────────────────
	fmt.Println("→ Scanning for duplicates...")
	duplicates, err := scanner.ScanForDuplicates(assets, workers, &warnings)
	if err != nil {
		log.Fatalf("duplicate scan failed: %v", err)
	}
	fmt.Printf("  ✓ Found %d duplicate group(s).\n", len(duplicates))

	// ── 4. Large file detection ────────────────────────────────────────────
	fmt.Println("→ Scanning for large files...")
	largeFiles := scanner.FindLargeFiles(assets, largeThresholdBytes)
	fmt.Printf("  ✓ Found %d large file(s) (≥ %d MB).\n", len(largeFiles), largeThresholdMB)

	// ── 5. Reference analysis ──────────────────────────────────────────────
	fmt.Println("→ Analysing asset references...")
	unreferenced, err := analyzer.AnalyzeReferences(projectDir, assets, &warnings)
	if err != nil {
		log.Fatalf("reference analysis failed: %v", err)
	}
	fmt.Printf("  ✓ Found %d unreferenced asset(s).\n", len(unreferenced))

	// ── 6. Assemble report ─────────────────────────────────────────────────
	report := model.Report{
		GeneratedAt:     time.Now().UTC(),
		ProjectDir:      projectDir,
		Duplicates:      duplicates,
		LargeFiles:      largeFiles,
		Unreferenced:    unreferenced,
		TotalWasteBytes: computeTotalWaste(duplicates, largeFiles, unreferenced),
		Warnings:        warnings,
	}

	// ── 7. Write report ────────────────────────────────────────────────────
	fmt.Println("→ Writing report...")
	if err := reporter.Write(report, reportPath, outputFormat); err != nil {
		log.Fatalf("report generation failed: %v", err)
	}
}

// computeTotalWaste estimates the number of reclaimable bytes across all three
// waste categories. For duplicate groups, only the redundant copies are counted
// (i.e. group size × (n-1) copies). Large files and unreferenced assets are
// counted in full, but de-duplicated against each other to avoid double-counting.
func computeTotalWaste(duplicates []model.FileGroup, largeFiles, unreferenced []model.FileEntry) int64 {
	// counted keeps track of file paths we've already included in the total to avoid double-counting.
	counted := make(map[string]bool)
	var total int64

	for _, group := range duplicates {
		// Skip groups with a single file, as they don't represent any waste.
		if len(group.Files) < 2 {
			continue
		}

		// For a group of n identical files, only (n-1) of them are redundant.
		redundantCopies := int64(len(group.Files) - 1)

		// The size of each file in the group is the same, so we can take the size of the first file.
		total += group.Files[0].Size * redundantCopies
		
		// Mark all files in this group as counted to avoid double-counting if they also appear in largeFiles or unreferenced.
		for _, file := range group.Files {
			counted[file.Path] = true
		}
	}

	for _, file := range largeFiles {
		// Skip files that have already been counted in duplicate groups.
		if counted[file.Path] {
			continue
		}
		total += file.Size
		counted[file.Path] = true
	}

	for _, file := range unreferenced {
		// Skip files that have already been counted in duplicate groups or large files.
		if counted[file.Path] {
			continue
		}
		total += file.Size
		counted[file.Path] = true
	}

	return total
}
