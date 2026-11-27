//go:build !windows

package builtins

import "testing"

// captureStdinCommand returns a POSIX shell command that writes the hook's
// JSON stdin to path verbatim, produces no stdout, and exits 0. Mirrored for
// PowerShell in capture_stdin_windows_test.go. t.TempDir() paths only contain
// letters, digits and path separators, so single quoting needs no escaping.
func captureStdinCommand(t *testing.T, path string) string {
	t.Helper()
	return "cat > '" + path + "'"
}
