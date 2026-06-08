package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

// TestInjectMemoriesBuiltin_ReturnsNilWhenNoopStub verifies that the scaffold
// implementation is a no-op: it returns (nil, nil) so the hook pipeline
// treats it as contributing no additional context.
func TestInjectMemoriesBuiltin_ReturnsNilWhenNoopStub(t *testing.T) {
	t.Parallel()

	rt := &LocalRuntime{}
	out, err := rt.injectMemoriesBuiltin(context.Background(), &hooks.Input{AgentName: "a"}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
