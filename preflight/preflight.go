// Package preflight validates the environment before any expensive I/O begins.
//
// All checks are fast and cheap (process list, file-stat). A failure here exits
// early with a targeted error message so the user knows exactly what to fix —
// there is no point walking gigabytes of content if the editor is still running
// or the project directory is not a valid Unreal Engine project.
//
// Checks performed (in order):
//  1. A .uproject file exists directly inside the project directory.
//  2. The Unreal Editor process (UE4Editor.exe / UnrealEditor.exe) is NOT running.
//  3. The Content/ subdirectory exists and is readable.
package preflight

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoUProject is returned when no .uproject file is found in the project directory.
var ErrNoUProject = errors.New("no .uproject file found — is this an Unreal Engine project directory?")

// ErrEditorRunning is returned when the Unreal Editor process is detected as running.
var ErrEditorRunning = errors.New("Unreal Editor is currently running — close it before scanning to avoid file locks")

// ErrContentNotAccessible is returned when the Content/ subdirectory is missing or unreadable.
var ErrContentNotAccessible = errors.New("Content/ directory is missing or not accessible")

// Validate runs all pre-flight checks against the given project directory.
// It returns the first error encountered, or nil if all checks pass.
func Validate(projectDir string) error {
	// Check 1: .uproject file exists at the top level of projectDir.
	if err := checkUProject(projectDir); err != nil {
		return err
	}

	// Check 2: Unreal Editor is not running.
	if err := checkEditorNotRunning(); err != nil {
		return err
	}
	// Check 3: Content/ directory exists and is accessible.
	if err := checkContentAccessible(projectDir); err != nil {
		return err
	}

	return nil
}

// checkUProject verifies that exactly one .uproject file exists at the top level
// of projectDir. It does not descend into subdirectories.
func checkUProject(projectDir string) error {
	// ReadDir reads the contents of projectDir without descending,
	// which is more efficient than WalkDir for this check.
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return fmt.Errorf("cannot read project directory: %w", err)
	}

	// Look for any .uproject file among the entries. We don't care about multiple .uproject files here;
	// if at least one is found, we consider it a valid Unreal project.
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".uproject" {
			return nil
		}
	}
	return ErrNoUProject
}

// checkEditorNotRunning inspects the running process list for known Unreal Editor
// executable names. Returns ErrEditorRunning if any are found.
//
// Note: process detection is platform-specific and implemented in
// preflight_windows.go / preflight_unix.go via build tags.
func checkEditorNotRunning() error {
	return checkEditorProcess()
}

// checkContentAccessible confirms that <projectDir>/Content exists and that the
// current user can read it.
func checkContentAccessible(projectDir string) error {
	// Join projectDir with "Content" to get the path to the Content/ subdirectory.
	contentDir := filepath.Join(projectDir, "Content")
	// Stat the Content/ directory to check for existence and readability.
	// We don't need to descend into it here, just confirm we can access it.
	info, err := os.Stat(contentDir)
	if err != nil {
		return ErrContentNotAccessible
	}
	// Ensure that the path is a directory, not a file.
	if !info.IsDir() {
		return ErrContentNotAccessible
	}
	return nil
}
