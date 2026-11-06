package root

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

const cliRuntimeConfigYAML = `agents:
  root:
    model: primary
    instruction: Be helpful.
    safety: balanced
    max_iterations: 7
    max_consecutive_tool_calls: 3
    max_old_tool_call_tokens: 1100
    max_tool_result_tokens: 900
    toolsets:
      - type: filesystem
  reviewer:
    model: secondary
    instruction: Review carefully.
    max_iterations: 9
    max_consecutive_tool_calls: 4
    max_old_tool_call_tokens: 1200
    max_tool_result_tokens: 1000
    toolsets:
      - type: filesystem
models:
  primary:
    provider: openai
    model: gpt-4o
    temperature: 0.3
    max_tokens: 456
  secondary:
    provider: private
    model: gpt-4o
    temperature: 0.2
    max_tokens: 321
    cost:
      input: 3
      output: 9
    provider_opts:
      context_size: 8192
      test_option: model
providers:
  private:
    provider: openai
    base_url: %s/v1
    token_key: CLI_RUNTIME_TEST_TOKEN
    api_type: openai_chatcompletions
    temperature: 0.7
    max_tokens: 600
    top_p: 0.8
    parallel_tool_calls: false
    provider_opts:
      test_option: provider
      inherited_option: retained
runtime:
  safety: strict
`

type cliRuntimeEncryptedSource struct {
	config.Source
}

func (cliRuntimeEncryptedSource) EncryptedConfig() string { return "source-encrypted-config" }

type cliRuntimeProviderRequest struct {
	authorization string
	model         string
	temperature   float64
	maxTokens     int64
}

func newCLIRuntimeConfig(t *testing.T) (*runExecFlags, <-chan cliRuntimeProviderRequest) {
	t.Helper()
	requests := make(chan cliRuntimeProviderRequest, 4)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"openai/gpt-4o-mini"}]}`)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		var body struct {
			Model       string  `json:"model"`
			Temperature float64 `json:"temperature"`
			MaxTokens   int64   `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case requests <- cliRuntimeProviderRequest{r.Header.Get("Authorization"), body.Model, body.Temperature, body.MaxTokens}:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"id":"test","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Offline reply"}}]}

data: {"id":"test","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}

`+"data: [DONE]\n\n")
	}))
	t.Cleanup(providerServer.Close)
	catalogModel := modelsdev.Model{
		Name: "CLI runtime catalog model", Family: "gpt", ToolCall: true, Temperature: true,
		Cost:       &modelsdev.Cost{Input: 2, Output: 8},
		Limit:      modelsdev.Limit{Context: 64000, Output: 2048},
		Modalities: modelsdev.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
	}
	return &runExecFlags{runConfig: config.RuntimeConfig{
		Config: config.Config{ModelsGateway: providerServer.URL, WorkingDir: t.TempDir()},
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{
			"OPENAI_API_KEY":         "dummy-openai-key",
			"CLI_RUNTIME_TEST_TOKEN": "isolated-provider-token",
		}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"openai": {Models: map[string]modelsdev.Model{"gpt-4o": catalogModel, "gpt-4o-mini": catalogModel}},
		}}),
	}}, requests
}

func loadCLIRuntimeConfig(t *testing.T, f *runExecFlags, yaml string) *teamloader.LoadResult {
	t.Helper()
	source := cliRuntimeEncryptedSource{Source: config.NewBytesSource("agent.yaml", []byte(yaml))}
	loaded, err := f.loadAgentFrom(t.Context(), runtime.LoadTeamRequest{Source: source, RunConfig: &f.runConfig})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loaded.Team.StopToolSets(context.WithoutCancel(t.Context()))) })
	return loaded
}

func createCLIRuntimeConfig(t *testing.T, f *runExecFlags, loaded *teamloader.LoadResult, req runtime.CreateSessionRequest) (runtime.Runtime, *session.Session) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	rt, sess, err := f.createLocalRuntimeAndSession(t.Context(), loaded, req, store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	assert.Same(t, store, rt.SessionStore())
	sess.SetTitle("Configuration regression")
	return rt, sess
}

func cliRuntimeModelChoice(t *testing.T, rt runtime.Runtime, ref string) modelcatalog.ModelChoice {
	t.Helper()
	choices := rt.AvailableModels(t.Context())
	index := slices.IndexFunc(choices, func(choice modelcatalog.ModelChoice) bool { return choice.Ref == ref })
	require.NotEqual(t, -1, index, "model %q missing from CLI runtime picker", ref)
	return choices[index]
}

func TestCLIRuntimeConfig_ModelSwitchingPropagatesLoadedConfiguration(t *testing.T) {
	t.Parallel()
	for _, explicit := range []string{"", "explicit-encrypted-config"} {
		t.Run("encrypted_config="+explicit, func(t *testing.T) {
			t.Parallel()
			f, requests := newCLIRuntimeConfig(t)
			f.runConfig.EncryptedConfig = explicit
			loaded := loadCLIRuntimeConfig(t, f, fmt.Sprintf(cliRuntimeConfigYAML, f.runConfig.ModelsGateway))
			wantEncrypted := explicit
			if wantEncrypted == "" {
				wantEncrypted = "source-encrypted-config"
			}
			assert.Equal(t, wantEncrypted, loaded.EncryptedConfig)
			assert.Equal(t, explicit, f.runConfig.EncryptedConfig, "source resolution must not mutate shared config")
			rt, sess := createCLIRuntimeConfig(t, f, loaded, runtime.CreateSessionRequest{AgentName: "reviewer", WorkingDir: f.runConfig.WorkingDir})
			assert.True(t, rt.SupportsModelSwitching())
			assert.Equal(t, "reviewer", rt.CurrentAgentName(t.Context()))
			reviewer, err := loaded.Team.Agent("reviewer")
			require.NoError(t, err)
			initial := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, "secondary", initial.ModelConfig.Name)
			assert.Same(t, f.runConfig.EnvProviderOverride, initial.Env)
			assert.Same(t, f.runConfig.ModelsDevStoreOverride, initial.ModelOptions.ModelsDevStore())
			assert.Equal(t, wantEncrypted, initial.ModelOptions.EncryptedConfig())
			assert.True(t, cliRuntimeModelChoice(t, rt, "secondary").IsDefault)
			assert.False(t, cliRuntimeModelChoice(t, rt, "primary").IsDefault)

			require.NoError(t, rt.SetAgentModel(t.Context(), "reviewer", "primary"))
			switched := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, "primary", switched.ModelConfig.Name)
			assert.Equal(t, f.runConfig.ModelsGateway, switched.ModelOptions.Gateway())
			assert.Equal(t, wantEncrypted, switched.ModelOptions.EncryptedConfig())
			assert.Same(t, f.runConfig.EnvProviderOverride, switched.Env)
			assert.Same(t, f.runConfig.ModelsDevStoreOverride, switched.ModelOptions.ModelsDevStore())
			require.NotNil(t, switched.ModelConfig.Temperature)
			assert.InDelta(t, 0.3, *switched.ModelConfig.Temperature, 1e-9)
			require.NotNil(t, switched.ModelConfig.MaxTokens)
			assert.Equal(t, int64(456), *switched.ModelConfig.MaxTokens)

			require.NoError(t, rt.SetAgentModel(t.Context(), "reviewer", "openai/gpt-4o-mini"))
			inline := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, "gpt-4o-mini", inline.ModelConfig.Model)
			assert.Equal(t, f.runConfig.ModelsGateway, inline.ModelOptions.Gateway())
			assert.Equal(t, wantEncrypted, inline.ModelOptions.EncryptedConfig())
			assert.Same(t, f.runConfig.EnvProviderOverride, inline.Env)
			assert.Equal(t, int64(2048), inline.ModelOptions.MaxTokens())
			assert.Same(t, f.runConfig.ModelsDevStoreOverride, inline.ModelOptions.ModelsDevStore())
			catalog := cliRuntimeModelChoice(t, rt, "openai/gpt-4o-mini")
			assert.True(t, catalog.IsCatalog)
			assert.Equal(t, 64000, catalog.ContextLimit)
			assert.Equal(t, int64(2048), catalog.OutputLimit)
			assert.InDelta(t, 2, catalog.InputCost, 1e-9)

			require.NoError(t, rt.SetAgentModel(t.Context(), "reviewer", "private/gpt-4o-mini"))
			inherited := reviewer.Model(t.Context()).BaseConfig()
			require.NotNil(t, inherited.ModelConfig.Temperature)
			assert.InDelta(t, 0.7, *inherited.ModelConfig.Temperature, 1e-9)
			require.NotNil(t, inherited.ModelConfig.MaxTokens)
			assert.Equal(t, int64(600), *inherited.ModelConfig.MaxTokens)
			assert.Equal(t, "provider", inherited.ModelConfig.ProviderOpts["test_option"])
			assert.Equal(t, f.runConfig.ModelsGateway+"/v1", inherited.BaseURL)
			assert.Equal(t, wantEncrypted, inherited.ModelOptions.EncryptedConfig())
			assert.Same(t, f.runConfig.EnvProviderOverride, inherited.Env)
			assert.Empty(t, inherited.ModelOptions.Gateway())

			require.NoError(t, rt.SetAgentModel(t.Context(), "reviewer", "secondary"))
			custom := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, initial.ModelConfig, custom.ModelConfig)
			assert.Empty(t, custom.ModelOptions.Gateway(), "custom endpoints bypass the gateway")
			assert.Equal(t, f.runConfig.ModelsGateway+"/v1", custom.BaseURL)
			assert.Equal(t, wantEncrypted, custom.ModelOptions.EncryptedConfig())
			assert.Equal(t, "model", custom.ModelConfig.ProviderOpts["test_option"])
			assert.Equal(t, "retained", custom.ModelConfig.ProviderOpts["inherited_option"])
			require.NotNil(t, custom.ModelConfig.TopP)
			assert.InDelta(t, 0.8, *custom.ModelConfig.TopP, 1e-9)
			require.NotNil(t, custom.ModelConfig.ParallelToolCalls)
			assert.False(t, *custom.ModelConfig.ParallelToolCalls)

			sess.AddMessage(session.UserMessage("Reply offline"))
			for event := range rt.RunStream(t.Context(), sess) {
				if failure, ok := event.(*runtime.ErrorEvent); ok {
					t.Errorf("runtime error: %s", failure.Error)
				}
			}
			assert.Equal(t, "Offline reply", sess.GetLastAssistantMessageContent())
			select {
			case request := <-requests:
				assert.Equal(t, "Bearer isolated-provider-token", request.authorization)
				assert.Equal(t, "gpt-4o", request.model)
				assert.InDelta(t, 0.2, request.temperature, 1e-9)
				assert.Equal(t, int64(321), request.maxTokens)
			default:
				t.Fatal("runtime never reached the configured provider")
			}
			require.NoError(t, rt.SetAgentModel(t.Context(), "reviewer", ""))
			assert.False(t, reviewer.HasModelOverride())
			assert.Equal(t, initial.ModelConfig, reviewer.Model(t.Context()).BaseConfig().ModelConfig)
			require.NoError(t, rt.SetCurrentAgent(t.Context(), "root"))
			assert.True(t, cliRuntimeModelChoice(t, rt, "primary").IsDefault)
		})
	}
}

func TestCLIRuntimeConfig_SelectedAgentSessionDefaultsAndWorkingDir(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		selected   string
		explicit   session.SafetyPolicy
		wantAgent  string
		wantPolicy session.SafetyPolicy
		wantLimits [4]int
	}{
		{"team default", "", "", "root", session.SafetyPolicyBalanced, [4]int{7, 3, 1100, 900}},
		{"selected agent", "reviewer", "", "reviewer", session.SafetyPolicyStrict, [4]int{9, 4, 1200, 1000}},
		{"explicit safety", "reviewer", session.SafetyPolicyAutonomous, "reviewer", session.SafetyPolicyAutonomous, [4]int{9, 4, 1200, 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newCLIRuntimeConfig(t)
			require.NoError(t, os.WriteFile(filepath.Join(f.runConfig.WorkingDir, "marker.txt"), []byte(tc.name), 0o600))
			loaded := loadCLIRuntimeConfig(t, f, fmt.Sprintf(cliRuntimeConfigYAML, f.runConfig.ModelsGateway))
			rt, sess := createCLIRuntimeConfig(t, f, loaded, runtime.CreateSessionRequest{
				AgentName: tc.selected, SafetyPolicy: tc.explicit, WorkingDir: f.runConfig.WorkingDir,
			})
			assert.Equal(t, tc.wantAgent, rt.CurrentAgentName(t.Context()))
			assert.Equal(t, tc.wantPolicy, sess.GetSafetyPolicy())
			assert.Equal(t, f.runConfig.WorkingDir, sess.WorkingDir)
			assert.Equal(t, tc.wantLimits, [4]int{sess.MaxIterations, sess.MaxConsecutiveToolCalls, sess.MaxOldToolCallTokens, sess.MaxToolResultTokens})
			available, err := rt.CurrentAgentTools(t.Context())
			require.NoError(t, err)
			index := slices.IndexFunc(available, func(tool tools.Tool) bool { return tool.Name == "read_file" })
			require.NotEqual(t, -1, index)
			result, err := available[index].Handler(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"marker.txt"}`},
			}, nil)
			require.NoError(t, err)
			require.False(t, result.IsError, "%s", result.Output)
			assert.Equal(t, tc.name, result.Output)
		})
	}
}

func TestCLIRuntimeConfig_EnforcesManifestRunAndNamedBudgets(t *testing.T) {
	t.Parallel()
	for _, budget := range []string{"run", "shared"} {
		t.Run(budget, func(t *testing.T) {
			t.Parallel()
			f, requests := newCLIRuntimeConfig(t)
			yaml := fmt.Sprintf(cliRuntimeConfigYAML, f.runConfig.ModelsGateway)
			if budget == "run" {
				yaml += "budget:\n  max_tokens: 1\n"
			} else {
				yaml = strings.Replace(yaml, "    model: secondary\n", "    model: secondary\n    budgets: [shared]\n", 1)
				yaml += "budgets:\n  shared:\n    max_tokens: 1\n"
			}
			loaded := loadCLIRuntimeConfig(t, f, yaml)
			rt, sess := createCLIRuntimeConfig(t, f, loaded, runtime.CreateSessionRequest{AgentName: "reviewer", WorkingDir: f.runConfig.WorkingDir})
			var statuses []runtime.BudgetStatus
			var stops []*runtime.BudgetExceededEvent
			for range 2 {
				sess.AddMessage(session.UserMessage("Reply offline"))
				for event := range rt.RunStream(t.Context(), sess) {
					switch e := event.(type) {
					case *runtime.BudgetUsageEvent:
						statuses = e.Budgets
					case *runtime.BudgetExceededEvent:
						stops = append(stops, e)
					case *runtime.ErrorEvent:
						t.Errorf("runtime error: %s", e.Error)
					}
				}
			}
			require.Len(t, statuses, 1)
			assert.Equal(t, budget, statuses[0].Name)
			assert.Equal(t, int64(1), statuses[0].MaxTokens)
			assert.Equal(t, int64(150), statuses[0].Tokens)
			require.NotEmpty(t, stops)
			for _, stop := range stops {
				assert.Equal(t, budget, stop.Budget)
				assert.Equal(t, "max_tokens", stop.Limit)
			}
			assert.Len(t, requests, 1, "an exhausted conversation budget must block the next model call")
		})
	}
}
