package chatserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimePool_DisabledIsNotCached(t *testing.T) {
	t.Parallel()
	p := newRuntimePool(t.Context(), nil, 0)

	// Put with maxIdle=0 must be a no-op (we don't have a runtime to put,
	// but the channel-for behaviour itself shouldn't allocate).
	p.Put("root", nil)
	assert.Empty(t, p.idle, "no per-agent channels should be allocated when pooling is disabled")
}

func TestRuntimePool_NegativeCapTreatedAsZero(t *testing.T) {
	t.Parallel()
	p := newRuntimePool(t.Context(), nil, -1)
	assert.Equal(t, 0, p.maxIdle)
}

func TestRuntimePoolReusesRunner(t *testing.T) {
	t.Parallel()
	p := newRuntimePool(t.Context(), nil, 1)
	first, second := &stubRunner{}, &stubRunner{}
	p.Put("root", first)
	p.Put("root", second)

	runner, err := p.Get("root")
	require.NoError(t, err)
	assert.Same(t, first, runner, "a full pool must retain the first runner")
	assert.Nil(t, p.takeIdle("root"), "acquired runners must leave the pool")
	assert.Nil(t, p.takeIdle("other"), "runners must remain agent-scoped")
}

func TestRuntimePool_takeIdleNoChannel(t *testing.T) {
	t.Parallel()
	p := newRuntimePool(t.Context(), nil, 4)
	assert.Nil(t, p.takeIdle("anything"))
}
