package root

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The frontend/TCK validate the schema; these tests check the CLI and policy
// contracts that an external schema cannot know about.
func TestPublishedSandboxKit(t *testing.T) {
	t.Parallel()

	repoDir := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(repoDir, "sandbox.yaml"))
	require.NoError(t, err)
	var descriptor struct {
		Capabilities []struct {
			Type     string
			Optional bool
			Config   struct {
				Runtime struct{ Allow []string }
				APIKey  struct {
					Name         string
					ProxyManaged bool `yaml:"proxyManaged"`
					Inject       []struct{ Domain string }
				} `yaml:"apiKey"`
				Prompt   []string
				Resume   []string
				Continue []string
			}
		}
	}
	require.NoError(t, yaml.Unmarshal(raw, &descriptor))

	recipe, err := os.ReadFile(filepath.Join(repoDir, "Dockerfile"))
	require.NoError(t, err)
	_, stage, ok := strings.Cut(string(recipe), "FROM template AS kit\n")
	require.True(t, ok)
	stage, _, _ = strings.Cut(stage, "\nFROM ")
	var entrypoint []string
	for line := range strings.SplitSeq(stage, "\n") {
		if argv, ok := strings.CutPrefix(line, "ENTRYPOINT "); ok {
			require.NoError(t, json.Unmarshal([]byte(argv), &entrypoint))
		}
	}
	require.GreaterOrEqual(t, len(entrypoint), 2)
	require.Equal(t, []string{"docker-agent", "run"}, entrypoint[:2])

	var allowed []string
	for _, capability := range descriptor.Capabilities {
		if capability.Type == "com.docker.sandbox/network-policy@1" {
			allowed = capability.Config.Runtime.Allow
		}
	}
	require.NotEmpty(t, allowed)
	assert.Contains(t, allowed, "models.dev:443")
	var credentials []string
	for _, capability := range descriptor.Capabilities {
		switch capability.Type {
		case "com.docker.sandbox/credential@1":
			assert.True(t, capability.Optional)
			assert.True(t, capability.Config.APIKey.ProxyManaged)
			credentials = append(credentials, capability.Config.APIKey.Name)
			for _, inject := range capability.Config.APIKey.Inject {
				assert.Contains(t, allowed, inject.Domain+":443", "credential destination must be allowed")
			}
		case "com.docker.sandbox/agent-sessions@1", "com.docker.sandbox/agent-interactive-sessions@1":
			headless := capability.Type == "com.docker.sandbox/agent-sessions@1"
			for name, tail := range map[string][]string{
				"prompt": capability.Config.Prompt, "resume": capability.Config.Resume, "continue": capability.Config.Continue,
			} {
				t.Run(capability.Type+"/"+name, func(t *testing.T) {
					if len(tail) == 0 {
						return
					}
					args := slices.Clone(entrypoint[2:])
					for _, arg := range tail {
						arg = strings.ReplaceAll(arg, "{{.Prompt}}", "--untrusted-prompt")
						args = append(args, strings.ReplaceAll(arg, "{{.SessionID}}", "session-id"))
					}
					cmd := newRunCmd()
					require.NoError(t, cmd.ParseFlags(args))
					exec, err := cmd.Flags().GetBool("exec")
					require.NoError(t, err)
					assert.Equal(t, headless, exec)
					if name == "prompt" {
						assert.Equal(t, []string{"default", "--untrusted-prompt"}, cmd.Flags().Args())
					} else {
						session, err := cmd.Flags().GetString("session")
						require.NoError(t, err)
						want := "session-id"
						if name == "continue" {
							want = "-1"
						}
						assert.Equal(t, want, session)
					}
				})
			}
		}
	}
	assert.ElementsMatch(t, []string{
		"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "GOOGLE_API_KEY", "MISTRAL_API_KEY",
		"NEBIUS_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "XAI_API_KEY",
	}, credentials)
}
