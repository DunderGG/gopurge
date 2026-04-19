//go:build !windows

package discovery

// applyMaxPathPrefix is a no-op on non-Windows platforms.
func applyMaxPathPrefix(path string) string {
	return path
}
