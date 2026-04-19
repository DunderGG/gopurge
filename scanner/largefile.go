package scanner

import "GoPurge/model"

// filterLargeFiles returns a slice containing only those entries whose Size is
// greater than or equal to thresholdBytes. The original slice is not modified.
// This is a single-pass O(n) filter — no concurrency is needed.
func filterLargeFiles(assets []model.FileEntry, thresholdBytes int64) []model.FileEntry {
	var large []model.FileEntry
	for _, asset := range assets {
		if asset.Size >= thresholdBytes {
			large = append(large, asset)
		}
	}
	return large
}
