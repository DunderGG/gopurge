//go:build windows

package preflight

import (
	"os/exec"
	"strings"
)

// unrealEditorProcessNames lists the known Unreal Editor executable names on Windows.
var unrealEditorProcessNames = []string{
	"UnrealEditor.exe",
	"UE4Editor.exe",
}

// checkEditorProcess uses tasklist to inspect running processes on Windows.
func checkEditorProcess() error {
	// tasklist with /FO CSV and /NH outputs a simple CSV list of process names without headers, which is easier to parse.
	out, err := exec.Command("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		// If we cannot query the process list, proceed with a warning rather
		// than blocking the scan entirely.
		return nil
	}

	// Check if any known Unreal Editor process names are present in the output from tasklist.
	output := strings.ToLower(string(out))
	for _, name := range unrealEditorProcessNames {
		if strings.Contains(output, strings.ToLower(name)) {
			return ErrEditorRunning
		}
	}
	return nil
}
