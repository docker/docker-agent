//go:build windows

package hooks

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Command hooks run under PowerShell on Windows (shellpath.DetectShell).
// These helpers generate PowerShell equivalents of the POSIX commands in
// helpers_other_test.go so the tests validate the same hook contracts
// on every platform.

// emitContextEnvPwdCmd returns a command printing the hook JSON envelope
// whose additional_context is the values of envVars plus the working
// directory, joined with ":". ConvertTo-Json escapes the backslashes of
// Windows paths so the output stays valid JSON.
func emitContextEnvPwdCmd(envVars ...string) string {
	parts := make([]string, 0, len(envVars)+1)
	for _, name := range envVars {
		parts = append(parts, "$env:"+name)
	}
	parts = append(parts, "(Get-Location).Path")
	return `@{hook_specific_output=@{additional_context=(@(` + strings.Join(parts, ", ") + `) -join ':')}} | ConvertTo-Json -Compress`
}

// printStdinJSONFieldCmd returns a command printing one field of the JSON
// document the hook receives on stdin.
// Signature mirrors the POSIX helper, which needs *testing.T to skip when jq is
// missing. PowerShell's ConvertFrom-Json is built in, so there is nothing to
// skip on here.
func printStdinJSONFieldCmd(t *testing.T, field string) string {
	t.Helper()
	return `([Console]::In.ReadToEnd() | ConvertFrom-Json).` + field
}

// stderrExit2Cmd returns a command writing msg to stderr and exiting with
// code 2 (the blocking exit code of the hook protocol).
func stderrExit2Cmd(msg string) string {
	return "[Console]::Error.WriteLine('" + msg + "'); exit 2"
}

// assertSamePath asserts that want and got refer to the same directory.
// EvalSymlinks maps Windows 8.3 short names (RUNNER~1) onto long names;
// EqualFold absorbs the case-insensitive filesystem (drive-letter case).
func assertSamePath(t *testing.T, want, got string) {
	t.Helper()
	wantResolved, err := filepath.EvalSymlinks(want)
	require.NoError(t, err)
	gotResolved, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.True(t, strings.EqualFold(wantResolved, gotResolved),
		"paths differ: want %q, got %q", wantResolved, gotResolved)
}
