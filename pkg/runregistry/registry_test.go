package runregistry

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRegistry returns a Registry rooted at an isolated temp dir whose pids
// are all considered alive. Because it shares no process-global state, every
// test that uses it can run with t.Parallel().
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := New(filepath.Join(t.TempDir(), "runs"))
	r.alive = func(int) bool { return true }
	return r
}

func TestWriteAndList_RoundTrip(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	rec := Record{
		PID:       1234,
		Addr:      "http://127.0.0.1:1234",
		SessionID: "sess-1",
		Agent:     "root",
		StartedAt: time.Now(),
	}
	cleanup, err := r.Write(rec)
	require.NoError(t, err)

	// Unix mode bits are not enforced on Windows.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(r.Dir(), "1234.json"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "record must not be group- or world-readable")
	}

	records, err := r.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, rec.SessionID, records[0].SessionID)
	assert.Equal(t, rec.Addr, records[0].Addr)

	cleanup()
	cleanup() // safe to call twice

	records, err = r.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

// TestWrite_RestrictsDirectoryPermissions verifies that the registry
// directory is created with 0o700 so other local users cannot enumerate
// running PIDs/addresses by listing it.
func TestWrite_RestrictsDirectoryPermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits are not enforced on Windows")
	}
	r := newTestRegistry(t)

	cleanup, err := r.Write(Record{
		PID:       1,
		Addr:      "http://127.0.0.1:1",
		SessionID: "s",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)

	info, err := os.Stat(r.Dir())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "registry dir must not be world- or group-readable")
}

// TestWrite_TightensExistingDirectoryPermissions ensures Write fixes the
// permissions of a pre-existing, too-permissive registry dir (MkdirAll only
// applies its mode when creating the directory).
func TestWrite_TightensExistingDirectoryPermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits are not enforced on Windows")
	}
	r := newTestRegistry(t)
	require.NoError(t, os.MkdirAll(r.Dir(), 0o755))

	cleanup, err := r.Write(Record{
		PID:       1,
		Addr:      "http://127.0.0.1:1",
		SessionID: "s",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)

	info, err := os.Stat(r.Dir())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "registry dir must be tightened to 0o700")
}

func TestList_DropsStaleRecords(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	r.alive = func(pid int) bool { return pid != 999999 }

	writeRecord(t, r, "999999.json", Record{
		PID: 999999, Addr: "x", SessionID: "y",
		StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})

	records, err := r.List()
	require.NoError(t, err)
	assert.Empty(t, records)

	_, err = os.Stat(filepath.Join(r.Dir(), "999999.json"))
	assert.True(t, os.IsNotExist(err))
}

func TestList_RealLiveness(t *testing.T) {
	t.Parallel()
	r := New(filepath.Join(t.TempDir(), "runs"))

	exited := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	require.NoError(t, exited.Run())
	deadPID := exited.Process.Pid

	writeRecord(t, r, strconv.Itoa(os.Getpid())+".json", Record{PID: os.Getpid(), Addr: "http://live", SessionID: "live", StartedAt: time.Now()})
	writeRecord(t, r, strconv.Itoa(deadPID)+".json", Record{PID: deadPID, Addr: "http://dead", SessionID: "dead", StartedAt: time.Now()})

	records, err := r.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "live", records[0].SessionID)
}

func TestLatest_PicksMostRecent(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	writeRecord(t, r, "1.json", Record{PID: 1, Addr: "http://a", SessionID: "old", StartedAt: time.Now().Add(-time.Hour)})
	writeRecord(t, r, "2.json", Record{PID: 2, Addr: "http://b", SessionID: "new", StartedAt: time.Now()})

	rec, ok, err := r.Latest()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "new", rec.SessionID)
}

func TestFind(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	writeRecord(t, r, "1111.json", Record{PID: 1111, Addr: "http://127.0.0.1:1111", SessionID: "alpha", StartedAt: time.Now().Add(-time.Hour)})
	writeRecord(t, r, "2222.json", Record{PID: 2222, Addr: "http://127.0.0.1:2222", SessionID: "beta", StartedAt: time.Now()})

	t.Run("empty target returns latest", func(t *testing.T) {
		rec, err := r.Find("")
		require.NoError(t, err)
		assert.Equal(t, "beta", rec.SessionID)
	})

	t.Run("by pid", func(t *testing.T) {
		rec, err := r.Find(strconv.Itoa(1111))
		require.NoError(t, err)
		assert.Equal(t, 1111, rec.PID)
	})

	t.Run("by addr", func(t *testing.T) {
		rec, err := r.Find("http://127.0.0.1:1111")
		require.NoError(t, err)
		assert.Equal(t, "alpha", rec.SessionID)
	})

	t.Run("by addr trims trailing slash", func(t *testing.T) {
		rec, err := r.Find("http://127.0.0.1:2222/")
		require.NoError(t, err)
		assert.Equal(t, "beta", rec.SessionID)
	})

	t.Run("by session id exact", func(t *testing.T) {
		rec, err := r.Find("alpha")
		require.NoError(t, err)
		assert.Equal(t, "alpha", rec.SessionID)
	})

	t.Run("unknown pid errors", func(t *testing.T) {
		_, err := r.Find("999999999")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no live run with pid")
		assert.ErrorIs(t, err, ErrNoRun)
	})

	t.Run("unknown addr errors", func(t *testing.T) {
		_, err := r.Find("http://nope")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no live run at")
		assert.ErrorIs(t, err, ErrNoRun)
	})

	t.Run("unknown session id errors", func(t *testing.T) {
		_, err := r.Find("zzz")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no live run matches")
		assert.ErrorIs(t, err, ErrNoRun)
	})
}

func TestFind_AmbiguousSessionID(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	writeRecord(t, r, "1.json", Record{PID: 1, Addr: "http://a", SessionID: "shared-1", StartedAt: time.Now()})
	writeRecord(t, r, "2.json", Record{PID: 2, Addr: "http://b", SessionID: "shared-2", StartedAt: time.Now()})

	_, err := r.Find("shared")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

// TestFind_ExactMatchBeatsSubstring guards against a regression where an
// exact session-id match was reported as ambiguous because a longer id
// contained it as a substring.
func TestFind_ExactMatchBeatsSubstring(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	writeRecord(t, r, "1.json", Record{PID: 1, Addr: "http://a", SessionID: "abc", StartedAt: time.Now()})
	writeRecord(t, r, "2.json", Record{PID: 2, Addr: "http://b", SessionID: "abcd", StartedAt: time.Now()})

	rec, err := r.Find("abc")
	require.NoError(t, err)
	assert.Equal(t, "abc", rec.SessionID)
}

func TestFind_EmptyRegistry(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	_, err := r.Find("")
	require.ErrorIs(t, err, ErrNoRun)

	_, err = r.Find("123")
	require.ErrorIs(t, err, ErrNoRun)
}

func writeRecord(t *testing.T, r *Registry, name string, rec Record) {
	t.Helper()
	require.NoError(t, os.MkdirAll(r.Dir(), 0o755))
	buf, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), name), buf, 0o600))
}
