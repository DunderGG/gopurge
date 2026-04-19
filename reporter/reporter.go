// Package reporter writes the completed scan results to disk and prints a
// human-readable summary to stdout.
//
// Supported output formats:
//   - HTML (default): self-contained dashboard with SVG charts and sortable tables.
//   - JSON: encoding/json with MarshalIndent for human readability.
//   - CSV: one row per flagged file with columns Category, Path, SizeBytes,
//     SHA256, VerifyManually, Notes.
//
// The reporter never deletes any files. Its sole responsibility is to persist
// the report and present the summary so the user can make informed decisions
// inside the Unreal Editor.
package reporter

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"GoPurge/model"
)

const (
	// FormatJSON is the identifier for JSON output.
	FormatJSON = "json"
	// FormatCSV is the identifier for CSV output.
	FormatCSV = "csv"
)

// Write persists the report to outputPath in the specified format (html, json,
// or csv) and prints a one-page summary to stdout.
func Write(report model.Report, outputPath, format string) error {
	// Normalise all paths to forward slashes so they are copy-paste friendly
	// in the report file. Windows File Explorer, PowerShell, and the Unreal
	// Editor all accept forward slashes, and they require no escaping in JSON.
	report = normalizeReportPaths(report)

	switch format {
	case FormatHTML:
		if err := writeHTML(report, outputPath); err != nil {
			return err
		}
	case FormatCSV:
		if err := writeCSV(report, outputPath); err != nil {
			return err
		}
	case FormatJSON:
		if err := writeJSON(report, outputPath); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown report format %q: use html, json, or csv", format)
	}
	printSummary(report, outputPath)
	return nil
}

// writeJSON serialises the report to the given path using indented JSON.
func writeJSON(report model.Report, path string) error {
	// MarshalIndent produces human-readable JSON with indentation and newlines.
	// It uses reflection to look at our struct at runtime and sees every field.
	// As long as our struct fields are exported (capitalised), they will be included in the output.
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

// writeCSV serialises the report to the given path as a flat CSV file. Each
// flagged file produces one row regardless of category.
func writeCSV(report model.Report, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Header row.
	if err := writer.Write([]string{"Category", "Path", "SizeBytes", "SHA256", "VerifyManually", "Notes"}); err != nil {
		return err
	}

	// Duplicate entries — every file in every group.
	for _, group := range report.Duplicates {
		for _, entry := range group.Files {
			// Make a slice of strings we want to write as a row in the CSV. 
			// The order of these fields should match the header row we wrote above.
			if err := writer.Write([]string{
				"Duplicate",
				entry.Path,
				strconv.FormatInt(entry.Size, 10),
				entry.SHA256,
				strconv.FormatBool(entry.VerifyManually),
				fmt.Sprintf("duplicate group SHA256=%s", group.Hash),
			}); err != nil {
				return err
			}
		}
	}

	// Large file entries.
	for _, entry := range report.LargeFiles {
		if err := writer.Write([]string{
			"Large",
			entry.Path,
			strconv.FormatInt(entry.Size, 10),
			entry.SHA256,
			strconv.FormatBool(entry.VerifyManually),
			"",
		}); err != nil {
			return err
		}
	}

	// Unreferenced entries.
	for _, entry := range report.Unreferenced {
		notes := ""
		if entry.VerifyManually {
			notes = "possible false positive — verify in Unreal Editor before deleting"
		}
		if err := writer.Write([]string{
			"Unreferenced",
			entry.Path,
			strconv.FormatInt(entry.Size, 10),
			entry.SHA256,
			strconv.FormatBool(entry.VerifyManually),
			notes,
		}); err != nil {
			return err
		}
	}

	return writer.Error()
}

// printSummary writes a concise one-page summary of the scan to stdout.
func printSummary(report model.Report, outputPath string) {
	reclaimableGB := float64(report.TotalWasteBytes) / (1024 * 1024 * 1024)

	fmt.Println()
	fmt.Println("GoPurge scan complete")
	fmt.Println("─────────────────────────────────────────────────────────────")
	fmt.Printf("  Duplicates:    %d files in %d groups\n", countDuplicateFiles(report.Duplicates), len(report.Duplicates))
	fmt.Printf("  Large files:   %d files\n", len(report.LargeFiles))
	fmt.Printf("  Unreferenced:  %d files\n", len(report.Unreferenced))
	fmt.Println("─────────────────────────────────────────────────────────────")
	fmt.Printf("  Total reclaimable: ~%.2f GB\n", reclaimableGB)
	absPath, err := filepath.Abs(outputPath)
	if err != nil {
		absPath = outputPath
	}
	fmt.Printf("  Report written to: %s\n", absPath)
	fmt.Println()
	fmt.Println("  ⚠ Never delete automatically.")
	fmt.Println("    Review the report and verify each entry in Unreal Editor")
	fmt.Println("    before removing any files.")
	fmt.Println()

	if len(report.Warnings) > 0 {
		fmt.Printf("  ⚠ %d warning(s) during scan — see report for details.\n", len(report.Warnings))
		fmt.Println()
	}
}

// countDuplicateFiles returns the total number of individual files across all
// duplicate groups.
func countDuplicateFiles(groups []model.FileGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.Files)
	}
	return total
}

// normalizeReportPaths returns a shallow copy of report with all file paths
// converted to forward slashes. This makes paths copy-paste friendly in the
// report file — Windows File Explorer, PowerShell, and the Unreal Editor all
// accept forward slashes, and they require no escaping in JSON (unlike `\`).
func normalizeReportPaths(report model.Report) model.Report {
	report.ProjectDir = toForwardSlash(report.ProjectDir)

	for i := range report.Duplicates {
		for j := range report.Duplicates[i].Files {
			report.Duplicates[i].Files[j].Path = toForwardSlash(report.Duplicates[i].Files[j].Path)
		}
	}
	for i := range report.LargeFiles {
		report.LargeFiles[i].Path = toForwardSlash(report.LargeFiles[i].Path)
	}
	for i := range report.Unreferenced {
		report.Unreferenced[i].Path = toForwardSlash(report.Unreferenced[i].Path)
	}
	return report
}

// toForwardSlash converts a path to forward slashes and strips any \\?\ 
// extended-path prefix that was added for internal scanning purposes.
func toForwardSlash(path string) string {
	// Strip the Windows extended-path prefix before writing to the report.
	path = strings.TrimPrefix(path, `\\?\`)
	return filepath.ToSlash(path)
}
