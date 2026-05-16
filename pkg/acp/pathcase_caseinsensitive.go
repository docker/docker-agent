//go:build windows || darwin

package acp

import "strings"

// normalizePathForComparison lowercases the path on case-insensitive filesystems
// (macOS and Windows) to ensure path traversal checks cannot be bypassed by
// varying the case of path components.
func normalizePathForComparison(path string) string {
	return strings.ToLower(path)
}
