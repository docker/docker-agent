package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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
			result, err := ExpandCommandsWithError(t.Context(), tt.content, ShellRunner(t.TempDir()))
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestExpandCommands_WorkingDirectory(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("hello"), 0o644))

	result, err := ExpandCommandsWithError(t.Context(), "Content: !`cat test.txt`", ShellRunner(tmpDir))
	require.NoError(t, err)
	assert.Equal(t, "Content: hello", result)
}

func TestExpandCommands_ScriptExecution(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "info.sh"), []byte("#!/bin/sh\necho from-script"), 0o755))

	result, err := ExpandCommandsWithError(t.Context(), "Output: !`./info.sh`", ShellRunner(tmpDir))
	require.NoError(t, err)
	assert.Equal(t, "Output: from-script", result)
}

func TestExpandCommands_FailedCommand(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	result, err := ExpandCommandsWithError(t.Context(), "Before !`nonexistent_command_12345` after", ShellRunner(t.TempDir()))
	require.NoError(t, err)
	assert.Contains(t, result, "Before ")
	assert.Contains(t, result, "[error executing `nonexistent_command_12345`:")
	assert.Contains(t, result, " after")
}

func TestExpandCommands_RefusedCommand(t *testing.T) {
	t.Parallel()

	run := func(context.Context, string) (string, error) {
		return "", errors.New("the user rejected the command")
	}

	result, err := ExpandCommandsWithError(t.Context(), "Status: !`git status`", run)
	require.NoError(t, err)
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

func TestCommandOutputBuffer(t *testing.T) {
	t.Parallel()
	var output commandOutputBuffer
	for _, data := range [][]byte{nil, bytes.Repeat([]byte("x"), maxOutputSize-1), []byte("yz"), []byte("ignored")} {
		n, err := output.Write(data)
		require.NoError(t, err)
		assert.Equal(t, len(data), n, "discarded bytes must still count as written")
	}
	assert.Equal(t, maxOutputSize, output.buffer.Len())
	assert.True(t, strings.HasSuffix(output.buffer.String(), "y"))
}

func TestRunCommandDrainsBeyondOutputLimit(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	dir := t.TempDir()
	output, err := runCommand(t.Context(), fmt.Sprintf("head -c %d /dev/zero; printf done > finished", maxOutputSize+10000), dir)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("\x00", maxOutputSize), output)
	data, err := os.ReadFile(filepath.Join(dir, "finished"))
	require.NoError(t, err)
	assert.Equal(t, "done", string(data))
}

func TestRunCommandPreservesStderrError(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	output, err := runCommand(t.Context(), "printf ignored; printf failure >&2; exit 7", t.TempDir())
	require.ErrorContains(t, err, "failure")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
	assert.Empty(t, output)
}

func TestRunCommandCancellationWithInheritedPipes(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("SHELL", "/bin/sh")
	for _, tc := range []struct {
		name, prefix, redirect string
	}{
		{name: "stdout", redirect: "2>/dev/null"},
		{name: "stderr", redirect: ">/dev/null"},
		{name: "both"},
		{name: "stdout beyond limit", prefix: fmt.Sprintf("head -c %d /dev/zero; ", maxOutputSize+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dir := t.TempDir()
			done := make(chan error, 1)
			go func() {
				_, err := runCommand(ctx, tc.prefix+"sleep 30 </dev/null "+tc.redirect+" & child=$!; printf '%s' \"$child\" > child.pid; wait", dir)
				done <- err
			}()
			childPath := filepath.Join(dir, "child.pid")
			childProcess := func() *os.Process {
				data, err := os.ReadFile(childPath)
				if err != nil {
					return nil
				}
				pid, err := strconv.Atoi(string(data))
				if err != nil || pid <= 0 {
					return nil
				}
				child, _ := os.FindProcess(pid)
				return child
			}
			joined := false
			t.Cleanup(func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("command did not stop during cleanup")
					}
				}
				if child := childProcess(); child != nil {
					_ = child.Kill()
				}
			})
			require.Eventually(t, func() bool { return childProcess() != nil }, 5*time.Second, 10*time.Millisecond)
			cancel()
			select {
			case err := <-done:
				joined = true
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation waited for a descendant to close inherited output pipes")
			}
		})
	}
}

func TestRunCommandPreservesDelayedBackgroundOutput(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("SHELL", "/bin/sh")
	output, err := runCommand(t.Context(), "printf foreground; (sleep 1; printf background) &", t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "foregroundbackground", output)
}
