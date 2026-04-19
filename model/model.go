// Package model defines the shared data types used across all GoPurge packages.
//
// All other packages (discovery, scanner, analyzer, reporter) import model to
// produce or consume these types. model itself imports nothing from GoPurge,
// which guarantees it can never introduce an import cycle.
package model

import "time"

// FileEntry represents a single discovered asset file on disk.
// Populated progressively through the pipeline: Path and Size are set by
// discovery, SHA256 is set only after the scanner hashes the file.
type FileEntry struct {
	// Path is the absolute path to the file.
	// On Windows, paths exceeding 260 characters are prefixed with \\?\ to
	// bypass the MAX_PATH limit.
	Path string

	// Size is the file size in bytes, read from the directory entry.
	Size int64

	// SHA256 is the hex-encoded SHA-256 digest of the full file contents.
	// Empty string until the scanner has processed this entry.
	SHA256 string

	// VerifyManually indicates that this entry may be a false positive —
	// typically a DataTable or config-driven asset that is loaded by string
	// path at runtime and therefore appears unreferenced in a static scan.
	VerifyManually bool
}

// FileGroup is a set of two or more files that are byte-for-byte identical,
// confirmed by matching SHA-256 digests. Only the scanner produces this type.
type FileGroup struct {
	// Hash is the shared SHA-256 digest of every file in the group.
	Hash string

	// Files contains at least two FileEntry values with the same Hash.
	Files []FileEntry
}

// Report is the top-level output structure assembled by main and written to
// disk by the reporter. It captures every category of waste found during a
// single GoPurge run.
type Report struct {
	// GeneratedAt is the UTC timestamp when the report was produced.
	GeneratedAt time.Time

	// ProjectDir is the absolute path to the scanned Unreal Engine project.
	ProjectDir string

	// Duplicates is the list of file groups where every member is an exact
	// byte-for-byte copy of the others. Keeping one copy and deleting the
	// rest would reclaim wasted space.
	Duplicates []FileGroup

	// LargeFiles is the list of individual assets that exceed the configured
	// size threshold (default 100 MB).
	LargeFiles []FileEntry

	// Unreferenced is the list of assets that have zero inbound references
	// in the project's static dependency graph.
	Unreferenced []FileEntry

	// TotalWasteBytes is the sum of reclaimable bytes across all three
	// categories. Duplicate groups count only the space taken by the
	// redundant copies (i.e. group size × (n-1)).
	TotalWasteBytes int64

	// Warnings is a list of non-fatal issues encountered during the scan,
	// such as access-denied errors or skipped symbolic links.
	Warnings []string
}
