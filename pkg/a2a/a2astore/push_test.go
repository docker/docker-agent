package a2astore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.Context(), filepath.Join(t.TempDir(), "push.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestSaveGeneratesIDAndRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	saved, err := s.Save(ctx, "task-1", &a2a.PushConfig{URL: "https://hook.example/cb"})
	require.NoError(t, err)
	assert.NotEmpty(t, saved.ID, "an ID should be generated when none is provided")
	assert.Equal(t, a2a.TaskID("task-1"), saved.TaskID)
	assert.Equal(t, "https://hook.example/cb", saved.URL)

	got, err := s.Get(ctx, "task-1", saved.ID)
	require.NoError(t, err)
	assert.Equal(t, saved, got)
}

func TestSavePreservesProvidedIDAndAuth(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	cfg := &a2a.PushConfig{
		ID:     "my-config",
		URL:    "https://hook.example/cb",
		Token:  "sekret",
		Tenant: "acme",
		Auth:   &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "abc123"},
	}
	saved, err := s.Save(ctx, "task-1", cfg)
	require.NoError(t, err)
	assert.Equal(t, "my-config", saved.ID)

	got, err := s.Get(ctx, "task-1", "my-config")
	require.NoError(t, err)
	assert.Equal(t, "sekret", got.Token)
	assert.Equal(t, "acme", got.Tenant)
	require.NotNil(t, got.Auth)
	assert.Equal(t, "Bearer", got.Auth.Scheme)
	assert.Equal(t, "abc123", got.Auth.Credentials)
}

func TestSaveUpsertsExistingConfig(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.Save(ctx, "task-1", &a2a.PushConfig{ID: "c1", URL: "https://old.example/cb"})
	require.NoError(t, err)
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{ID: "c1", URL: "https://new.example/cb", Token: "t"})
	require.NoError(t, err)

	got, err := s.Get(ctx, "task-1", "c1")
	require.NoError(t, err)
	assert.Equal(t, "https://new.example/cb", got.URL)
	assert.Equal(t, "t", got.Token)

	list, err := s.List(ctx, "task-1")
	require.NoError(t, err)
	assert.Len(t, list, 1, "upsert must not create a duplicate row")
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get(t.Context(), "task-1", "nope")
	assert.ErrorIs(t, err, push.ErrPushConfigNotFound)
}

func TestListMultipleAndEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	empty, err := s.List(ctx, "task-1")
	require.NoError(t, err)
	assert.NotNil(t, empty)
	assert.Empty(t, empty)

	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{ID: "a", URL: "https://a.example/cb"})
	require.NoError(t, err)
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{ID: "b", URL: "https://b.example/cb"})
	require.NoError(t, err)
	// A different task must not leak into task-1's list.
	_, err = s.Save(ctx, "task-2", &a2a.PushConfig{ID: "c", URL: "https://c.example/cb"})
	require.NoError(t, err)

	list, err := s.List(ctx, "task-1")
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.Save(ctx, "task-1", &a2a.PushConfig{ID: "c1", URL: "https://a.example/cb"})
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, "task-1", "c1"))
	_, err = s.Get(ctx, "task-1", "c1")
	require.ErrorIs(t, err, push.ErrPushConfigNotFound)

	// Deleting again (or a missing one) is not an error.
	require.NoError(t, s.Delete(ctx, "task-1", "c1"))
	require.NoError(t, s.Delete(ctx, "task-1", "never-existed"))
}

func TestDeleteAll(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.Save(ctx, "task-1", &a2a.PushConfig{ID: "a", URL: "https://a.example/cb"})
	require.NoError(t, err)
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{ID: "b", URL: "https://b.example/cb"})
	require.NoError(t, err)

	require.NoError(t, s.DeleteAll(ctx, "task-1"))
	list, err := s.List(ctx, "task-1")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestValidationErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	for name, cfg := range map[string]*a2a.PushConfig{
		"nil":           nil,
		"empty url":     {ID: "x"},
		"invalid url":   {ID: "x", URL: "not a url"},
		"relative path": {ID: "x", URL: "/callback"},
		"non-http":      {ID: "x", URL: "ftp://hook.example/cb"},
		"file scheme":   {ID: "x", URL: "file:///etc/passwd"},
		"missing host":  {ID: "x", URL: "https:///cb"},
		"unparsable":    {ID: "x", URL: "https://hook.example/%zz?sig=leak"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Save(ctx, "task-1", cfg)
			require.ErrorIs(t, err, a2a.ErrInvalidParams)
			assert.NotContains(t, err.Error(), "leak", "errors must not echo the URL")
		})
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "push.db")

	s1, err := New(ctx, path)
	require.NoError(t, err)
	saved, err := s1.Save(ctx, "task-1", &a2a.PushConfig{URL: "https://hook.example/cb", Token: "keepme"})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s2.Close()) })

	got, err := s2.Get(ctx, "task-1", saved.ID)
	require.NoError(t, err)
	assert.Equal(t, "keepme", got.Token, "config must survive a store restart")
}

func TestReturnedConfigIsCopy(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	saved, err := s.Save(ctx, "task-1", &a2a.PushConfig{ID: "c1", URL: "https://a.example/cb", Token: "orig"})
	require.NoError(t, err)
	saved.Token = "mutated" // mutating the returned value must not affect stored state

	got, err := s.Get(ctx, "task-1", "c1")
	require.NoError(t, err)
	assert.Equal(t, "orig", got.Token)
}

// TestDatabaseFilesAreOwnerOnly asserts the store keeps callback tokens and
// auth credentials in files unreadable by other users, tightening even a
// database file that already exists with permissive permissions.
func TestDatabaseFilesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "a2a.db")
	// Pre-create the file world-readable so the assertion proves New tightens
	// it rather than merely inheriting a restrictive ambient umask.
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	s, err := New(ctx, path)
	require.NoError(t, err)
	// A write materializes the WAL sidecars so we can assert their mode too.
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{URL: "https://hook.example/cb", Token: "sekret"})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) {
			continue // sidecar not present in this journal mode
		}
		require.NoError(t, err)
		assert.Equalf(t, os.FileMode(0o600), info.Mode().Perm(),
			"%q must be owner-only; group/other bits leak stored credentials", path+suffix)
	}
}

// Guard against accidental interface drift.
var _ push.ConfigStore = (*Store)(nil)
