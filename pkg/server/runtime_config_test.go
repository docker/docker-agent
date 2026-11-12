package server

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
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

const apiRuntimeConfigYAML = `agents:
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
    token_key: API_RUNTIME_TEST_TOKEN
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

type apiRuntimeProviderRequest struct {
	authorization string
	model         string
	temperature   float64
	maxTokens     int64
}

func newAPIRuntimeConfig(t *testing.T) (*config.RuntimeConfig, <-chan apiRuntimeProviderRequest) {
	t.Helper()

	requests := make(chan apiRuntimeProviderRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
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
		case requests <- apiRuntimeProviderRequest{r.Header.Get("Authorization"), body.Model, body.Temperature, body.MaxTokens}:
		default:
			t.Error("unexpected provider request overflow")
			http.Error(w, "request recorder overflow", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"id":"test","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Offline reply"}}]}

data: {"id":"test","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}

`+"data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	catalogModel := modelsdev.Model{
		Name: "API runtime catalog model", Family: "gpt",
		Cost:       &modelsdev.Cost{Input: 2, Output: 8},
		Limit:      modelsdev.Limit{Context: 64000, Output: 2048},
		Modalities: modelsdev.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
		ToolCall:   true, Temperature: true,
	}
	return &config.RuntimeConfig{
		Config: config.Config{ModelsGateway: server.URL, WorkingDir: t.TempDir()},
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{
			"OPENAI_API_KEY":         "dummy-openai-key",
			"API_RUNTIME_TEST_TOKEN": "isolated-provider-token",
		}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"openai": {Models: map[string]modelsdev.Model{"gpt-4o": catalogModel, "gpt-4o-mini": catalogModel}},
		}}),
	}, requests
}

func newAPIRuntimeConfigManager(t *testing.T, rc *config.RuntimeConfig, yaml string) *SessionManager {
	t.Helper()

	store := session.NewInMemorySessionStore()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	source := &encryptedMockSource{
		mockSource: &mockSource{name: "resolved-agent.yaml", data: []byte(yaml)},
		enc:        "source-encrypted-config",
	}
	return NewSessionManager(t.Context(), config.Sources{"agent.yaml": source}, store, 0, rc)
}

func newAPIRuntimeForSession(t *testing.T, sm *SessionManager, sess *session.Session, selected string) (runtime.Runtime, *team.Team) {
	t.Helper()

	var loadedTeam *team.Team
	sm.newRuntime = func(ctx context.Context, tm *team.Team, opts ...runtime.Opt) (runtime.Runtime, error) {
		loadedTeam = tm
		t.Cleanup(func() { require.NoError(t, tm.StopToolSets(context.WithoutCancel(t.Context()))) })
		return runtime.New(ctx, tm, opts...)
	}
	run, _, err := sm.runtimeForSession(t.Context(), sess, "agent.yaml", selected, sm.runConfig)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, run.Close()) })
	require.NotNil(t, loadedTeam)
	return run, loadedTeam
}

func apiRuntimeModelChoice(t *testing.T, run runtime.Runtime, ref string) modelcatalog.ModelChoice {
	t.Helper()

	choices := run.AvailableModels(t.Context())
	index := slices.IndexFunc(choices, func(choice modelcatalog.ModelChoice) bool { return choice.Ref == ref })
	require.NotEqual(t, -1, index, "model %q missing from API runtime picker", ref)
	return choices[index]
}

func TestAPIRuntimeConfig_ModelSwitchingPropagatesLoadedConfiguration(t *testing.T) {
	t.Parallel()

	for _, explicit := range []string{"", "explicit-encrypted-config"} {
		t.Run("encrypted_config="+explicit, func(t *testing.T) {
			t.Parallel()
			rc, requests := newAPIRuntimeConfig(t)
			rc.EncryptedConfig = explicit
			wantEncrypted := explicit
			if wantEncrypted == "" {
				wantEncrypted = "source-encrypted-config"
			}
			sm := newAPIRuntimeConfigManager(t, rc, fmt.Sprintf(apiRuntimeConfigYAML, rc.ModelsGateway))
			sess, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Configuration regression")))
			require.NoError(t, err)
			run, tm := newAPIRuntimeForSession(t, sm, sess, "reviewer")
			require.True(t, run.SupportsModelSwitching())
			assert.Equal(t, "reviewer", run.CurrentAgentName(t.Context()))
			assert.Same(t, sm.sessionStore, run.SessionStore())

			reviewer, err := tm.Agent("reviewer")
			require.NoError(t, err)
			initial := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, "secondary", initial.ModelConfig.Name)
			assert.Equal(t, rc.ModelsGateway+"/v1", initial.BaseURL)
			assert.Same(t, rc.EnvProviderOverride, initial.Env)
			assert.Same(t, rc.ModelsDevStoreOverride, initial.ModelOptions.ModelsDevStore())
			assert.Empty(t, initial.ModelOptions.Gateway(), "explicit custom endpoints bypass the gateway")
			assert.Equal(t, wantEncrypted, initial.ModelOptions.EncryptedConfig())
			assert.Equal(t, explicit, rc.EncryptedConfig, "source resolution must not mutate the shared runtime config")

			primary := apiRuntimeModelChoice(t, run, "primary")
			assert.False(t, primary.IsDefault)
			secondary := apiRuntimeModelChoice(t, run, "secondary")
			assert.True(t, secondary.IsDefault)
			assert.Equal(t, 8192, secondary.ContextLimit)
			assert.InDelta(t, 3, secondary.InputCost, 1e-9)
			assert.InDelta(t, 9, secondary.OutputCost, 1e-9)

			require.NoError(t, run.SetAgentModel(t.Context(), "reviewer", "primary"))
			switched := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, "primary", switched.ModelConfig.Name)
			require.NotNil(t, switched.ModelConfig.Temperature)
			assert.InDelta(t, 0.3, *switched.ModelConfig.Temperature, 1e-9)
			require.NotNil(t, switched.ModelConfig.MaxTokens)
			assert.Equal(t, int64(456), *switched.ModelConfig.MaxTokens)
			assert.Equal(t, rc.ModelsGateway, switched.ModelOptions.Gateway())
			assert.Equal(t, wantEncrypted, switched.ModelOptions.EncryptedConfig())
			assert.Same(t, rc.EnvProviderOverride, switched.Env)
			assert.Same(t, rc.ModelsDevStoreOverride, switched.ModelOptions.ModelsDevStore())

			require.NoError(t, run.SetAgentModel(t.Context(), "reviewer", "openai/gpt-4o-mini"))
			inline := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, "gpt-4o-mini", inline.ModelConfig.Model)
			assert.Equal(t, int64(2048), inline.ModelOptions.MaxTokens())
			assert.Same(t, rc.ModelsDevStoreOverride, inline.ModelOptions.ModelsDevStore())
			assert.Equal(t, wantEncrypted, inline.ModelOptions.EncryptedConfig())
			catalog := apiRuntimeModelChoice(t, run, "openai/gpt-4o-mini")
			assert.True(t, catalog.IsCatalog)
			assert.Equal(t, "gpt", catalog.Family)
			assert.Equal(t, 64000, catalog.ContextLimit)
			assert.Equal(t, int64(2048), catalog.OutputLimit)
			assert.InDelta(t, 2, catalog.InputCost, 1e-9)
			assert.InDelta(t, 8, catalog.OutputCost, 1e-9)
			assert.Equal(t, []string{"text", "image"}, catalog.InputModalities)

			require.NoError(t, run.SetAgentModel(t.Context(), "reviewer", "private/gpt-4o-mini"))
			inherited := reviewer.Model(t.Context()).BaseConfig()
			require.NotNil(t, inherited.ModelConfig.Temperature)
			assert.InDelta(t, 0.7, *inherited.ModelConfig.Temperature, 1e-9)
			require.NotNil(t, inherited.ModelConfig.MaxTokens)
			assert.Equal(t, int64(600), *inherited.ModelConfig.MaxTokens)
			assert.Equal(t, "provider", inherited.ModelConfig.ProviderOpts["test_option"])
			assert.Equal(t, rc.ModelsGateway+"/v1", inherited.BaseURL)
			assert.Same(t, rc.EnvProviderOverride, inherited.Env)
			assert.Empty(t, inherited.ModelOptions.Gateway())
			assert.Equal(t, wantEncrypted, inherited.ModelOptions.EncryptedConfig())

			require.NoError(t, run.SetAgentModel(t.Context(), "reviewer", "secondary"))
			custom := reviewer.Model(t.Context()).BaseConfig()
			assert.Equal(t, initial.ModelConfig, custom.ModelConfig)
			assert.Equal(t, rc.ModelsGateway+"/v1", custom.BaseURL)
			assert.Equal(t, "API_RUNTIME_TEST_TOKEN", custom.ModelConfig.TokenKey)
			require.NotNil(t, custom.ModelConfig.TopP)
			assert.InDelta(t, 0.8, *custom.ModelConfig.TopP, 1e-9)
			require.NotNil(t, custom.ModelConfig.ParallelToolCalls)
			assert.False(t, *custom.ModelConfig.ParallelToolCalls)
			assert.Equal(t, "model", custom.ModelConfig.ProviderOpts["test_option"])
			assert.Equal(t, "retained", custom.ModelConfig.ProviderOpts["inherited_option"])
			assert.Equal(t, "openai_chatcompletions", custom.ModelConfig.ProviderOpts["api_type"])

			sess.AddMessage(session.UserMessage("Reply offline"))
			for event := range run.RunStream(t.Context(), sess) {
				if failure, ok := event.(*runtime.ErrorEvent); ok {
					t.Errorf("runtime error: %s", failure.Error)
				}
			}
			select {
			case request := <-requests:
				assert.Equal(t, "Bearer isolated-provider-token", request.authorization)
				assert.Equal(t, "gpt-4o", request.model)
				assert.InDelta(t, 0.2, request.temperature, 1e-9)
				assert.Equal(t, int64(321), request.maxTokens)
			default:
				t.Fatal("runtime never reached the configured provider")
			}
			require.NoError(t, run.SetAgentModel(t.Context(), "reviewer", ""))
			assert.False(t, reviewer.HasModelOverride())
			assert.Equal(t, initial.ModelConfig, reviewer.Model(t.Context()).BaseConfig().ModelConfig)
			require.NoError(t, run.SetCurrentAgent(t.Context(), "root"))
			assert.True(t, apiRuntimeModelChoice(t, run, "primary").IsDefault)
			assert.False(t, apiRuntimeModelChoice(t, run, "secondary").IsDefault)
		})
	}
}

func TestAPIRuntimeConfig_SelectedAgentSessionDefaultsAndWorkingDir(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		selected   string
		workingDir bool
		explicit   session.SafetyPolicy
		wantAgent  string
		wantPolicy session.SafetyPolicy
		wantLimits [4]int
	}{
		{"team default", "", false, "", "root", session.SafetyPolicyBalanced, [4]int{7, 3, 1100, 900}},
		{"selected agent", "reviewer", true, "", "reviewer", session.SafetyPolicyStrict, [4]int{9, 4, 1200, 1000}},
		{"explicit safety", "root", true, session.SafetyPolicyAutonomous, "root", session.SafetyPolicyAutonomous, [4]int{7, 3, 1100, 900}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc, _ := newAPIRuntimeConfig(t)
			serverDir := rc.WorkingDir
			workspace := serverDir
			template := session.New(session.WithSafetyPolicy(tc.explicit))
			if tc.workingDir {
				workspace = t.TempDir()
				template.WorkingDir = workspace
			}
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "marker.txt"), []byte(tc.name), 0o600))
			sm := newAPIRuntimeConfigManager(t, rc, fmt.Sprintf(apiRuntimeConfigYAML, rc.ModelsGateway))
			sess, err := sm.CreateSession(t.Context(), template)
			require.NoError(t, err)
			run, _ := newAPIRuntimeForSession(t, sm, sess, tc.selected)
			assert.Equal(t, tc.wantAgent, run.CurrentAgentName(t.Context()))
			assert.Equal(t, tc.wantPolicy, sess.GetSafetyPolicy())
			assert.Equal(t, tc.wantLimits, [4]int{sess.MaxIterations, sess.MaxConsecutiveToolCalls, sess.MaxOldToolCallTokens, sess.MaxToolResultTokens})
			assert.Equal(t, serverDir, rc.WorkingDir, "session cwd must not leak to the server's shared config")
			stored, err := sm.sessionStore.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPolicy, stored.GetSafetyPolicy())
			assert.Equal(t, template.WorkingDir, stored.WorkingDir)

			available, err := run.CurrentAgentTools(t.Context())
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

func TestAPIRuntimeConfig_EnforcesManifestBudgets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		limits    string
		wantNames []string
		stoppedBy string
		limit     string
	}{
		{"unbudgeted", "", nil, "", ""},
		{"run tokens", "budget:\n  max_tokens: 1\n", []string{"run"}, "run", "max_tokens"},
		{"run cost", "budget:\n  max_cost: 0.0001\n", []string{"run"}, "run", "max_cost"},
		{"named tokens", "budgets:\n  shared:\n    max_tokens: 1\n", []string{"shared"}, "shared", "max_tokens"},
		{"named cost", "budgets:\n  shared:\n    max_cost: 0.0001\n", []string{"shared"}, "shared", "max_cost"},
		{"both", "budget:\n  max_tokens: 1\nbudgets:\n  shared:\n    max_tokens: 1\n", []string{"run", "shared"}, "run", "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc, requests := newAPIRuntimeConfig(t)
			agentsYAML := "agents:\n  root:\n    model: offline\n    instruction: Reply briefly.\n"
			if slices.Contains(tc.wantNames, "shared") {
				agentsYAML += "    budgets: [shared]\n"
			}
			sm := newAPIRuntimeConfigManager(t, rc, agentsYAML+fmt.Sprintf(apiBudgetModelsYAML, rc.ModelsGateway)+tc.limits)
			cleanupAPIBudgetRuntimes(t, sm)
			sess, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Budget enforcement")))
			require.NoError(t, err)
			first := runAPIBudgetTurn(t, sm, sess.ID, "root")
			if tc.stoppedBy == "" {
				second := runAPIBudgetTurn(t, sm, sess.ID, "root")
				assert.Len(t, requests, 2, "configs without budgets must remain unlimited")
				for _, event := range append(first, second...) {
					switch event.(type) {
					case *runtime.BudgetUsageEvent, *runtime.BudgetExceededEvent:
						t.Errorf("unbudgeted session emitted a budget event: %T", event)
					}
				}
				return
			}
			usage := lastAPIBudgetUsage(t, first)
			require.Len(t, usage.Budgets, len(tc.wantNames))
			for i, wallet := range usage.Budgets {
				assert.Equal(t, tc.wantNames[i], wallet.Name)
				assert.Equal(t, int64(150), wallet.Tokens)
				assert.InDelta(t, 0.0006, wallet.Cost, 1e-9)
				assert.False(t, wallet.Unpriced)
			}
			second := runAPIBudgetTurn(t, sm, sess.ID, "root")
			requireAPIBudgetStop(t, second, "root", tc.stoppedBy, tc.limit)
			assert.Equal(t, usage.Budgets, lastAPIBudgetUsage(t, second).Budgets,
				"a blocked turn must retain the same wallet without further spend")
			assert.Len(t, requests, 1, "exhausted budgets must block the next model call")
			stored, err := sm.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.InDelta(t, 0.0006, stored.TotalCost(), 1e-9)
		})
	}
}
