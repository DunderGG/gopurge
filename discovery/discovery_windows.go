//go:build windows

package discovery

const maxPath = 260

// applyMaxPathPrefix prepends the \\?\ extended-path prefix on Windows when the
// path length exceeds MAX_PATH (260 characters), bypassing the OS limit.
func applyMaxPathPrefix(path string) string {
	if len(path) > maxPath && len(path) > 4 && path[:4] != `\\?\` {
		return `\\?\` + path
	}
	return path
}
