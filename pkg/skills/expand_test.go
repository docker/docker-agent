package skills

import (
	"context"
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
			result := ExpandCommands(t.Context(), tt.content, t.TempDir())
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestExpandCommands_WorkingDirectory(t *testing.T) {
	skipOnWindows(t)

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("hello"), 0o644))

	result := ExpandCommands(t.Context(), "Content: !`cat test.txt`", tmpDir)
	assert.Equal(t, "Content: hello", result)
}

func TestExpandCommands_ScriptExecution(t *testing.T) {
	skipOnWindows(t)

	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "info.sh"), []byte("#!/bin/sh\necho from-script"), 0o755))

	result := ExpandCommands(t.Context(), "Output: !`./info.sh`", tmpDir)
	assert.Equal(t, "Output: from-script", result)
}

func TestExpandCommands_FailedCommand(t *testing.T) {
	skipOnWindows(t)

	result := ExpandCommands(t.Context(), "Before !`nonexistent_command_12345` after", t.TempDir())
	assert.Contains(t, result, "Before ")
	assert.Contains(t, result, "[error executing `nonexistent_command_12345`:")
	assert.Contains(t, result, " after")
}

func TestExpandCommands_CancelledContext(t *testing.T) {
	skipOnWindows(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := ExpandCommands(ctx, "Result: !`echo hello`", t.TempDir())
	assert.Contains(t, result, "[error executing `echo hello`:")
}
