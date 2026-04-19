// Package scanner identifies duplicate and oversized assets within the
// discovered file list.
//
// It exposes two public functions that main calls in sequence:
//   - ScanForDuplicates — finds groups of byte-for-byte identical files.
//   - FindLargeFiles    — finds files exceeding a configurable size threshold.
//
// Both functions accept a warnings slice so that non-fatal errors (e.g. access
// denied during hashing) are surfaced to the caller without aborting the scan.
package scanner

import "GoPurge/model"

// ScanForDuplicates identifies groups of two or more files that are
// byte-for-byte identical. Detection is staged to minimise disk I/O:
//
//  1. Group by file size — files with a unique size cannot be duplicates.
//  2. Group by 1 KB header — files with a unique header within a size group are
//     eliminated cheaply before full hashing.
//  3. Full SHA-256 — surviving candidates are hashed in parallel by a worker
//     pool whose size is controlled by the workers parameter.
//
//  Only entries with identical sizes and headers are hashed, and
//  only groups of two or more identical hashes are returned as duplicates.
//
// Access-denied and other per-file errors are non-fatal: the affected file is
// skipped and a warning is appended to warnings.
func ScanForDuplicates(assets []model.FileEntry, workers int, warnings *[]string) ([]model.FileGroup, error) {
	// Stage A: group by size.
	bySizeCandidates := groupBySize(assets)

	// Stage B: group by 1 KB header, narrowing candidates further.
	byHeaderCandidates := groupByHeader(bySizeCandidates, warnings)

	// Stage C: full SHA-256 via worker pool, produce confirmed duplicate groups.
	groups, err := hashCandidates(byHeaderCandidates, workers, warnings)

	if err != nil {
		return nil, err
	}

	return groups, nil
}

// FindLargeFiles returns all entries whose size is greater than or equal to
// thresholdBytes. This is a single-pass filter and requires no concurrency.
func FindLargeFiles(assets []model.FileEntry, thresholdBytes int64) []model.FileEntry {
	return filterLargeFiles(assets, thresholdBytes)
}
