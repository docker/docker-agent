package chatserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimePool_DisabledIsNotCached(t *testing.T) {
	p := newRuntimePool(nil, 0)

	// Put with maxIdle=0 must be a no-op (we don't have a runtime to put,
	// but the channel-for behaviour itself shouldn't allocate).
	p.Put("root", nil)
	assert.Empty(t, p.idle, "no per-agent channels should be allocated when pooling is disabled")
}

func TestRuntimePool_NegativeCapTreatedAsZero(t *testing.T) {
	p := newRuntimePool(nil, -1)
	assert.Equal(t, 0, p.maxIdle)
}

func TestRuntimePool_takeIdleNoChannel(t *testing.T) {
	p := newRuntimePool(nil, 4)
	assert.Nil(t, p.takeIdle("anything"))
}
