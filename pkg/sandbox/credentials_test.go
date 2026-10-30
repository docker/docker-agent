package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestEnvForSourceSkipsGitHubCredentials(t *testing.T) {
	t.Parallel()
	for _, tokenName := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Run(tokenName, func(t *testing.T) {
			t.Parallel()
			source := config.NewBytesSource("agent.yaml", []byte(`agents:
  root:
    model: github-copilot/gpt-4.1
`))
			env := environment.NewMapEnvProvider(map[string]string{tokenName: "host-secret"})
			names, err := gatherSourceEnvVars(t.Context(), source, env, nil)
			require.NoError(t, err)
			require.Contains(t, names, tokenName)
			flags, values := EnvForSource(t.Context(), source, env, nil)
			assert.Empty(t, flags)
			assert.Empty(t, values)
		})
	}
}
