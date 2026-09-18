package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

func TestSQLiteSessionStore_CloseWaitsForInFlightConnection(t *testing.T) {
	t.Parallel()

	db, err := sqliteutil.OpenDB(t.Context(), filepath.Join(t.TempDir(), "close.db"))
	require.NoError(t, err)
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)

	// Check out the single pooled connection and keep it busy until released.
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	released := make(chan struct{})
	go func() {
		<-released
		_ = conn.Close()
	}()

	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()

	select {
	case <-closed:
		t.Fatal("Close returned while a connection was still checked out")
	case <-time.After(100 * time.Millisecond):
	}

	close(released)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(closeDrainTimeout):
		t.Fatal("Close did not return after the connection was released")
	}
	assert.Zero(t, db.Stats().OpenConnections)
}
