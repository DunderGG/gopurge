// Package discovery walks an Unreal Engine project's Content/ directory and
// returns a flat list of all asset files found.
//
// Key responsibilities:
//   - Recursively walk Content/ using filepath.WalkDir.
//   - Skip non-asset directories: Intermediate, DerivedDataCache, Binaries, Saved.
//   - Detect and skip symbolic links / NTFS junctions to prevent infinite loops.
//   - Prepend the \\?\ extended-path prefix on Windows for paths exceeding 260 chars.
//   - Collect only files with extensions .uasset and .umap.
//
// The returned []model.FileEntry has Path and Size populated. SHA256 is empty
// and will be filled in later by the scanner package.
package discovery

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"GoPurge/model"
)

// ignoreFolders is the set of directory names that should never be descended into.
var ignoreFolders = map[string]bool{
	"Intermediate":     true,
	"DerivedDataCache": true,
	"Binaries":         true,
	"Saved":            true,
}

// allowedExtensions is the set of file extensions treated as Unreal assets.
var allowedExtensions = map[string]bool{
	".uasset": true,
	".umap":   true,
}

// Walk scans the Content/ subdirectory of projectDir and returns a list of all
// discovered asset files. Warnings about skipped entries (symlinks, access
// errors) are appended to the provided warnings slice.
//
// WalkDir does not follow symbolic links to directories.
// However, in Unreal Engine projects on Windows, developers often use
// NTFS Junctions (a type of symlink) to share content between projects.
// While WalkDir won't recurse into them, it will still report them as a directory entry.
//
// WalkDir only prevents recursion into symlinked directories.
// It will still visit individual file symlinks
// (e.g., a .uasset that points to a file in another folder).
//
// By checking info.Mode() & os.ModeSymlink inside the walk:
// Safety: We guarantee that even if a future version of Go
// or a specific OS implementation changes how WalkDir behaves,
// our tool remains "safe by default" against infinite recursion.
func Walk(projectDir string, warnings *[]string) ([]model.FileEntry, error) {
	// Join projectDir with "Content" to get the path to the Content/ subdirectory.
	contentDir := filepath.Join(projectDir, "Content")
	// FileEntry (defined in model) list to accumulate discovered assets.
	var assets []model.FileEntry

	// WalkDir recursively traverses contentDir. The provided function is called
	// for every file and directory encountered, and can control the traversal by
	// returning filepath.SkipDir to skip descending into a directory.
	err := filepath.WalkDir(contentDir, func(path string, dirEntry fs.DirEntry, err error) error {
		// Handle any error encountered while trying to access the path.
		// This could be due to permission issues, missing files, etc.
		if err != nil {
			// Non-fatal: log and skip the entry.
			*warnings = append(*warnings, fmt.Sprintf("skipped %q: %v", path, err))
			return nil
		}

		// Detect and skip symbolic links / NTFS junctions before descending.
		info, statErr := os.Lstat(path)
		if statErr != nil {
			*warnings = append(*warnings, fmt.Sprintf("lstat failed for %q: %v", path, statErr))
			return nil
		}

		// Skip symlinks to prevent infinite loops and unintended side effects.
		// info.Mode() returns the full bitmask for the file.
		// os.ModeSymlink is a constant where only the "Symlink bit" is set to 1.
		// The & operator compares the two and results in a value where a bit is 1 only if it is 1 in both.
		// So, info.Mode() & os.ModeSymlink effectively "masks" everything except the symlink bit.
		// If that bit is set, the result is non-zero (true); if it's not set, the result is zero (false).
		// It's the standard idiom to check if a specific "flag" is active within a group of flags.
		if info.Mode()&os.ModeSymlink != 0 {
			*warnings = append(*warnings, fmt.Sprintf("skipped symlink: %q", path))

			// If it's a symlink to a directory, skip descending into it.
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip ignored directories.
		if dirEntry.IsDir() {
			if ignoreFolders[dirEntry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip files that don't have allowed asset extensions.
		if !allowedExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}

		// Normalise the path (handle long paths on Windows) and add it to the assets list.
		assets = append(assets, model.FileEntry{
			Path: normalizePath(path),
			Size: info.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk failed: %w", err)
	}

	return assets, nil
}

// normalizePath prepends the Windows extended-path prefix (\\?\) when the path
// exceeds MAX_PATH (260 characters). On non-Windows platforms, it returns the
// path unchanged.
func normalizePath(path string) string {
	return applyMaxPathPrefix(path)
}
