//go:build !windows

package preflight

import (
	"os/exec"
	"strings"
)

// unrealEditorProcessNames lists the known Unreal Editor executable names on Unix-like systems.
var unrealEditorProcessNames = []string{
	"UnrealEditor",
	"UE4Editor",
}

// checkEditorProcess uses ps to inspect running processes on Unix-like systems.
func checkEditorProcess() error {
	// The "comm" column from ps gives the executable name without arguments.
	// -e lists all processes, and
	// -o comm= outputs only the command name, without headers.
	out, err := exec.Command("ps", "-e", "-o", "comm=").Output()
	if err != nil {
		// If we cannot query the process list, proceed rather than blocking.
		return nil
	}

	// Check if any known Unreal Editor process names are present in the output from ps.
	output := strings.ToLower(string(out))
	for _, name := range unrealEditorProcessNames {
		if strings.Contains(output, strings.ToLower(name)) {
			return ErrEditorRunning
		}
	}
	return nil
}
