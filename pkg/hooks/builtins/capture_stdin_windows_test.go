//go:build windows

package builtins

import "testing"

// captureStdinCommand returns a PowerShell command that writes the hook's
// JSON stdin to path verbatim, produces no stdout, and exits 0. Mirrors the
// POSIX helper in capture_stdin_other_test.go. t.TempDir() paths only contain
// letters, digits and path separators, so the literal path needs no escaping.
func captureStdinCommand(t *testing.T, path string) string {
	t.Helper()
	return "[Console]::In.ReadToEnd() | Set-Content -NoNewline -LiteralPath '" + path + "'"
}
