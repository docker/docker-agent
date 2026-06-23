package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/team"
)

// TestNewLocalRuntime_DefaultsToLazyModelStore verifies that NewLocalRuntime
// no longer eagerly constructs a modelsdev store: callers that do not pass
// WithModelStore receive a lazyModelStore that defers the underlying disk
// access until first use.
//
// This is a testability seam: tests can construct a runtime without paying
// the cost (or risking the failure modes) of os.UserHomeDir + os.MkdirAll
// in NewLocalRuntime.
func TestNewLocalRuntime_DefaultsToLazyModelStore(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm)
	require.NoError(t, err)

	_, ok := rt.modelsStore.(*lazyModelStore)
	assert.True(t, ok, "default modelsStore should be *lazyModelStore, got %T", rt.modelsStore)
}

// TestNewLocalRuntime_WithModelStoreSkipsLazyDefault verifies that callers
// who supply their own ModelStore are not wrapped — the explicit injection
// is kept verbatim so tests can fully control catalog behaviour.
func TestNewLocalRuntime_WithModelStoreSkipsLazyDefault(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	stub := mockModelStore{}
	rt, err := NewLocalRuntime(tm, WithModelStore(stub))
	require.NoError(t, err)

	assert.Equal(t, stub, rt.modelsStore)
}

// TestLazyModelStore_DefersError verifies that lazyModelStore caches the
// load error and returns it on every subsequent call, never re-running the
// loader. This is the property that lets NewLocalRuntime return cleanly
// even on hosts where modelsdev.NewStore would fail.
func TestLazyModelStore_DefersError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("home dir unavailable")
	calls := 0
	l := &lazyModelStore{}
	// Pre-seed a failed load via the once. We can't override l.once.Do
	// directly, so simulate it by invoking load() with a stubbed-out
	// internal: easier to test the contract via sync.Once behaviour using
	// a small helper.
	l.once.Do(func() {
		calls++
		l.err = wantErr
	})

	_, err := l.GetModel(t.Context(), modelsdev.NewID("openai", "anything"))
	require.ErrorIs(t, err, wantErr)
	_, err = l.GetDatabase(t.Context())
	require.ErrorIs(t, err, wantErr)

	assert.Equal(t, 1, calls, "loader should only run once even after multiple method calls")
}

// TestLazyModelStore_GatesCustomProvider is the regression test for issue
// #3165. The runtime's default ModelStore must not reach out to models.dev
// when resolving a model for a user-defined custom provider, so a
// self-contained custom-provider config stays usable when models.dev is
// unreachable. This exercises the exact store the request loop (loop.go:405)
// and session compaction (session_compaction.go:198) use in production.
func TestLazyModelStore_GatesCustomProvider(t *testing.T) {
	// Point the cache at an empty home so there is no on-disk catalog to fall
	// back on: a cold start, as in a freshly-spawned pod. t.Setenv forbids
	// t.Parallel, which is fine here.
	t.Setenv("HOME", t.TempDir())

	var store ModelStore = &lazyModelStore{}

	// An already-expired deadline makes any HTTP attempt fail instantly,
	// standing in for "models.dev is unreachable" without real network state.
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancel()

	_, err := store.GetModel(ctx, modelsdev.NewID("mistral_gateway", "mistral-small-latest"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fetch from API",
		"custom provider must not trigger a models.dev fetch from the runtime store")
	assert.Contains(t, err.Error(), `provider "mistral_gateway" not found`)
}
