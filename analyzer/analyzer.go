// Package analyzer performs a static reference analysis on the discovered
// asset list to identify files that have zero inbound references.
//
// Strategy:
//  1. Treat the full asset list returned by discovery as the "known universe".
//  2. For each .uasset and .umap file, parse the Package File Summary header
//     to locate the Name Map — an uncompressed table of all string identifiers
//     used by the package — and extract /Game/... paths directly from it.
//     This avoids false matches that can occur when /Game/ byte sequences
//     appear inside compressed or encrypted payload sections. If the structured
//     parse fails (e.g. non-standard or third-party binary), the file falls
//     back to a full-binary raw scan and a warning is recorded.
//  3. Non-binary assets (config files, source, etc.) are always processed with
//     the raw scan.
//  4. Walk the Source/ directory and apply a regular expression to find
//     FSoftObjectPath references in C++ source files.
//  5. Any asset in the known universe that appears in neither scan is flagged
//     as unreferenced.
//
// False-positive risk:
// DataTables, PrimaryAssetLabels, and other config-driven assets are loaded at
// runtime via string paths and will appear unreferenced in a static scan. These
// entries are annotated with VerifyManually = true in the returned list.
package analyzer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"GoPurge/model"
)

// softObjectPathPrefix is the byte prefix used by Unreal's Soft Object Path
// system when embedding asset references in binary .uasset files.
const softObjectPathPrefix = "/Game/"

// fSoftObjectPathRegex matches C++ FSoftObjectPath constructor / assignment
// patterns in Source/ files.
var fSoftObjectPathRegex = regexp.MustCompile(`FSoftObjectPath\s*\(\s*TEXT\s*\(\s*"([^"]+)"`)

// knownFalsePositiveExtensions lists file types that are commonly loaded at
// runtime by string path and are therefore prone to false-positive detection.
var knownFalsePositiveExtensions = map[string]bool{
	// DataTables, CurveTable, etc. are often referenced only from config.
	".uasset": false, // not all .uasset files are false positives; checked by name below
}

// AnalyzeReferences cross-references the full asset list against all inbound
// references found in binary assets and C++ source files. It returns the subset
// of assets that appear to have zero inbound references.
//
// The projectDir is used to locate the Source/ directory alongside Content/.
// Non-fatal errors (unreadable files) are appended to warnings.
func AnalyzeReferences(projectDir string, assets []model.FileEntry, workers int, warnings *[]string) ([]model.FileEntry, error) {
	// Build a map of referenced asset paths discovered in binaries and source files.
	// The keys are in the /Game/... format used by Unreal's Soft Object Path system.
	referenced := make(map[string]bool)

	// Pass 1: scan all asset binaries for embedded Soft Object Paths, 
	// using the given number of worker goroutines.
	if err := scanAssetBinaries(assets, workers, referenced, warnings); err != nil {
		return nil, err
	}

	// Pass 2: scan C++ source files for FSoftObjectPath references.
	sourceDir := filepath.Join(projectDir, "Source")
	if _, err := os.Stat(sourceDir); err == nil {
		if err := scanSourceFiles(sourceDir, referenced, warnings); err != nil {
			return nil, err
		}
	}

	// Determine which assets from the known universe are not referenced.
	var unreferenced []model.FileEntry
	for _, asset := range assets {
		// Normalise the path to the /Game/... key format for lookup.
		key := toGamePath(asset.Path)
		// If the key is not in the referenced map, this asset has zero references in the project.
		if !referenced[key] {
			entry := asset
			entry.VerifyManually = isLikelyFalsePositive(asset.Path)
			unreferenced = append(unreferenced, entry)
		}
	}
	return unreferenced, nil
}

// scanResult carries the outcome of scanning a single asset file.
// It is passed from a worker goroutine back to the collector via the results
// channel. Bundling both the paths and any warning into one struct means the
// worker never has to touch the shared referenced map or warnings slice —
// those are only written by the collector goroutine, which eliminates the need
// for a mutex.
type scanResult struct {
	// paths holds every /Game/... path found in the file.
	// It is nil (not just empty) when the file could not be read at all.
	paths []string

	// warning is non-empty when a non-fatal issue occurred, such as a read
	// error or a structured parse failure that triggered a raw scan fallback.
	warning string
}

// scanAssetBinaries scans each asset file for /Game/... references and adds
// them to the referenced map. It uses a fan-out/fan-in worker pool so that
// files are read and parsed concurrently across N goroutines.
//
// Concurrency model (mirrors the SHA-256 hashing pool in scanner/duplicates.go):
//
//	Producer goroutine         → jobs channel (buffered, cap = workers)
//	N worker goroutines        → each reads one job, scans the file, sends a scanResult
//	wg-closer goroutine        → closes results channel once all workers finish
//	Main goroutine (collector) → merges every scanResult into referenced and warnings
//
// The collector is the only writer to referenced and warnings, so no mutex is needed.
func scanAssetBinaries(assets []model.FileEntry, workers int, referenced map[string]bool, warnings *[]string) error {
	// jobs carries FileEntry values to the worker goroutines.
	// results carries the scanResult values back to the collector on the main goroutine.
	// Both channels are buffered so that producers and consumers don't have to
	// synchronise on every single item — they can each work at their own pace
	// up to the buffer capacity.
	jobs := make(chan model.FileEntry, workers)
	results := make(chan scanResult, workers)

	// Fan-out: start N worker goroutines.
	// Each goroutine waits for a FileEntry on jobs, calls scanSingleAsset to do
	// the actual file I/O and parsing, then sends the result to results.
	// sync.WaitGroup lets us track when every worker has finished so we know
	// it is safe to close the results channel.
	var wg sync.WaitGroup
	for workerIndex := 0; workerIndex < workers; workerIndex++ {
		wg.Add(1)
		go func() {
			// Tell the WaitGroup this worker has finished when the goroutine returns.
			defer wg.Done()

			// "range over a channel" reads values until the channel is closed.
			// When close(jobs) is called by the producer, all workers will
			// finish their current job and then exit this loop automatically.
			for asset := range jobs {
				results <- scanSingleAsset(asset)
			}
		}()
	}

	// wg-closer: once every worker has called wg.Done(), close the results channel.
	// Closing results signals the collector loop below that there are no more
	// results to receive. We run this in its own goroutine because wg.Wait()
	// is a blocking call — it would stall the main goroutine if called directly.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Producer: send every asset into the jobs channel, then close it.
	// This runs in its own goroutine so the main goroutine is free to collect
	// results at the same time. Without this goroutine, the main goroutine would
	// block here trying to send new jobs while the (also buffered) results
	// channel fills up — a classic channel deadlock.
	go func() {
		for _, asset := range assets {
			jobs <- asset
		}
		// Closing jobs tells workers' range loops to stop waiting for more work.
		close(jobs)
	}()

	// Collector: receive every scanResult from the results channel.
	// "range over a channel" blocks until a new result arrives, processes it,
	// then waits for the next — repeating until the wg-closer calls close(results).
	// Only this goroutine (the main goroutine) writes to referenced and warnings,
	// so no mutex is required.
	for result := range results {
		if result.warning != "" {
			*warnings = append(*warnings, result.warning)
		}
		for _, path := range result.paths {
			referenced[path] = true
		}
	}

	return nil
}

// scanSingleAsset reads one asset file and returns all /Game/... paths found
// in it, along with any non-fatal warning string. It is designed to be called
// from a worker goroutine — it only reads from the filesystem and returns a
// value; it never touches any shared state, so it is safe to call concurrently.
func scanSingleAsset(asset model.FileEntry) scanResult {
	data, err := os.ReadFile(asset.Path)
	if err != nil {
		// Record a warning and return an empty result. The file simply will not
		// contribute any references, which is the safest behaviour on a read error.
		return scanResult{warning: fmt.Sprintf("analyzer: skipped reading %q: %v", asset.Path, err)}
	}

	ext := strings.ToLower(filepath.Ext(asset.Path))
	if ext == ".uasset" || ext == ".umap" {
		paths, parseErr := parseUAssetImports(data)
		if parseErr == nil {
			// Structured parse succeeded — return the Name Map paths directly.
			return scanResult{paths: paths}
		}

		// Structured parse failed; fall back to a raw byte scan and record a
		// warning so the user knows which files could not be parsed properly.
		return scanResult{
			paths: extractSoftObjectPaths(string(data)),
			warning: fmt.Sprintf(
				"analyzer: UAsset header parse failed for %q (%v), falling back to raw scan",
				asset.Path, parseErr,
			),
		}
	}

	// For all other file types (config, non-standard assets, etc.), always use
	// the raw scan — there is no structured header to parse.
	return scanResult{paths: extractSoftObjectPaths(string(data))}
}

// extractSoftObjectPaths finds all /Game/... substrings in content and returns
// them as a slice of strings. Each entry is a complete Unreal asset path
// (e.g. "/Game/Characters/Alice").
//
// Returning a slice instead of writing directly to a map makes this function
// safe to call from multiple goroutines simultaneously — it only reads its
// argument and writes to a local variable, never touching shared state.
func extractSoftObjectPaths(content string) []string {
	var paths []string
	index := 0
	for {
		// Look for the next occurrence of the softObjectPathPrefix starting from index.
		pos := strings.Index(content[index:], softObjectPathPrefix)

		// If no more occurrences are found, break the loop.
		if pos == -1 {
			break
		}

		// Calculate the absolute position of the found prefix in the content string.
		start := index + pos
		end := start + len(softObjectPathPrefix)

		// Advance until a non-path character is found.
		for end < len(content) && isPathChar(content[end]) {
			end++
		}

		// Add the extracted path to the result slice.
		paths = append(paths, content[start:end])
		index = end
	}
	return paths
}

// The isPathChar() function is necessary because Unreal Engine's .uasset and .umap files are binary, not plain text.
// When we search for asset references like /Game/Characters/Alice, we can't just use a simple "split by space" or "read line" 
// because the reference is embedded in a stream of binary data (0x00, 0x01, and control characters).

// 1. Determining the "End" of a Path
//    In a binary file, an asset string isn't always followed by a space or a newline. 
//    It might be immediately followed by a null terminator (0x00), a length prefix for the next variable, or binary metadata.
//    isPathChar allows us to "crawl" forward from the /Game/ prefix and stop the moment we hit a byte that cannot possibly 
//    be part of an Unreal path (like a null byte, a bracket, or a random binary character).

// 2. Matching Unreal's Asset Naming Rules
//    Unreal Engine has strict rules for what characters can be in a folder or asset name. 
//    By restricting the scan to a-z, A-Z, 0-9, /, _, -, and ., we:
//       Prevent False Matches: 
//          If the scanner sees /Game/ followed by random binary garbage, 
//          isPathChar will fail immediately, and we won't try to cross-reference a "broken" path.
//       Performance: 
//          It's much faster to check a single byte against a few ranges than to run 
//          a complex Regular Expression over a massive 500MB binary file.

// 3. Why it's "Hardcoded"
//    The character set for paths in Unreal hasn't changed in over a decade. 
//    While it feels specific, it is actually a performance optimization. 
//    By hardcoding the valid characters, we ensure the analyzer loop is as tight and fast as possible 
//    while remaining robust against the "noise" of a binary file.

// Summary
//    If we didn't have isPathChar, we wouldn't know where a path like /Game/Maps/Level1 ends and the binary file metadata begins. 
//    It acts as our terminator detector in a world without line endings.

//    isPathChar returns true for characters that are valid inside an Unreal asset path.
func isPathChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '/' || c == '_' || c == '-' || c == '.'
}

// scanSourceFiles walks the Source/ directory and applies fSoftObjectPathRegex to
// every .cpp and .h file, adding matched paths to the referenced map.
func scanSourceFiles(sourceDir string, referenced map[string]bool, warnings *[]string) error {
	return filepath.WalkDir(sourceDir, func(path string, dirEntry fs.DirEntry, err error) error {
		// If there is an error accessing the file, log a warning and skip it.
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("analyzer: walk error at %q: %v", path, err))
			return nil
		}

		// Skip directories.
		if dirEntry.IsDir() {
			return nil
		}

		// Only scan .cpp and .h files to avoid unnecessary work.
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".cpp" && ext != ".h" {
			return nil
		}

		// Read the file content. If it fails, log a warning and skip it.
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			*warnings = append(*warnings, fmt.Sprintf("analyzer: skipped source %q: %v", path, readErr))
			return nil
		}

		// Apply the regex to find all FSoftObjectPath references in the file.
		for _, match := range fSoftObjectPathRegex.FindAllStringSubmatch(string(data), -1) {
			// match[1] contains the captured path inside the TEXT("...") part of the FSoftObjectPath constructor.
			if len(match) > 1 {
				referenced[match[1]] = true
			}
		}
		return nil
	})
}

// toGamePath converts an absolute filesystem path to the /Game/... key format
// used by Unreal's Soft Object Path system.
func toGamePath(absPath string) string {
	// Normalise separators.
	path := filepath.ToSlash(absPath)

	// Find the Content/ segment and replace with /Game/.
	if index := strings.Index(path, "/Content/"); index != -1 {
		// Extract the relative path out of the absolute path.
		relativePath := path[index+len("/Content/"):]
		
		// Strip extension (.uasset / .umap).
		relativePath = strings.TrimSuffix(relativePath, filepath.Ext(relativePath))

		return softObjectPathPrefix + relativePath
	}
	return path
}

// isLikelyFalsePositive returns true for assets that are commonly loaded at
// runtime by string path and therefore prone to appearing unreferenced in a
// static scan.
func isLikelyFalsePositive(path string) bool {
	lower := strings.ToLower(path)
	
	// Check for known false-positive extensions first.
	falsePositiveKeywords := []string{
		"datatable", "curvetable", "primaryassetlabel", "gamedata",
	}

	for _, keyword := range falsePositiveKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}
