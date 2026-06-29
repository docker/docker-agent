package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPath(t *testing.T) {
	t.Parallel()
	t.Run("accepts UUID-shaped IDs", func(t *testing.T) {
		path, err := Path("/plans", "7c2d8f0a-1234-4abc-9def-1234567890ab")
		require.NoError(t, err)
		assert.Equal(t, "/plans/7c2d8f0a-1234-4abc-9def-1234567890ab.md", path)
	})

	// Path-traversal defence: the regex is the only thing standing between an
	// adversarial session ID and arbitrary disk writes.
	for _, name := range []string{
		"",
		"../escape",
		"a/b",
		`a\b`,
		"-leading-dash",
		"_leading-underscore",
		".leading-dot",
		strings.Repeat("a", 200),
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := Path("/plans", name)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidSessionID)
		})
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	path, err := WriteContent(dir, "session-1", "# my plan\nstep 1\n")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "session-1.md"), path)

	got, gotPath, err := ReadContent(dir, "session-1")
	require.NoError(t, err)
	assert.Equal(t, path, gotPath)
	assert.Equal(t, "# my plan\nstep 1\n", got)
}

func TestWriteContentOverwrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := WriteContent(dir, "s", "v1")
	require.NoError(t, err)
	_, err = WriteContent(dir, "s", "v2")
	require.NoError(t, err)

	got, _, err := ReadContent(dir, "s")
	require.NoError(t, err)
	assert.Equal(t, "v2", got)
}

func TestWriteContentCreatesDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "plans")

	_, err := WriteContent(dir, "s", "hello")
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestReadContentNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// The path is returned alongside the sentinel so the caller can include it
	// in a "no plan yet, would write to …" message.
	content, path, err := ReadContent(dir, "ghost")
	assert.Empty(t, content)
	assert.Equal(t, filepath.Join(dir, "ghost.md"), path)
	assert.ErrorIs(t, err, ErrPlanNotFound)
}

func TestReadContentInvalidSessionID(t *testing.T) {
	t.Parallel()
	_, _, err := ReadContent(t.TempDir(), "../escape")
	assert.ErrorIs(t, err, ErrInvalidSessionID)
}

func TestSweepRemovesOldPlans(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()

	makeFile := func(name string, mtime time.Time) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		require.NoError(t, os.Chtimes(path, mtime, mtime))
		return path
	}

	fresh := makeFile("fresh.md", now.Add(-time.Hour))
	stale := makeFile("stale.md", now.Add(-maxPlanAge-time.Hour))
	// Sweep targets *.md only so we don't poke at unrelated files a future
	// version of the toolset might drop in the dir.
	other := makeFile("notes.txt", now.Add(-maxPlanAge*2))
	subdir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o700))

	require.NoError(t, Sweep(dir, now, maxPlanAge))

	_, err := os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist, "stale plan should have been removed")

	_, err = os.Stat(fresh)
	require.NoError(t, err, "fresh plan should survive")

	_, err = os.Stat(other)
	require.NoError(t, err, "non-md file should survive")

	_, err = os.Stat(subdir)
	require.NoError(t, err, "subdir should survive")
}

func TestSweepMissingDirIsNoOp(t *testing.T) {
	t.Parallel()
	err := Sweep(filepath.Join(t.TempDir(), "does-not-exist"), time.Now(), maxPlanAge)
	assert.NoError(t, err)
}

func TestTools(t *testing.T) {
	t.Parallel()
	ts := New()
	got, err := ts.Tools(t.Context())
	require.NoError(t, err)

	names := make([]string, 0, len(got))
	for _, tool := range got {
		names = append(names, tool.Name)
		// Handlers must be nil so the runtime's toolMap takes precedence; a
		// non-nil handler here would silently bypass the runtime path.
		assert.Nil(t, tool.Handler, "tool %q should not declare a handler", tool.Name)
	}
	assert.ElementsMatch(t, []string{ToolNameWritePlan, ToolNameReadPlan, ToolNameExitPlanMode}, names)
}

func TestInstructionsNonEmpty(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, (&ToolSet{}).Instructions())
}
