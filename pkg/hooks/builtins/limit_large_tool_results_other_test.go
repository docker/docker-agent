//go:build !js

package builtins_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

func TestLimitLargeToolResultsSpillFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	// A file at the directory path fails even when the test runs as root.
	require.NoError(t, os.WriteFile(filepath.Join(os.TempDir(), "docker-agent-tool-results"), nil, 0o600))
	fn := lookup(t, builtins.LimitLargeToolResults)
	for _, name := range []string{"shell", "read_file"} {
		category := "shell"
		if name == "read_file" {
			category = "filesystem"
		}
		out, err := fn(t.Context(), &hooks.Input{HookEventName: hooks.EventToolResponseTransform, ToolCategory: category, ToolName: name, ToolResponse: strings.Repeat("世", 100_000)}, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		got := *out.HookSpecificOutput.UpdatedToolResponse
		assert.LessOrEqual(t, len(got), 50*1024)
		assert.True(t, utf8.ValidString(got))
		assert.Contains(t, got, "could not be saved")
		assert.NotContains(t, got, "available in a file:")
	}
}

func TestLimitLargeToolResultsBackgroundStatusAndFullLog(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	payload := "Job ID: job_1\nCommand: " + strings.Repeat("c", 100_000) + "\nStatus: failed\nExit Code: 1\n\n--- Output ---\n" + strings.Repeat("x", 10*1024*1024) + "FINAL DIAGNOSTIC"
	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{HookEventName: hooks.EventToolResponseTransform, ToolCategory: "background_jobs", ToolName: "wait_background_job", ToolResponse: payload}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	got := *out.HookSpecificOutput.UpdatedToolResponse
	assert.LessOrEqual(t, len(got), 50*1024)
	assert.Contains(t, got, "Job ID: job_1")
	assert.Contains(t, got, "Status: failed")
	assert.Contains(t, got, "Exit Code: 1")
	assert.Contains(t, got, "FINAL DIAGNOSTIC")
	stored, err := os.ReadFile(extractLargeResultPath(t, got))
	require.NoError(t, err)
	assert.Equal(t, payload, string(stored))
}
