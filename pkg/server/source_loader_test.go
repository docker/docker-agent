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

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/teamloader"
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
		synctest.Sleep(110 * time.Millisecond)

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
		synctest.Sleep(60 * time.Millisecond)

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
		synctest.Sleep(60 * time.Millisecond)

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
		synctest.Sleep(2 * time.Second)
		assert.Equal(t, 2, inner.getReadCount())

		inner.setErr(nil)
		inner.setData([]byte("recovered"))
		synctest.Sleep(15 * time.Second)
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

// encryptedMockSource is a mockSource that also carries an agent config
// envelope, like the URL source a trusted Docker /gordon-agent fetch produces.
type encryptedMockSource struct {
	*mockSource

	mu  sync.RWMutex
	enc string
}

func (m *encryptedMockSource) EncryptedConfig() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enc
}

func (m *encryptedMockSource) setEncryptedConfig(enc string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enc = enc
}

// TestSourceLoaderForwardsEncryptedConfig is the regression guard for the bug
// where API-server mode (Docker Desktop) stopped forwarding the agent config
// envelope to the models gateway: sourceLoader decorates every source, so losing
// this passthrough makes teamloader's type assertion fail and the gateway's
// prompt verification silently no-op (it fails open when no config is sent).
func TestSourceLoaderForwardsEncryptedConfig(t *testing.T) {
	t.Parallel()
	inner := &encryptedMockSource{
		mockSource: &mockSource{name: "gordon-agent", data: []byte("test data")},
		enc:        "packed-signed-config",
	}
	sl := newSourceLoader(t.Context(), inner, 0)

	// The decorator must expose the capability through the very interface
	// teamloader asserts on, not merely have a method of the same name.
	ecs, ok := config.Source(sl).(config.EncryptedConfigSource)
	require.True(t, ok, "sourceLoader must satisfy config.EncryptedConfigSource")
	assert.Equal(t, "packed-signed-config", ecs.EncryptedConfig())
}

// TestSourceLoaderEncryptedConfigFollowsRefresh pins the read-through: the
// envelope is refreshed by the same Read the refresh loop performs, so a tag
// repushed under a new signature is picked up without restarting the daemon.
func TestSourceLoaderEncryptedConfigFollowsRefresh(t *testing.T) {
	t.Parallel()
	inner := &encryptedMockSource{
		mockSource: &mockSource{name: "gordon-agent", data: []byte("test data")},
		enc:        "first-envelope",
	}
	sl := newSourceLoader(t.Context(), inner, 0)
	require.Equal(t, "first-envelope", sl.EncryptedConfig())

	inner.setEncryptedConfig("second-envelope")
	assert.Equal(t, "second-envelope", sl.EncryptedConfig())
}

// TestSourceLoaderEncryptedConfigAbsent covers the ordinary case: a file or
// bytes source has no envelope, and asking for one must not panic or invent a
// value — an empty string leaves the request body untouched.
func TestSourceLoaderEncryptedConfigAbsent(t *testing.T) {
	t.Parallel()
	inner := &mockSource{name: "local.yaml", data: []byte("test data")}
	sl := newSourceLoader(t.Context(), inner, 0)

	assert.Empty(t, sl.EncryptedConfig())
}

const envelopeTestAgent = `version: "2"
agents:
  root:
    model: gpt
    instruction: hello
models:
  gpt:
    provider: openai
    model: gpt-4o
`

// TestTeamloaderAdoptsEncryptedConfigThroughSourceLoader exercises the real
// API-server wiring end to end: teamloader discovers the config envelope by
// type-asserting the source it is handed, and in API-server mode that source is
// always a sourceLoader. It is the assertion that silently failed before the
// passthrough existed, leaving the gateway with nothing to verify.
func TestTeamloaderAdoptsEncryptedConfigThroughSourceLoader(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")
	inner := &encryptedMockSource{
		mockSource: &mockSource{name: "gordon-agent", data: []byte(envelopeTestAgent)},
		enc:        "packed-signed-config",
	}
	sl := newSourceLoader(t.Context(), inner, 0)

	res, err := teamloader.LoadWithConfig(t.Context(), sl, &config.RuntimeConfig{},
		teamloader.WithProviderRegistry(providers.NewDefaultRegistry()))
	require.NoError(t, err)
	assert.Equal(t, "packed-signed-config", res.EncryptedConfig,
		"teamloader must adopt the envelope through the sourceLoader decorator")
}
