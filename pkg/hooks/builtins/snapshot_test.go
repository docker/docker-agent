package builtins_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/paths"
)

func TestSnapshotBuiltinUndoSurvivesStreamEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, true)
	require.NoError(t, err)
	fn, ok := r.LookupBuiltin(builtins.Snapshot)
	require.True(t, ok)

	dir := snapshotBuiltinRepo(t)
	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventTurnStart,
	}, nil)
	require.NoError(t, err)

	changedPath := filepath.Join(dir, "changed.txt")
	require.NoError(t, os.WriteFile(changedPath, []byte("changed"), 0o644))

	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventTurnEnd,
		Reason:        "continue",
	}, nil)
	require.NoError(t, err)

	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventTurnStart,
	}, nil)
	require.NoError(t, err)

	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventTurnEnd,
		Reason:        "normal",
	}, nil)
	require.NoError(t, err)

	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventSessionEnd,
		Reason:        "stream_ended",
	}, nil)
	require.NoError(t, err)

	entries, err := os.ReadDir(filepath.Join(paths.GetDataDir(), "snapshot"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.DirExists(t, filepath.Join(paths.GetDataDir(), "snapshot", entries[0].Name()))

	files, restored, err := ctrl.UndoLast(t.Context(), "s", dir)
	require.NoError(t, err)
	assert.True(t, restored)
	assert.Equal(t, 1, files)
	assert.NoFileExists(t, changedPath)
}

func TestSnapshotBuiltinListAndReset(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, true)
	require.NoError(t, err)
	fn, ok := r.LookupBuiltin(builtins.Snapshot)
	require.True(t, ok)

	dir := snapshotBuiltinRepo(t)

	// Initially: no checkpoints.
	assert.Empty(t, ctrl.List("s"))

	// Capture three snapshots: each turn modifies one file.
	recordTurn := func(t *testing.T, name, contents string) {
		t.Helper()
		_, err := fn(t.Context(), &hooks.Input{
			SessionID:     "s",
			Cwd:           dir,
			HookEventName: hooks.EventTurnStart,
		}, nil)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644))
		_, err = fn(t.Context(), &hooks.Input{
			SessionID:     "s",
			Cwd:           dir,
			HookEventName: hooks.EventTurnEnd,
			Reason:        "continue",
		}, nil)
		require.NoError(t, err)
	}

	recordTurn(t, "a.txt", "a")
	recordTurn(t, "b.txt", "b")
	recordTurn(t, "c.txt", "c")

	snaps := ctrl.List("s")
	require.Len(t, snaps, 3)
	assert.Equal(t, 1, snaps[0].Files)
	assert.Equal(t, 1, snaps[1].Files)
	assert.Equal(t, 1, snaps[2].Files)

	// Reset to snapshot 2: revert turn 3 only, leaving a.txt and b.txt intact.
	files, restored, err := ctrl.Reset(t.Context(), "s", dir, 2)
	require.NoError(t, err)
	assert.True(t, restored)
	assert.Equal(t, 1, files)
	assert.FileExists(t, filepath.Join(dir, "a.txt"))
	assert.FileExists(t, filepath.Join(dir, "b.txt"))
	assert.NoFileExists(t, filepath.Join(dir, "c.txt"))
	require.Len(t, ctrl.List("s"), 2)

	// Reset to original: revert remaining checkpoints, deleting all three files.
	files, restored, err = ctrl.Reset(t.Context(), "s", dir, 0)
	require.NoError(t, err)
	assert.True(t, restored)
	assert.Equal(t, 2, files)
	assert.NoFileExists(t, filepath.Join(dir, "a.txt"))
	assert.NoFileExists(t, filepath.Join(dir, "b.txt"))
	assert.Empty(t, ctrl.List("s"))

	// Subsequent reset is a no-op (nothing to revert).
	_, restored, err = ctrl.Reset(t.Context(), "s", dir, 0)
	require.NoError(t, err)
	assert.False(t, restored)
}

func TestSnapshotBuiltinResetKeepBeyondHistoryIsNoop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, true)
	require.NoError(t, err)
	fn, ok := r.LookupBuiltin(builtins.Snapshot)
	require.True(t, ok)

	dir := snapshotBuiltinRepo(t)
	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventTurnStart,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "s",
		Cwd:           dir,
		HookEventName: hooks.EventTurnEnd,
		Reason:        "continue",
	}, nil)
	require.NoError(t, err)

	// keep == len(history) means "keep everything" — no checkpoints reverted.
	files, restored, err := ctrl.Reset(t.Context(), "s", dir, 1)
	require.NoError(t, err)
	assert.False(t, restored)
	assert.Equal(t, 0, files)
	assert.FileExists(t, filepath.Join(dir, "a.txt"))
	require.Len(t, ctrl.List("s"), 1)

	// keep way past the end is also a no-op.
	_, restored, err = ctrl.Reset(t.Context(), "s", dir, 99)
	require.NoError(t, err)
	assert.False(t, restored)
	require.Len(t, ctrl.List("s"), 1)
}

func snapshotBuiltinRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitForSnapshotBuiltin(t, dir, "init")
	runGitForSnapshotBuiltin(t, dir, "config", "user.email", "test@example.com")
	runGitForSnapshotBuiltin(t, dir, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644))
	runGitForSnapshotBuiltin(t, dir, "add", ".")
	runGitForSnapshotBuiltin(t, dir, "commit", "-m", "init")
	return dir
}

func runGitForSnapshotBuiltin(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}
