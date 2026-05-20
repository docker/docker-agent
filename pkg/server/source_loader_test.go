package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockSource struct {
	name      string
	parentDir string
	mu        sync.RWMutex
	data      []byte
	err       error
	readCount int
}

func (m *mockSource) Name() string {
	return m.name
}

func (m *mockSource) ParentDir() string {
	return m.parentDir
}

func (m *mockSource) Read(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readCount++
	if m.err != nil {
		return nil, m.err
	}
	return m.data, nil
}

func (m *mockSource) setData(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = data
}

func (m *mockSource) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *mockSource) getReadCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.readCount
}

func TestSourceLoader_Read_WithRefreshInterval_BeforeExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := &mockSource{
			name: "test.yaml",
			data: []byte("test data"),
		}
		ctx := t.Context()
		refreshInterval := 100 * time.Millisecond
		sl := newSourceLoader(ctx, inner, refreshInterval)

		// Read blocks until the asynchronous initial load completes, then
		// returns the cached data.
		data, err := sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("test data"), data)
		assert.Equal(t, 1, inner.getReadCount()) // No additional read

		// Immediate second read - should return cached data
		data, err = sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("test data"), data)
		assert.Equal(t, 1, inner.getReadCount())
	})
}

func TestSourceLoader_Read_WithRefreshInterval_AfterExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := &mockSource{
			name: "test.yaml",
			data: []byte("test data"),
		}
		ctx := t.Context()
		refreshInterval := 100 * time.Millisecond
		sl := newSourceLoader(ctx, inner, refreshInterval)

		synctest.Wait()
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()

		// Read should refresh
		data, err := sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("test data"), data)
		assert.Equal(t, 2, inner.getReadCount())
	})
}

func TestSourceLoader_Read_Error(t *testing.T) {
	t.Parallel()
	expectedErr := errors.New("read error")
	inner := &mockSource{
		name: "test.yaml",
		err:  expectedErr,
	}
	ctx := t.Context()
	sl := newSourceLoader(ctx, inner, 0)

	// Read blocks on the initial load and returns its error.
	data, err := sl.Read(ctx)
	require.Error(t, err)
	assert.Equal(t, expectedErr, err)
	assert.Nil(t, data)
	assert.Equal(t, 1, inner.getReadCount())
}

func TestSourceLoader_Read_DataChanges(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := &mockSource{
			name: "test.yaml",
			data: []byte("initial data"),
		}
		ctx := t.Context()
		refreshInterval := 50 * time.Millisecond
		sl := newSourceLoader(ctx, inner, refreshInterval)

		// First read gets initial data
		data, err := sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("initial data"), data)

		// Change the data in the mock
		inner.setData([]byte("updated data"))

		// Immediate read still gets old cached data
		data, err = sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("initial data"), data)

		synctest.Wait()
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()

		// Read after interval should get updated data from background refresh
		data, err = sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("updated data"), data)
	})
}

func TestSourceLoader_Read_ZeroRefreshInterval(t *testing.T) {
	t.Parallel()
	inner := &mockSource{
		name: "test.yaml",
		data: []byte("test data"),
	}
	ctx := t.Context()
	sl := newSourceLoader(ctx, inner, 0)

	// First Read blocks on and then drives the single initial load.
	data, err := sl.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("test data"), data)
	require.Equal(t, 1, inner.getReadCount())

	// Subsequent reads must not trigger any further Source.Read calls.
	for range 10 {
		data, err := sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("test data"), data)
	}
	assert.Equal(t, 1, inner.getReadCount())
}

func TestSourceLoader_SuccessThenError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := &mockSource{
			name: "test.yaml",
			data: []byte("initial data"),
		}
		ctx := t.Context()
		refreshInterval := 50 * time.Millisecond
		sl := newSourceLoader(ctx, inner, refreshInterval)

		// Initial read succeeds
		data, err := sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("initial data"), data)

		// Introduce error
		inner.setErr(errors.New("refresh error"))

		synctest.Wait()
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()

		// Should still return old cached data despite refresh error
		data, err = sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("initial data"), data)
	})
}

// slowSource blocks Read until the test releases it. It models a slow OCI
// pull or HTTPS GET that would previously have blocked NewSessionManager.
type slowSource struct {
	name    string
	release chan struct{}
	data    []byte
}

func (s *slowSource) Name() string      { return s.name }
func (s *slowSource) ParentDir() string { return "" }

func (s *slowSource) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-s.release:
		return s.data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestSourceLoader_ConstructorReturnsBeforeInitialLoad is the regression test
// for the docker-agent-as-AI-backend startup hang: NewSourceLoader (and
// therefore NewSessionManager and server.New) must return immediately, even
// if the underlying Source.Read is slow. Endpoints that don't need source
// data (notably /api/ping) can then start serving while the load is still
// in flight.
func TestSourceLoader_ConstructorReturnsBeforeInitialLoad(t *testing.T) {
	t.Parallel()
	inner := &slowSource{
		name:    "slow.yaml",
		release: make(chan struct{}),
		data:    []byte("loaded data"),
	}
	ctx := t.Context()

	constructorDone := make(chan struct{})
	var sl *sourceLoader
	go func() {
		sl = newSourceLoader(ctx, inner, 0)
		close(constructorDone)
	}()

	select {
	case <-constructorDone:
	case <-time.After(time.Second):
		t.Fatal("newSourceLoader did not return while the source was still loading")
	}

	// Read should block until we release the underlying source.
	readDone := make(chan struct {
		data []byte
		err  error
	})
	go func() {
		data, err := sl.Read(ctx)
		readDone <- struct {
			data []byte
			err  error
		}{data, err}
	}()

	select {
	case <-readDone:
		t.Fatal("Read returned before the initial load completed")
	case <-time.After(50 * time.Millisecond):
		// Expected: Read is waiting on sl.ready
	}

	close(inner.release)

	select {
	case result := <-readDone:
		require.NoError(t, result.err)
		assert.Equal(t, []byte("loaded data"), result.data)
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after the initial load completed")
	}
}

// TestSourceLoader_ReadCancelledByContext verifies that a caller of Read
// can give up if the initial load is taking too long, without leaking the
// loader's background goroutine.
func TestSourceLoader_ReadCancelledByContext(t *testing.T) {
	t.Parallel()
	inner := &slowSource{
		name:    "slow.yaml",
		release: make(chan struct{}),
		data:    []byte("loaded data"),
	}
	loaderCtx, cancelLoader := context.WithCancel(t.Context())
	defer cancelLoader()
	sl := newSourceLoader(loaderCtx, inner, 0)

	readCtx, cancelRead := context.WithCancel(t.Context())
	cancelRead()

	data, err := sl.Read(readCtx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, data)

	// Let the loader's goroutine finish so we don't leak it across tests.
	close(inner.release)
}
