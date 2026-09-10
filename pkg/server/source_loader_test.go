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

		// Read should return cached data immediately
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
		time.Sleep(110 * time.Millisecond) //nolint:forbidigo // fake time inside a synctest bubble; returns instantly
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

	// Initial load failed
	assert.Equal(t, 1, inner.getReadCount())

	// Read should return the error from initial load
	data, err := sl.Read(ctx)
	require.Error(t, err)
	assert.Equal(t, expectedErr, err)
	assert.Nil(t, data)
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
		time.Sleep(60 * time.Millisecond) //nolint:forbidigo // fake time inside a synctest bubble; returns instantly
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

	initialReadCount := inner.getReadCount()

	// Multiple reads with zero refresh interval
	for range 10 {
		data, err := sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("test data"), data)
	}

	// All reads should return cached data from startup
	assert.Equal(t, initialReadCount, inner.getReadCount())
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
		time.Sleep(60 * time.Millisecond) //nolint:forbidigo // fake time inside a synctest bubble; returns instantly
		synctest.Wait()

		// Should still return old cached data despite refresh error
		data, err = sl.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("initial data"), data)
	})
}

func TestSourceLoaderRetriesFailedStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &mockSource{name: "test.yaml", err: errors.New("unavailable")}
		sl := newSourceLoader(t.Context(), inner, 0)

		synctest.Wait()
		time.Sleep(2 * time.Second) //nolint:forbidigo // fake time inside a synctest bubble
		synctest.Wait()
		assert.Equal(t, 2, inner.getReadCount())

		inner.setErr(nil)
		inner.setData([]byte("recovered"))
		time.Sleep(15 * time.Second) //nolint:forbidigo // fake time inside a synctest bubble
		synctest.Wait()
		assert.Equal(t, 3, inner.getReadCount())

		data, err := sl.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []byte("recovered"), data)
	})
}

func TestSourceRetryScheduleOutlivesDesktopDetectionCache(t *testing.T) {
	var total time.Duration
	for _, delay := range sourceRetrySchedule {
		total += delay
	}
	assert.Greater(t, total, time.Minute)
}
