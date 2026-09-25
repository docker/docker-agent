package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestCanonicalFilePathPreservesLeafSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink privileges")
	}
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(sub, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "real.yaml"), []byte("config"), 0o600))
	require.NoError(t, os.Symlink("sub/real.yaml", filepath.Join(dir, "agent.yaml")))
	parent, err := CanonicalPath(dir)
	require.NoError(t, err)
	got, err := CanonicalFilePath(filepath.Join(dir, "agent.yaml"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "agent.yaml"), got)
	require.NoError(t, os.WriteFile(filepath.Join(sub, "real.yaml"), []byte("agents:\n  root:\n    model: openai/gpt-5.6\n    instruction_file: prompt.md\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("original instructions"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "prompt.md"), []byte("wrong instructions"), 0o600))
	cfg, err := config.Load(t.Context(), config.NewFileSource(got))
	require.NoError(t, err)
	assert.Equal(t, "original instructions", cfg.Agents.First().Instruction)
}

func TestCanonicalPathStoredCase(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS stored casing")
	}
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "work"), 0o700))
	if _, err := os.Stat(filepath.Join(dir, "WORK")); err != nil {
		t.Skip("case-sensitive volume")
	}
	want, err := CanonicalPath(filepath.Join(dir, "work"))
	require.NoError(t, err)
	got, err := CanonicalPath(filepath.Join(dir, "WORK"))
	require.NoError(t, err)
	assert.Equal(t, want, got)
	got, err = CanonicalPath(filepath.Join(dir, "WORK", "new", "cache"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(want, "new", "cache"), got)
}

func TestCanonicalPathRejectsDanglingSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink privileges")
	}
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(dir, "state")))
	for _, path := range []string{filepath.Join(dir, "state"), filepath.Join(dir, "state", "cache")} {
		_, err := CanonicalPath(path)
		require.ErrorContains(t, err, "dangling symlink")
	}
}
