//go:build !js

package embeddedchat

import (
	"testing"

	"github.com/stretchr/testify/require"

	dagentcfg "github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/embeddedchat/defaults"
)

// The default registries pull in sqlite-backed toolsets that do not build for
// js/wasm; the rest of the package's tests stay portable.
func TestNewLoadsAgentAndWelcomeMessage(t *testing.T) {
	t.Parallel()
	cfg := []byte(`agents:
  root:
    description: Test agent
    instruction: Be helpful.
    welcome_message: Hello from embedded chat.
    harness:
      type: claude-code
`)

	s, err := New(t.Context(), Config{
		AgentSource: dagentcfg.NewBytesSource("agent.yaml", cfg),
		LoadOpts:    defaults.Opts(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "Hello from embedded chat.", s.WelcomeMessage())
	require.NotNil(t, s.Runtime())
	require.NotNil(t, s.Conversation())
}
