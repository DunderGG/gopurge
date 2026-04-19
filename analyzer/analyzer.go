// Package analyzer performs a static reference analysis on the discovered
// asset list to identify files that have zero inbound references.
//
// Strategy:
//  1. Treat the full asset list returned by discovery as the "known universe".
//  2. For each .uasset and .umap file, scan the binary content for byte
//     sequences matching Unreal's Soft Object Path format: /Game/<path>.
//  3. Walk the Source/ directory and apply a regular expression to find
//     FSoftObjectPath references in C++ source files.
//  4. Any asset in the known universe that appears in neither scan is flagged
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
func AnalyzeReferences(projectDir string, assets []model.FileEntry, warnings *[]string) ([]model.FileEntry, error) {
	// Build a map of referenced asset paths discovered in binaries and source files.
	// The keys are in the /Game/... format used by Unreal's Soft Object Path system.
	referenced := make(map[string]bool)

	// Pass 1: scan all asset binaries for embedded Soft Object Paths.
	if err := scanAssetBinaries(assets, referenced, warnings); err != nil {
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

// scanAssetBinaries reads each asset file and collects all /Game/... substrings
// it contains, adding them to the referenced map.
func scanAssetBinaries(assets []model.FileEntry, referenced map[string]bool, warnings *[]string) error {
	for _, asset := range assets {
		data, err := os.ReadFile(asset.Path)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("analyzer: skipped reading %q: %v", asset.Path, err))
			continue
		}
		// data is a byte slice, but extractSoftObjectPaths expects a string. We can convert it directly.
		extractSoftObjectPaths(string(data), referenced)
	}
	return nil
}

// extractSoftObjectPaths finds all /Game/... substrings in content and marks
// them as referenced.
func extractSoftObjectPaths(content string, referenced map[string]bool) {
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

		// Mark the extracted path as referenced. The key is the substring from start to end.
		referenced[content[start:end]] = true
		index = end
	}
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
