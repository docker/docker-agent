//go:build !windows && !darwin

package acp

// normalizePathForComparison returns the path unchanged on case-sensitive filesystems (Linux).
func normalizePathForComparison(path string) string {
	return path
}
