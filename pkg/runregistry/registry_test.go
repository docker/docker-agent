package runregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestWriteAndList_RoundTrip(t *testing.T) {
	withTempDataDir(t)

	rec := Record{
		PID:       os.Getpid(),
		Addr:      "http://127.0.0.1:1234",
		SessionID: "sess-1",
		Agent:     "root",
		StartedAt: time.Now(),
	}
	cleanup, err := Write(rec)
	require.NoError(t, err)

	records, err := List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, rec.SessionID, records[0].SessionID)
	assert.Equal(t, rec.Addr, records[0].Addr)

	cleanup()
	cleanup() // safe to call twice

	records, err = List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

// TestWrite_RestrictsDirectoryPermissions verifies that the registry
// directory is created with 0o700 so other local users cannot enumerate
// running PIDs/addresses by listing it.
func TestWrite_RestrictsDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits are not enforced on Windows")
	}
	withTempDataDir(t)

	cleanup, err := Write(Record{
		PID:       os.Getpid(),
		Addr:      "http://127.0.0.1:1",
		SessionID: "s",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)

	info, err := os.Stat(Dir())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "registry dir must not be world- or group-readable")
}

func TestList_DropsStaleRecords(t *testing.T) {
	withTempDataDir(t)

	writeRecord(t, "999999.json", Record{
		PID: 999999, Addr: "x", SessionID: "y",
		StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})

	records, err := List()
	require.NoError(t, err)
	assert.Empty(t, records)

	_, err = os.Stat(filepath.Join(Dir(), "999999.json"))
	assert.True(t, os.IsNotExist(err))
}

func TestLatest_PicksMostRecent(t *testing.T) {
	withTempDataDir(t)

	pid := os.Getpid()
	writeRecord(t, "1.json", Record{PID: pid, Addr: "http://a", SessionID: "old", StartedAt: time.Now().Add(-time.Hour)})
	writeRecord(t, "2.json", Record{PID: pid, Addr: "http://b", SessionID: "new", StartedAt: time.Now()})

	rec, ok, err := Latest()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "new", rec.SessionID)
}

func TestFind(t *testing.T) {
	withTempDataDir(t)

	pid := os.Getpid()
	writeRecord(t, "1.json", Record{PID: pid, Addr: "http://127.0.0.1:1111", SessionID: "alpha", StartedAt: time.Now().Add(-time.Hour)})
	writeRecord(t, "2.json", Record{PID: pid, Addr: "http://127.0.0.1:2222", SessionID: "beta", StartedAt: time.Now()})

	t.Run("empty target returns latest", func(t *testing.T) {
		rec, err := Find("")
		require.NoError(t, err)
		assert.Equal(t, "beta", rec.SessionID)
	})

	t.Run("by pid", func(t *testing.T) {
		rec, err := Find(strconv.Itoa(pid))
		require.NoError(t, err)
		assert.Equal(t, pid, rec.PID)
	})

	t.Run("by addr", func(t *testing.T) {
		rec, err := Find("http://127.0.0.1:1111")
		require.NoError(t, err)
		assert.Equal(t, "alpha", rec.SessionID)
	})

	t.Run("by addr trims trailing slash", func(t *testing.T) {
		rec, err := Find("http://127.0.0.1:2222/")
		require.NoError(t, err)
		assert.Equal(t, "beta", rec.SessionID)
	})

	t.Run("by session id exact", func(t *testing.T) {
		rec, err := Find("alpha")
		require.NoError(t, err)
		assert.Equal(t, "alpha", rec.SessionID)
	})

	t.Run("unknown pid errors", func(t *testing.T) {
		_, err := Find("999999999")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no live run with pid")
		assert.ErrorIs(t, err, ErrNoRun)
	})

	t.Run("unknown addr errors", func(t *testing.T) {
		_, err := Find("http://nope")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no live run at")
		assert.ErrorIs(t, err, ErrNoRun)
	})

	t.Run("unknown session id errors", func(t *testing.T) {
		_, err := Find("zzz")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no live run matches")
		assert.ErrorIs(t, err, ErrNoRun)
	})
}

func TestFind_AmbiguousSessionID(t *testing.T) {
	withTempDataDir(t)

	pid := os.Getpid()
	writeRecord(t, "1.json", Record{PID: pid, Addr: "http://a", SessionID: "shared-1", StartedAt: time.Now()})
	writeRecord(t, "2.json", Record{PID: pid, Addr: "http://b", SessionID: "shared-2", StartedAt: time.Now()})

	_, err := Find("shared")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

// TestFind_ExactMatchBeatsSubstring guards against a regression where an
// exact session-id match was reported as ambiguous because a longer id
// contained it as a substring.
func TestFind_ExactMatchBeatsSubstring(t *testing.T) {
	withTempDataDir(t)

	pid := os.Getpid()
	writeRecord(t, "1.json", Record{PID: pid, Addr: "http://a", SessionID: "abc", StartedAt: time.Now()})
	writeRecord(t, "2.json", Record{PID: pid, Addr: "http://b", SessionID: "abcd", StartedAt: time.Now()})

	rec, err := Find("abc")
	require.NoError(t, err)
	assert.Equal(t, "abc", rec.SessionID)
}

func TestFind_EmptyRegistry(t *testing.T) {
	withTempDataDir(t)

	_, err := Find("")
	require.ErrorIs(t, err, ErrNoRun)

	_, err = Find("123")
	require.ErrorIs(t, err, ErrNoRun)
}

func withTempDataDir(t *testing.T) {
	t.Helper()
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
}

func writeRecord(t *testing.T, name string, rec Record) {
	t.Helper()
	require.NoError(t, os.MkdirAll(Dir(), 0o755))
	buf, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(Dir(), name), buf, 0o600))
}
