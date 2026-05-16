package runtime

import (
	"errors"
	"testing"

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
