package evaluation

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestContainerEnvironmentIsExplicit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake container runtime is a POSIX shell script")
	}
	t.Parallel()

	const script = `#!/bin/sh
printf '%s\n' "$@" > "${0%/*}/args"
printf '%s' "${OPENAI_API_KEY-}" > "${0%/*}/token"
printf '%s' "${LITERAL-}" > "${0%/*}/literal"
echo '{"type":"agent_choice","content":"hello"}'
`
	for _, gateway := range []string{"", "https://gateway.example"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("gateway=%s/explicit=%t", gateway, explicit), func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				fakeRuntime := filepath.Join(dir, "runtime")
				writeMockCLI(t, fakeRuntime, script)
				runConfig := &config.RuntimeConfig{
					Config: config.Config{ModelsGateway: gateway},
					EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{
						"OPENAI_API_KEY":                  "model-secret",
						"DOCKER_AGENT_MODELS_GATEWAY":     "https://gateway.example",
						environment.DockerDesktopTokenEnv: "docker-secret",
						"JUDGE_KEY":                       "judge-secret",
					}),
				}
				cfg := Config{ContainerRuntime: fakeRuntime}
				if explicit {
					cfg.EnvVars = []string{"OPENAI_API_KEY", "LITERAL=explicit-value", "DOCKER_AGENT_MODELS_GATEWAY", environment.DockerDesktopTokenEnv, "JUDGE_KEY"}
				}
				runner := newRunner(config.NewFileSource(filepath.Join(dir, "agent.yaml")), runConfig, cfg)
				runner.agentConfig = &latest.Config{Evaluators: map[string]latest.EvaluatorConfig{
					"check": {Provider: "openai", Model: "gpt-4o", TokenKey: "JUDGE_KEY"},
				}}
				_, err := runner.runDockerAgentInContainer(t.Context(), "image", []string{"hello"}, "")
				require.NoError(t, err)
				args, err := os.ReadFile(filepath.Join(dir, "args"))
				require.NoError(t, err)
				token, err := os.ReadFile(filepath.Join(dir, "token"))
				require.NoError(t, err)
				literal, err := os.ReadFile(filepath.Join(dir, "literal"))
				require.NoError(t, err)
				if explicit {
					for _, name := range []string{"OPENAI_API_KEY", "LITERAL", "DOCKER_AGENT_MODELS_GATEWAY", environment.DockerDesktopTokenEnv, "JUDGE_KEY"} {
						assert.Contains(t, strings.Split(string(args), "\n"), name)
					}
					assert.Equal(t, "model-secret", string(token))
					assert.Equal(t, "explicit-value", string(literal))
				} else {
					assert.NotContains(t, strings.Split(string(args), "\n"), "-e")
					assert.Empty(t, literal)
				}
			})
		}
	}
}
