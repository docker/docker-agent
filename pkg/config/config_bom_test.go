package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// utf8BOM is what PowerShell 5 (Out-File, Set-Content -Encoding utf8) and
// some editors write at the start of a UTF-8 file.
const utf8BOM = "\xef\xbb\xbf"

func TestLoad_UTF8BOM(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"LF", "agents:\n  root:\n    model: openai/gpt-4o\n    instruction: hi\n"},
		{"CRLF", "agents:\r\n  root:\r\n    model: openai/gpt-4o\r\n    instruction: hi\r\n"},
		{"version first", "version: \"" + latest.Version + "\"\nagents:\n  root:\n    model: openai/gpt-4o\n    instruction: hi\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want, err := Load(t.Context(), NewBytesSource("plain.yaml", []byte(tc.yaml)))
			require.NoError(t, err)

			got, err := Load(t.Context(), NewBytesSource("bom.yaml", []byte(utf8BOM+tc.yaml)))
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestLoad_BOMOnlyAtStartIsStripped(t *testing.T) {
	t.Parallel()

	// A BOM-like sequence inside a value is content and must survive.
	cfg, err := Load(t.Context(), NewBytesSource("bom.yaml", []byte(utf8BOM+
		"agents:\n  root:\n    model: openai/gpt-4o\n    instruction: \"keep \ufeff this\"\n")))
	require.NoError(t, err)
	assert.Equal(t, "keep \ufeff this", cfg.Agents.First().Instruction)
}

func TestReadHookDropIn_UTF8BOM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeHookDropIn(t, dir, "10-notify.yaml", utf8BOM+"stop:\n  type: command\n  command: echo done\n")
	hooks, err := readHookDropIn(filepath.Join(dir, "10-notify.yaml"))
	require.NoError(t, err)
	require.Len(t, hooks.Stop, 1)
	assert.Equal(t, "echo done", hooks.Stop[0].Command)
}
