package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
}

func TestExpandCommands(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "no patterns",
			content: "# My Skill\n\nJust regular markdown content.",
			want:    "# My Skill\n\nJust regular markdown content.",
		},
		{
			name:    "simple echo",
			content: "Hello !`echo world`!",
			want:    "Hello world!",
		},
		{
			name:    "multiple commands",
			content: "Name: !`echo alice`, Age: !`echo 30`",
			want:    "Name: alice, Age: 30",
		},
		{
			name:    "multiline output",
			content: "Files:\n!`printf 'a.go\nb.go\nc.go\n'`\nEnd.",
			want:    "Files:\na.go\nb.go\nc.go\nEnd.",
		},
		{
			name:    "empty output",
			content: "Before !`true` after",
			want:    "Before  after",
		},
		{
			name:    "pipes",
			content: "Count: !`printf 'a\nb\nc\n' | wc -l | tr -d ' '`",
			want:    "Count: 3",
		},
		{
			name:    "preserves regular backticks",
			content: "Use `echo hello` to print.\n\nCode: ```go\nfmt.Println()\n```",
			want:    "Use `echo hello` to print.\n\nCode: ```go\nfmt.Println()\n```",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExpandCommands(t.Context(), tt.content, ShellRunner(t.TempDir()))
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestExpandCommands_WorkingDirectory(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("hello"), 0o644))

	result := ExpandCommands(t.Context(), "Content: !`cat test.txt`", ShellRunner(tmpDir))
	assert.Equal(t, "Content: hello", result)
}

func TestExpandCommands_ScriptExecution(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "info.sh"), []byte("#!/bin/sh\necho from-script"), 0o755))

	result := ExpandCommands(t.Context(), "Output: !`./info.sh`", ShellRunner(tmpDir))
	assert.Equal(t, "Output: from-script", result)
}

func TestExpandCommands_FailedCommand(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	result := ExpandCommands(t.Context(), "Before !`nonexistent_command_12345` after", ShellRunner(t.TempDir()))
	assert.Contains(t, result, "Before ")
	assert.Contains(t, result, "[error executing `nonexistent_command_12345`:")
	assert.Contains(t, result, " after")
}

func TestExpandCommands_RefusedCommand(t *testing.T) {
	t.Parallel()

	run := func(context.Context, string) (string, error) {
		return "", errors.New("the user rejected the command")
	}

	result := ExpandCommands(t.Context(), "Status: !`git status`", run)
	assert.Equal(t, "Status: [error executing `git status`: the user rejected the command]", result)
}

type expansionAbortError struct{ error }

func (expansionAbortError) AbortExpansion() {}

func TestExpandCommands_AbortsOnFatalError(t *testing.T) {
	t.Parallel()

	calls := 0
	run := func(context.Context, string) (string, error) {
		calls++
		return "", expansionAbortError{errors.New("stop the run")}
	}

	result, err := ExpandCommandsWithError(t.Context(), "!`first` !`second`", run)

	require.EqualError(t, err, "stop the run")
	assert.Empty(t, result)
	assert.Equal(t, 1, calls)
}

func TestExpandCommands_CancelledContextStopsExpansion(t *testing.T) {
	t.Parallel()

	calls := 0
	run := func(ctx context.Context, _ string) (string, error) {
		calls++
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := ExpandCommandsWithError(ctx, "!`first` !`second`", run)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, result)
	assert.Zero(t, calls)
}

func TestExpandCommands_PropagatesRunnerCancellation(t *testing.T) {
	t.Parallel()

	calls := 0
	run := func(context.Context, string) (string, error) {
		calls++
		return "", fmt.Errorf("runner stopped: %w", context.Canceled)
	}

	result, err := ExpandCommandsWithError(t.Context(), "!`first` !`second`", run)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, result)
	assert.Equal(t, 1, calls)
}

func TestExpandCommands_RunnerSeesCancelledContext(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	ctx, cancel := context.WithCancel(t.Context())
	called := false
	run := func(ctx context.Context, command string) (string, error) {
		called = true
		cancel()
		return ShellRunner(t.TempDir())(ctx, command)
	}

	result, err := ExpandCommandsWithError(ctx, "Result: !`echo hello`", run)
	assert.True(t, called)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, result)
}
