package rag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

type countingBackend struct {
	stubBackend

	inits, watches, closes atomic.Int32
	initErr                error
}

func newCountingBackend() *countingBackend {
	b := &countingBackend{}
	b.events = make(chan ragtypes.Event, 8)
	b.initialize = func(context.Context) error {
		b.inits.Add(1)
		return b.initErr
	}
	b.watch = func(context.Context) error {
		b.watches.Add(1)
		return nil
	}
	b.close = func() error {
		b.closes.Add(1)
		return nil
	}
	return b
}

func newSharedPair(t *testing.T, reg *sharedRegistry, keyA, keyB sharedKey) (*sharedToolSet, *sharedToolSet, *countingBackend, *countingBackend) {
	t.Helper()
	backendA, backendB := newCountingBackend(), newCountingBackend()
	return newSharedToolSet(reg, keyA, New(backendA, "knowledge")),
		newSharedToolSet(reg, keyB, New(backendB, "knowledge")),
		backendA, backendB
}

func TestSharedToolSet_IdenticalKeysIndexOnce(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	key := sharedKey{config: "same"}
	a, b, backendA, backendB := newSharedPair(t, reg, key, key)

	require.NoError(t, a.Start(t.Context()))
	require.NoError(t, b.Start(t.Context()))

	assert.Equal(t, int32(1), backendA.inits.Load()+backendB.inits.Load(), "one index for both references")
	assert.Equal(t, int32(0), backendB.inits.Load(), "the second reference attaches to the first")

	require.NoError(t, a.Stop(t.Context()))
	assert.Equal(t, int32(0), backendA.closes.Load(), "the shared index outlives the first release")

	require.NoError(t, b.Stop(t.Context()))
	assert.Equal(t, int32(1), backendA.closes.Load(), "the last release closes the shared index")
	assert.Equal(t, int32(1), backendB.closes.Load(), "an unused per-reference manager is closed on Stop")
	assert.Empty(t, reg.entries, "released entries are removed")
}

func TestSharedToolSet_DifferentKeysDoNotShare(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	a, b, backendA, backendB := newSharedPair(t, reg, sharedKey{config: "one"}, sharedKey{config: "two"})

	require.NoError(t, a.Start(t.Context()))
	require.NoError(t, b.Start(t.Context()))
	defer func() {
		_ = a.Stop(t.Context())
		_ = b.Stop(t.Context())
	}()

	assert.Equal(t, int32(1), backendA.inits.Load())
	assert.Equal(t, int32(1), backendB.inits.Load())
}

func TestSharedToolSet_UnshareableKeyNeverRegisters(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	a, b, backendA, backendB := newSharedPair(t, reg, sharedKey{}, sharedKey{})

	require.NoError(t, a.Start(t.Context()))
	require.NoError(t, b.Start(t.Context()))

	assert.Equal(t, int32(1), backendA.inits.Load())
	assert.Equal(t, int32(1), backendB.inits.Load())
	assert.Empty(t, reg.entries)

	require.NoError(t, a.Stop(t.Context()))
	require.NoError(t, b.Stop(t.Context()))
	assert.Equal(t, int32(1), backendA.closes.Load())
	assert.Equal(t, int32(1), backendB.closes.Load())
}

func TestSharedToolSet_FailedStartIsRetriedByLaterReference(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	key := sharedKey{config: "same"}
	a, b, backendA, backendB := newSharedPair(t, reg, key, key)
	backendA.initErr = errors.New("embedding endpoint down")

	require.Error(t, a.Start(t.Context()))
	assert.Empty(t, reg.entries, "a failed start must not leave an entry behind")

	require.NoError(t, b.Start(t.Context()))
	assert.Equal(t, int32(1), backendB.inits.Load())
	require.NoError(t, b.Stop(t.Context()))
	assert.Equal(t, int32(1), backendB.closes.Load())
}

func TestSharedToolSet_ConcurrentStartsIndexOnce(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	key := sharedKey{config: "same"}

	const n = 16
	backends := make([]*countingBackend, n)
	handles := make([]*sharedToolSet, n)
	for i := range n {
		backends[i] = newCountingBackend()
		handles[i] = newSharedToolSet(reg, key, New(backends[i], "knowledge"))
	}

	var wg sync.WaitGroup
	for _, h := range handles {
		wg.Go(func() { assert.NoError(t, h.Start(t.Context())) })
	}
	wg.Wait()

	var inits int32
	for _, b := range backends {
		inits += b.inits.Load()
	}
	assert.Equal(t, int32(1), inits)

	for _, h := range handles {
		require.NoError(t, h.Stop(t.Context()))
	}
	assert.Empty(t, reg.entries)
}

func TestSharedToolSet_EventsReachEveryReference(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	key := sharedKey{config: "same"}
	a, b, backendA, _ := newSharedPair(t, reg, key, key)

	gotA, gotB := make(chan ragtypes.Event, 1), make(chan ragtypes.Event, 1)
	a.SetEventCallback(func(ev ragtypes.Event) { gotA <- ev })
	unsub := b.SubscribeEvents(func(ev ragtypes.Event) { gotB <- ev })
	defer unsub()

	require.NoError(t, a.Start(t.Context()))
	require.NoError(t, b.Start(t.Context()))
	defer func() {
		_ = a.Stop(t.Context())
		_ = b.Stop(t.Context())
	}()

	backendA.events <- ragtypes.Event{Type: ragtypes.EventTypeIndexingStarted}
	assert.Equal(t, ragtypes.EventTypeIndexingStarted, receive(t, gotA).Type)
	assert.Equal(t, ragtypes.EventTypeIndexingStarted, receive(t, gotB).Type)
}

func receive(t *testing.T, ch <-chan ragtypes.Event) ragtypes.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered")
		return ragtypes.Event{}
	}
}

func TestSharedToolSet_QueriesUseSharedIndex(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	key := sharedKey{config: "same"}
	a, b, backendA, backendB := newSharedPair(t, reg, key, key)

	require.NoError(t, a.Start(t.Context()))
	require.NoError(t, b.Start(t.Context()))
	defer func() {
		_ = a.Stop(t.Context())
		_ = b.Stop(t.Context())
	}()

	toolsB, err := b.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolsB, 1)
	_, err = toolsB[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"query":"hello"}`}}, nil)
	require.NoError(t, err)

	assert.Equal(t, "hello", backendA.query, "queries must hit the index that was initialized")
	assert.Empty(t, backendB.query)
}

func TestSharedKeyFor(t *testing.T) {
	t.Parallel()
	ragCfg := &latest.RAGConfig{Docs: []string{"./docs"}}
	newRC := func(env environment.Provider) *config.RuntimeConfig {
		return &config.RuntimeConfig{EnvProviderOverride: env}
	}
	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "a"})
	rc := newRC(env)
	dir := t.TempDir()

	k1, ok := sharedKeyFor("knowledge", ragCfg, dir, rc)
	require.True(t, ok)
	k2, ok := sharedKeyFor("knowledge", &latest.RAGConfig{Docs: []string{"./docs"}}, dir, newRC(env))
	require.True(t, ok)
	assert.True(t, sameKey(k1, k2), "identical inputs share")

	other, _ := sharedKeyFor("knowledge", ragCfg, dir, newRC(environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "a"})))
	assert.False(t, sameKey(k1, other), "a different credential source never shares, even with equal values")

	renamed, _ := sharedKeyFor("other", ragCfg, dir, rc)
	assert.False(t, sameKey(k1, renamed))

	otherDir, _ := sharedKeyFor("knowledge", ragCfg, t.TempDir(), rc)
	assert.False(t, sameKey(k1, otherDir))

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	viaLink, _ := sharedKeyFor("knowledge", ragCfg, link, rc)
	assert.True(t, sameKey(k1, viaLink), "a symlinked parent dir resolves to the same key")

	_, ok = sharedKeyFor("knowledge", ragCfg, dir, newRC(uncomparableEnv{}))
	assert.False(t, ok, "an env provider without identity disables sharing")
}

// sameKey compares like the registry map does; assert.Equal would follow the
// env provider pointer and call distinct providers with equal values equal.
func sameKey(a, b sharedKey) bool { return a == b }

type uncomparableEnv struct{ _ []string }

func (uncomparableEnv) Get(context.Context, string) (string, bool) { return "", false }

func TestSharedToolSet_ToolsDoesNotWaitForIndexing(t *testing.T) {
	t.Parallel()
	backend := newCountingBackend()
	release := make(chan struct{})
	backend.initialize = func(context.Context) error {
		backend.inits.Add(1)
		<-release
		return nil
	}
	s := newSharedToolSet(newSharedRegistry(), sharedKey{config: "same"}, New(backend, "knowledge"))

	started := make(chan error, 1)
	go func() { started <- s.Start(t.Context()) }()
	require.Eventually(t, func() bool { return backend.inits.Load() == 1 }, 5*time.Second, time.Millisecond)

	listed := make(chan struct{})
	go func() {
		_, _ = s.Tools(t.Context())
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("Tools blocked while indexing")
	}

	close(release)
	require.NoError(t, <-started)
	require.NoError(t, s.Stop(t.Context()))
}

func TestSharedToolSet_BackgroundWorkCarriesNoSession(t *testing.T) {
	t.Parallel()
	backend := newCountingBackend()
	watchCtx := make(chan context.Context, 1)
	backend.watch = func(ctx context.Context) error {
		watchCtx <- ctx
		return nil
	}
	s := newSharedToolSet(newSharedRegistry(), sharedKey{config: "same"}, New(backend, "knowledge"))

	require.NoError(t, s.Start(httpclient.ContextWithSessionID(t.Context(), "session-a")))
	defer func() { _ = s.Stop(t.Context()) }()

	select {
	case ctx := <-watchCtx:
		assert.Empty(t, httpclient.SessionIDFromContext(ctx), "a shared index must not act on behalf of the session that started it")
	case <-time.After(5 * time.Second):
		t.Fatal("file watcher not started")
	}
}
