//go:build windows

package shell

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// The shell tool runs commands under PowerShell on Windows
// (shellpath.DetectShell). These helpers generate PowerShell equivalents
// of the POSIX commands in helpers_other_test.go so the tests validate
// the same contracts on every platform.
//
// Script-tool commands (validateConfig) are scanned with os.Expand, which
// flags every `$name` in cmd as an arg reference. The PowerShell variants
// therefore avoid `$` syntax entirely and read the environment through
// [Environment]::GetEnvironmentVariable instead of $env:NAME.

// assertPlatformDefaultShell asserts New detected the platform shell:
// PowerShell (or the cmd.exe fallback) on Windows; SHELL is ignored.
func assertPlatformDefaultShell(t *testing.T, h *shellHandler, _ string) {
	t.Helper()
	wantShell, wantArgs := shellpath.DetectWindowsShell()
	assert.Equal(t, wantShell, h.shell)
	assert.Equal(t, wantArgs, h.shellArgsPrefix)
}

// pwdCmd returns a command printing the working directory. Bare `pwd`
// (alias of Get-Location) would print a formatted table; .Path prints
// just the path.
func pwdCmd() string {
	return "(Get-Location).Path"
}

// envDumpCmd returns a command printing the full environment of the
// spawned process as "key=value" lines, matching Linux `env` output.
// Uses Invoke-Expression with string formatting to avoid writing literal
// $ signs, which the script tool's arg validator would misinterpret as
// an undefined arg reference.
func envDumpCmd() string {
	return `Invoke-Expression ('Get-ChildItem env: | ForEach-Object {{ [string]::Concat({0}_.Name, ''='', {0}_.Value) }}' -f [char]36)`
}

func envDumpContainsName(output, name string) bool {
	return strings.Contains(output, name)
}

// printEnvValueCmd returns a command printing the value of one
// environment variable, without a trailing newline.
func printEnvValueCmd(name string) string {
	return `[Console]::Out.Write([Environment]::GetEnvironmentVariable('` + name + `'))`
}

// printEnvValuesAndPwdCmd returns a command printing the values of
// envVars plus the working directory, joined with "|".
func printEnvValuesAndPwdCmd(envVars ...string) string {
	parts := make([]string, 0, len(envVars)+1)
	for _, name := range envVars {
		parts = append(parts, `[Environment]::GetEnvironmentVariable('`+name+`')`)
	}
	parts = append(parts, "(Get-Location).Path")
	return `[Console]::Out.Write((@(` + strings.Join(parts, ", ") + `) -join '|'))`
}

// repeatMessageCmd returns a command printing the "message" env var
// "count" times, one per line.
func repeatMessageCmd() string {
	return `1..([int][Environment]::GetEnvironmentVariable('count')) | ForEach-Object { [Environment]::GetEnvironmentVariable('message') }`
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
