//go:build !windows

package hooks

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Command hooks run under $SHELL (or /bin/sh) on non-Windows platforms.
// These helpers generate the POSIX commands mirrored for PowerShell in
// helpers_windows_test.go so the tests validate the same hook contracts
// on every platform.

// emitContextEnvPwdCmd returns a command printing the hook JSON envelope
// whose additional_context is the values of envVars plus the working
// directory, joined with ":".
func emitContextEnvPwdCmd(envVars ...string) string {
	args := make([]string, 0, len(envVars)+1)
	for _, name := range envVars {
		args = append(args, `"$`+name+`"`)
	}
	args = append(args, `"$(pwd)"`)
	format := strings.TrimSuffix(strings.Repeat("%s:", len(args)), ":")
	return `printf '{"hook_specific_output":{"additional_context":"` + format + `"}}' ` + strings.Join(args, " ")
}

// printStdinJSONFieldCmd returns a command printing one field of the JSON
// document the hook receives on stdin.
//
// This is the only test helper that needs a JSON parser in the shell, and POSIX
// has no built-in one (the Windows mirror can lean on PowerShell's
// ConvertFrom-Json). It shells out to jq and skips when jq is absent.
//
// The skip matters: without it the hook still runs, produces no output, and the
// test fails on its content assertion instead -- reporting `"" does not contain
// "final answer content"`, which reads as a defect in the hook plumbing rather
// than a missing tool on the machine.
func printStdinJSONFieldCmd(t *testing.T, field string) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed; it is required to read a JSON field from hook stdin")
	}
	return `cat | jq -r '.` + field + `'`
}

// stderrExit2Cmd returns a command writing msg to stderr and exiting with
// code 2 (the blocking exit code of the hook protocol).
func stderrExit2Cmd(msg string) string {
	return "echo '" + msg + "' >&2; exit 2"
}

// assertSamePath asserts that want and got refer to the same directory.
// EvalSymlinks makes the comparison stable on macOS, where TempDir lives
// under /var -> /private/var.
func assertSamePath(t *testing.T, want, got string) {
	t.Helper()
	wantResolved, err := filepath.EvalSymlinks(want)
	require.NoError(t, err)
	gotResolved, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, wantResolved, gotResolved)
}
