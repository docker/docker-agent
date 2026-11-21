//go:build !docker_agent_no_js_expansion

package js

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/types"
)

// TestExpandCommandsPreservesAgent verifies the agent-switch target survives
// command expansion. Dropping it makes agent-only slash commands silently run
// on the root agent instead of handing off to the named sub-agent.
func TestExpandCommandsPreservesAgent(t *testing.T) {
	t.Parallel()

	env := testEnvProvider(map[string]string{})
	expander := NewJsExpander(&env)

	expanded := expander.ExpandCommands(t.Context(), types.Commands{
		"ask": {
			Description: "Hand off to the specialist",
			Agent:       "specialist",
		},
	})

	assert.Equal(t, "specialist", expanded["ask"].Agent)
}
