package evaluation

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestStageJudgeProviderLoadsInContainer(t *testing.T) {
	t.Parallel()

	const agentYAML = `agents:
  root:
    model: openai/gpt-4o
evaluators:
  relevance:
    provider: corporate
    model: jev-latest
    type: boolean
    instructions: Does transcript satisfy criterion?
`
	global := latest.ProviderConfig{
		Provider: "typesafe", BaseURL: "https://judge.example.com", TokenKey: "HOST_JUDGE_KEY",
		ProviderOpts: map[string]any{"secret": "must-not-forward"},
	}
	agentConfig, err := config.Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(agentYAML)))
	require.NoError(t, err)
	runner := newRunner(nil, &config.RuntimeConfig{Config: config.Config{
		Providers: map[string]latest.ProviderConfig{
			"corporate": global,
			"unrelated": {Provider: "openai", BaseURL: "https://unrelated.example.com"},
		},
	}}, Config{JudgeType: JudgeTypeEvaluator, JudgeModel: "relevance"})
	runner.agentConfig = agentConfig
	dir, err := runner.stageJudgeProvider()
	require.NoError(t, err)
	require.NotEmpty(t, dir)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "must-not-forward")
	assert.NotContains(t, string(data), "unrelated")
	var userConfig userconfig.Config
	require.NoError(t, yaml.Unmarshal(data, &userConfig))
	require.Len(t, userConfig.Providers, 1)
	assert.Equal(t, latest.ProviderConfig{Provider: global.Provider, BaseURL: global.BaseURL, TokenKey: global.TokenKey}, userConfig.Providers["corporate"])
	assert.Nil(t, userConfig.Settings)
	assert.Nil(t, userConfig.Aliases)
	assert.Nil(t, agentConfig.Providers, "staging must not change the source configuration")

	// Match the container CLI: only chat credentials are present.
	containerConfig := &config.RuntimeConfig{
		Config:                 config.Config{Providers: userConfig.GetProviders()},
		EnvProviderOverride:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "chat-key"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot()),
	}
	loaded, err := teamloader.LoadWithConfig(t.Context(), config.NewBytesSource("agent.yaml", []byte(agentYAML)), containerConfig,
		teamloader.WithProviderRegistry(providers.NewDefaultRegistry()))
	require.NoError(t, err)
	_, ok := loaded.Team.Evaluator("relevance")
	assert.True(t, ok)
}

func TestStageJudgeProviderSkipsUnneededConfig(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name, kind, model string
		agent             *latest.Config
	}{
		{"LLM", JudgeTypeLLM, "relevance", &latest.Config{}},
		{"no agent", JudgeTypeEvaluator, "relevance", nil},
		{"inline model", JudgeTypeEvaluator, "typesafe/jev-latest", &latest.Config{}},
		{"agent provider", JudgeTypeEvaluator, "relevance", &latest.Config{
			Evaluators: map[string]latest.EvaluatorConfig{"relevance": {Provider: "corporate"}},
			Providers:  map[string]latest.ProviderConfig{"corporate": {Provider: "typesafe"}},
		}},
		{"no global provider", JudgeTypeEvaluator, "relevance", &latest.Config{
			Evaluators: map[string]latest.EvaluatorConfig{"relevance": {Provider: "typesafe"}},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runner := newRunner(nil, &config.RuntimeConfig{}, Config{JudgeType: tt.kind, JudgeModel: tt.model})
			runner.agentConfig = tt.agent
			dir, err := runner.stageJudgeProvider()
			require.NoError(t, err)
			assert.Empty(t, dir)
		})
	}
}

func TestContainerMountsJudgeProviderWithoutCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake container runtime is a POSIX shell script")
	}
	const script = `#!/bin/sh
printf '%s\n' "$@" > "${0%/*}/args"
for arg in "$@"; do
  case "$arg" in
    *:/judge-config:ro)
      config_dir=${arg%:/judge-config:ro}
      printf '%s' "$config_dir" > "${0%/*}/config-dir"
      cat "$config_dir/config.yaml" > "${0%/*}/staged.yaml"
      ;;
  esac
done
printf '%s' "${HOST_JUDGE_KEY-}" > "${0%/*}/host-key"
echo '{"type":"agent_choice","content":"hello"}'
`
	t.Parallel()

	for _, setup := range []string{"", "echo setup"} {
		t.Run(setup, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fakeRuntime := filepath.Join(dir, "runtime")
			writeMockCLI(t, fakeRuntime, script)
			runner := newRunner(config.NewFileSource(filepath.Join(dir, "agent.yaml")), &config.RuntimeConfig{
				Config:              config.Config{Providers: map[string]latest.ProviderConfig{"corporate": {Provider: "typesafe", TokenKey: "HOST_JUDGE_KEY"}}},
				EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"HOST_JUDGE_KEY": "secret-value"}),
			}, Config{JudgeType: JudgeTypeEvaluator, JudgeModel: "relevance", ContainerRuntime: fakeRuntime})
			runner.agentConfig = &latest.Config{Evaluators: map[string]latest.EvaluatorConfig{"relevance": {
				Provider: "corporate", Model: "jev-latest", Type: "boolean", Instructions: relevanceEvaluatorInstructions,
			}}}
			_, err := runner.runDockerAgentInContainer(t.Context(), "image", []string{"hello"}, setup)
			require.NoError(t, err)
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			require.NoError(t, err)
			assert.Contains(t, string(args), "--config-dir\n/judge-config\n--\n/configs/agent.yaml")
			assert.NotContains(t, string(args), "secret-value")
			assert.NotContains(t, string(args), "-e\nHOST_JUDGE_KEY")
			key, err := os.ReadFile(filepath.Join(dir, "host-key"))
			require.NoError(t, err)
			assert.Empty(t, key)
			staged, err := os.ReadFile(filepath.Join(dir, "staged.yaml"))
			require.NoError(t, err)
			assert.Contains(t, string(staged), "HOST_JUDGE_KEY")
			assert.NotContains(t, string(staged), "secret-value")
			stagedDir, err := os.ReadFile(filepath.Join(dir, "config-dir"))
			require.NoError(t, err)
			_, err = os.Stat(string(stagedDir))
			assert.True(t, os.IsNotExist(err), "staged config must be removed after the container exits")
			for arg := range strings.SplitSeq(string(args), "\n") {
				if strings.HasSuffix(arg, ":/judge-config:ro") {
					assert.Equal(t, string(stagedDir)+":/judge-config:ro", arg)
				}
			}
		})
	}
}

func TestStageJudgeProviderOmitsOverriddenCredentials(t *testing.T) {
	t.Parallel()

	for _, baseURL := range []string{
		"https://user:secret@judge.example.com",
		"https://judge.example.com?api_key=secret",
		"https://judge.example.com#secret",
	} {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()
			runner := newRunner(nil, &config.RuntimeConfig{Config: config.Config{
				Providers: map[string]latest.ProviderConfig{"corporate": {
					Provider: "typesafe", BaseURL: baseURL, TokenKey: "UNUSED_GLOBAL_KEY",
				}},
			}}, Config{JudgeType: JudgeTypeEvaluator, JudgeModel: "relevance"})
			runner.agentConfig = &latest.Config{Evaluators: map[string]latest.EvaluatorConfig{
				"relevance": {
					Provider: "corporate", Model: "jev-latest", Type: "boolean",
					Instructions: relevanceEvaluatorInstructions,
					BaseURL:      "https://safe.example.com", TokenKey: "HOST_JUDGE_KEY",
				},
			}}
			dir, err := runner.stageJudgeProvider()
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })
			data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
			require.NoError(t, err)
			assert.NotContains(t, string(data), "secret")
			assert.NotContains(t, string(data), "UNUSED_GLOBAL_KEY")
			var staged userconfig.Config
			require.NoError(t, yaml.Unmarshal(data, &staged))
			assert.Equal(t, latest.ProviderConfig{
				Provider: "typesafe", BaseURL: "https://safe.example.com", TokenKey: "HOST_JUDGE_KEY",
			}, staged.Providers["corporate"])

			def := runner.agentConfig.Evaluators["relevance"]
			def.BaseURL = ""
			runner.agentConfig.Evaluators["relevance"] = def
			_, err = runner.stageJudgeProvider()
			require.ErrorContains(t, err, "resolving container judge provider")
		})
	}
}
