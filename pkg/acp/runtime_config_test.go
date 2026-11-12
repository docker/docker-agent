package acp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
)

const acpRuntimeConfigYAML = `agents:
  analyst:
    model: primary
    instruction: Reply briefly.
    safety: balanced
    max_iterations: 7
    max_consecutive_tool_calls: 3
    max_old_tool_call_tokens: 1100
    max_tool_result_tokens: 900
    budgets: [shared]
    toolsets:
      - type: filesystem
  worker:
    model: alternative
    instruction: Help the analyst.
models:
  primary:
    provider: private
    model: gpt-4o
    temperature: 0.2
    max_tokens: 321
    cost: {input: 3, output: 9}
    provider_opts: {test_option: model, context_size: 8192}
  alternative:
    provider: openai
    model: gpt-4o-mini
    max_tokens: 456
providers:
  private:
    provider: openai
    base_url: %s/v1
    token_key: ACP_RUNTIME_TEST_TOKEN
    api_type: openai_chatcompletions
    temperature: 0.7
    max_tokens: 600
    top_p: 0.8
    parallel_tool_calls: false
    provider_opts: {test_option: provider, inherited_option: retained}
budget: {max_tokens: 1}
budgets:
  shared: {max_tokens: 1}
runtime:
  safety: strict
`

type acpRuntimeConfigSource struct{ config.Source }

func (acpRuntimeConfigSource) EncryptedConfig() string { return "source-encrypted-config" }

type acpRuntimeProviderRequest struct {
	authorization string
	model         string
	temperature   float64
	maxTokens     int64
}

func newACPRuntimeConfigAgent(t *testing.T, encrypted string) (*Agent, *config.RuntimeConfig, <-chan acpRuntimeProviderRequest, *captureWriter) {
	t.Helper()
	requests := make(chan acpRuntimeProviderRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"openai/gpt-4o-mini"}]}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		var body struct {
			Model       string  `json:"model"`
			Temperature float64 `json:"temperature"`
			MaxTokens   int64   `json:"max_tokens"`
			Messages    []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case requests <- acpRuntimeProviderRequest{r.Header.Get("Authorization"), body.Model, body.Temperature, body.MaxTokens}:
		default:
			t.Error("provider request recorder overflow")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(body.Messages) > 0 && body.Messages[len(body.Messages)-1].Content == "budget probe" {
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"list","type":"function","function":{"name":"list_directory","arguments":"{\"path\":\".\"}"}}]}}]}`+"\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Offline reply\"}}]}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	catalogModel := modelsdev.Model{
		Family: "fixture-family", ToolCall: true, Temperature: true,
		Cost: &modelsdev.Cost{Input: 2, Output: 8}, Limit: modelsdev.Limit{Context: 64000, Output: 2048},
	}
	rc := &config.RuntimeConfig{
		Config: config.Config{ModelsGateway: server.URL, WorkingDir: t.TempDir(), EncryptedConfig: encrypted},
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{
			"OPENAI_API_KEY": "dummy-openai-key", "ACP_RUNTIME_TEST_TOKEN": "isolated-provider-token",
		}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"openai":  {Models: map[string]modelsdev.Model{"gpt-4o": catalogModel, "gpt-4o-mini": catalogModel}},
			"private": {Models: map[string]modelsdev.Model{"gpt-4o": catalogModel}},
		}}),
	}
	store := session.NewInMemorySessionStore()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	source := acpRuntimeConfigSource{config.NewBytesSource("resolved-agent.yaml", []byte(fmt.Sprintf(acpRuntimeConfigYAML, server.URL)))}
	a := NewAgent(source, rc, store)
	out := &captureWriter{}
	peer := newRunAgentFixtureWithPermissions(t, &fakeRuntime{}, out, func(acpsdk.RequestPermissionRequest) any {
		return permissionSelected("allow")
	})
	a.SetAgentConnection(peer.agent.conn)
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.NoError(t, err)
	return a, rc, requests, out
}

func TestACPRuntimeConfig_ModelSwitchingIsSessionOwned(t *testing.T) {
	t.Parallel()
	for _, encrypted := range []string{"", "explicit-encrypted-config"} {
		t.Run("encrypted="+encrypted, func(t *testing.T) {
			t.Parallel()
			a, rc, requests, _ := newACPRuntimeConfigAgent(t, encrypted)
			wantEncrypted := encrypted
			if wantEncrypted == "" {
				wantEncrypted = "source-encrypted-config"
			}
			first, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
			require.NoError(t, err)
			second, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
			require.NoError(t, err)
			s := a.sessions[string(first.SessionId)]
			other := a.sessions[string(second.SessionId)]
			assert.NotSame(t, s.team, other.team)
			assert.NotSame(t, s.rt, other.rt)
			assert.Equal(t, "analyst", s.rt.CurrentAgentName(t.Context()))
			assert.Same(t, a.sessionStore, s.rt.SessionStore())
			require.True(t, s.rt.SupportsModelSwitching())
			assert.Equal(t, [4]int{7, 3, 1100, 900}, [4]int{s.sess.MaxIterations, s.sess.MaxConsecutiveToolCalls, s.sess.MaxOldToolCallTokens, s.sess.MaxToolResultTokens})

			analyst, err := s.team.Agent("analyst")
			require.NoError(t, err)
			initial := analyst.Model(t.Context()).BaseConfig()
			assert.Equal(t, rc.ModelsGateway+"/v1", initial.BaseURL)
			assert.Equal(t, wantEncrypted, initial.ModelOptions.EncryptedConfig())
			assert.Same(t, rc.EnvProviderOverride, initial.Env)
			assert.Same(t, rc.ModelsDevStoreOverride, initial.ModelOptions.ModelsDevStore())
			assert.Equal(t, "model", initial.ModelConfig.ProviderOpts["test_option"])
			assert.Equal(t, "retained", initial.ModelConfig.ProviderOpts["inherited_option"])
			require.NotNil(t, initial.ModelConfig.TopP)
			assert.InDelta(t, 0.8, *initial.ModelConfig.TopP, 1e-9)
			require.NotNil(t, initial.ModelConfig.ParallelToolCalls)
			assert.False(t, *initial.ModelConfig.ParallelToolCalls)
			choices := s.rt.AvailableModels(t.Context())
			index := slices.IndexFunc(choices, func(c modelcatalog.ModelChoice) bool { return c.Ref == "alternative" })
			require.NotEqual(t, -1, index)
			assert.Equal(t, "fixture-family", choices[index].Family)
			assert.Equal(t, 64000, choices[index].ContextLimit)

			_, err = a.SetSessionConfigOption(t.Context(), configRequest(first.SessionId, "model", "model:alternative"))
			require.NoError(t, err)
			switched := analyst.Model(t.Context()).BaseConfig()
			assert.Equal(t, "alternative", switched.ModelConfig.Name)
			assert.Equal(t, rc.ModelsGateway, switched.ModelOptions.Gateway())
			assert.Equal(t, wantEncrypted, switched.ModelOptions.EncryptedConfig())
			assert.Same(t, rc.EnvProviderOverride, switched.Env)
			assert.Same(t, rc.ModelsDevStoreOverride, switched.ModelOptions.ModelsDevStore())
			assert.Equal(t, encrypted, rc.EncryptedConfig, "source resolution stays local to each load")
			otherAgent, err := other.team.Agent("analyst")
			require.NoError(t, err)
			assert.Equal(t, "primary", otherAgent.Model(t.Context()).BaseConfig().ModelConfig.Name)
			response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: first.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("Hello")}})
			require.NoError(t, err)
			assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
			require.Len(t, requests, 1)
			request := <-requests
			assert.Empty(t, request.authorization, "an untrusted localhost gateway does not receive direct-provider credentials")
			assert.Equal(t, "gpt-4o-mini", request.model)
			assert.Equal(t, int64(456), request.maxTokens)
			_, err = a.SetSessionConfigOption(t.Context(), configRequest(first.SessionId, "model", "model:primary"))
			require.NoError(t, err)
			restored := analyst.Model(t.Context()).BaseConfig()
			assert.Equal(t, initial.ModelConfig, restored.ModelConfig, "switching back retains custom provider defaults")
			assert.Equal(t, initial.BaseURL, restored.BaseURL)
			assert.Equal(t, wantEncrypted, restored.ModelOptions.EncryptedConfig())

			require.NoError(t, os.WriteFile(filepath.Join(s.workingDir, "marker.txt"), []byte("session workspace"), 0o600))
			assert.Contains(t, workspaceTool(t, s, "list_directory", `{"path":"."}`).Output, "marker.txt")
			assert.Equal(t, rc.WorkingDir, a.defaultWorkingDir())
		})
	}
}

// Baseline the current omission; ACP does not pass the manifest wallets to runtime.New.
func TestACPRuntimeConfig_DeclaredBudgetsRemainOmittedAcrossTurns(t *testing.T) {
	t.Parallel()
	a, _, requests, out := newACPRuntimeConfigAgent(t, "")
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	cfg, ok := s.team.AgentConfig("analyst")
	require.True(t, ok)
	assert.Equal(t, []string{"shared"}, cfg.Budgets)
	assert.Equal(t, "balanced", string(cfg.Safety))
	assert.Equal(t, "strict", string(s.team.RuntimeSafety()))
	assert.Equal(t, session.SafetyPolicy(""), s.sess.GetSafetyPolicy(), "ACP keeps the session override unset until selected")
	_, err = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "strict"})
	require.NoError(t, err)
	for range 2 {
		response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("budget probe")}})
		require.NoError(t, err)
		assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	}
	require.Len(t, requests, 4, "declared one-token wallets currently block neither later calls in a turn nor later turns")
	for range 4 {
		request := <-requests
		assert.Equal(t, acpRuntimeProviderRequest{"Bearer isolated-provider-token", "gpt-4o", 0.2, 321}, request)
	}
	assert.Equal(t, session.SafetyPolicyStrict, s.sess.GetSafetyPolicy())
	assert.Nil(t, s.sess.Termination())
	input, output := s.sess.Usage()
	assert.Equal(t, int64(100), input)
	assert.Equal(t, int64(50), output)
	assert.InDelta(t, 0.003, s.sess.TotalCost(), 1e-9)
	updates := usageUpdates(t, out)
	require.NotEmpty(t, updates)
	assert.Equal(t, 8192, updates[len(updates)-1].Size)
	require.NotNil(t, updates[len(updates)-1].Cost)
	assert.InDelta(t, 0.003, updates[len(updates)-1].Cost.Amount, 1e-9)
}
