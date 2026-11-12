package a2a

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

const a2aRuntimeConfigYAML = `agents:
  root:
    model: primary
    instruction: This is not the selected agent.
  analyst:
    model: primary
    instruction: Selected analyst instruction.
    safety: balanced
    max_iterations: 7
    max_consecutive_tool_calls: 3
    max_old_tool_call_tokens: 1100
    max_tool_result_tokens: 900
    budgets: [shared]
    toolsets:
      - type: model_picker
        models: [primary, alternative]
      - type: filesystem
models:
  primary:
    provider: private
    model: gpt-4o
    temperature: 0.2
    max_tokens: 321
    cost: {input: 3, output: 9}
    provider_opts: {test_option: model, context_size: 8192}
  alternative:
    provider: private
    model: gpt-4o-mini
providers:
  private:
    base_url: %s/v1
    token_key: A2A_RUNTIME_TEST_TOKEN
    api_type: openai_chatcompletions
    temperature: 0.7
    max_tokens: 600
    provider_opts: {test_option: provider, inherited_option: retained}
budget: {max_tokens: 1}
budgets:
  shared: {max_tokens: 1}
runtime:
  safety: strict
`

type a2aRuntimeConfigSource struct{ config.Source }

func (a2aRuntimeConfigSource) EncryptedConfig() string { return "source-encrypted-config" }

type a2aRuntimeProviderRequest struct {
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int64   `json:"max_tokens"`
	Messages    []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	authorization string
}

func newA2ARuntimeConfigTeam(t *testing.T, encrypted string) (*team.Team, *config.RuntimeConfig, <-chan a2aRuntimeProviderRequest) {
	t.Helper()
	requests := make(chan a2aRuntimeProviderRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		var request a2aRuntimeProviderRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request.authorization = r.Header.Get("Authorization")
		select {
		case requests <- request:
		default:
			t.Error("provider request recorder overflow")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(request.Messages) == 0 {
			http.Error(w, "missing messages", http.StatusBadRequest)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		switch {
		case last.Role == "tool":
			encoded, _ := json.Marshal(last.Content)
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", encoded)
		case last.Content == "workspace":
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"marker.txt\"}"}}]}}]}`+"\n\n")
		default:
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"switch","type":"function","function":{"name":"change_model","arguments":"{\"model\":\"alternative\"}"}}]}}]}`+"\n\n")
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	catalogModel := modelsdev.Model{
		Family: "fixture-family", ToolCall: true, Temperature: true,
		Cost: &modelsdev.Cost{Input: 20, Output: 80}, Limit: modelsdev.Limit{Context: 64000, Output: 2048},
	}
	rc := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir(), EncryptedConfig: encrypted},
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"A2A_RUNTIME_TEST_TOKEN": "isolated-provider-token"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"private": {Models: map[string]modelsdev.Model{"gpt-4o": catalogModel, "gpt-4o-mini": catalogModel}},
		}}),
	}
	source := a2aRuntimeConfigSource{config.NewBytesSource("resolved-agent.yaml", []byte(fmt.Sprintf(a2aRuntimeConfigYAML, server.URL)))}
	tm, err := teamloader.Load(t.Context(), source, rc, loaderdefaults.Opts()...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tm.StopToolSets(t.Context())) })
	return tm, rc, requests
}

func TestA2ARuntimeConfig_StartupProviderRetainsResolvedConfiguration(t *testing.T) {
	t.Parallel()
	for _, encrypted := range []string{"", "explicit-encrypted-config"} {
		t.Run("encrypted="+encrypted, func(t *testing.T) {
			t.Parallel()
			tm, rc, requests := newA2ARuntimeConfigTeam(t, encrypted)
			ag, err := tm.Agent("analyst")
			require.NoError(t, err)
			cfg := ag.Model(t.Context()).BaseConfig()
			wantEncrypted := encrypted
			if wantEncrypted == "" {
				wantEncrypted = "source-encrypted-config"
			}
			assert.Equal(t, "primary", cfg.ModelConfig.Name)
			assert.Equal(t, "A2A_RUNTIME_TEST_TOKEN", cfg.ModelConfig.TokenKey)
			assert.Equal(t, "model", cfg.ModelConfig.ProviderOpts["test_option"])
			assert.Equal(t, "retained", cfg.ModelConfig.ProviderOpts["inherited_option"])
			assert.Same(t, rc.EnvProviderOverride, cfg.Env)
			assert.Same(t, rc.ModelsDevStoreOverride, cfg.ModelOptions.ModelsDevStore())
			assert.Equal(t, wantEncrypted, cfg.ModelOptions.EncryptedConfig())
			assert.Equal(t, encrypted, rc.EncryptedConfig, "source metadata must stay load-local")
			require.NoError(t, os.WriteFile(filepath.Join(rc.WorkingDir, "marker.txt"), []byte("loaded workspace"), 0o600))
			store := session.NewInMemorySessionStore()
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			var final string
			for event, err := range runDockerAgent(newFakeInvocationContext(t.Context(), "workspace", "workspace"), tm, ag.Name(), ag, store, servesafety.Resolved{Policy: session.SafetyPolicyBalanced}, rc.WorkingDir) {
				require.NoError(t, err)
				if event.TurnComplete {
					final = eventText(t, event)
				}
			}
			assert.Equal(t, "loaded workspace", final)
			assert.Len(t, requests, 2)
		})
	}
}

// Baseline current assembly: sessions resume, but invocation runtimes omit model switching and wallets.
func TestA2ARuntimeConfig_ContextLifetimeAndCurrentOmissions(t *testing.T) {
	t.Parallel()
	tm, rc, requests := newA2ARuntimeConfigTeam(t, "")
	ag, err := tm.Agent("analyst")
	require.NoError(t, err)
	initial := ag.Model(t.Context())
	safety, err := servesafety.Resolve("", string(ag.Safety()), string(tm.RuntimeSafety()))
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyBalanced, safety.Policy)
	store := newRecordingStore()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	adapter, err := newDockerAgentAdapter(tm, "analyst", store, safety, rc.WorkingDir)
	require.NoError(t, err)

	for i, contextID := range []string{"conversation", "conversation", "separate"} {
		ctx := newFakeInvocationContext(t.Context(), contextID, "question")
		var final string
		for event, err := range adapter.Run(ctx) {
			require.NoError(t, err)
			assert.Equal(t, "analyst", event.Author)
			if event.TurnComplete {
				final = eventText(t, event)
			}
		}
		assert.Contains(t, final, "model switching not configured for this runtime")
		require.Len(t, requests, 2, "one-token manifest wallets currently allow both model calls")
		first, second := <-requests, <-requests
		for _, request := range []a2aRuntimeProviderRequest{first, second} {
			assert.Equal(t, "Bearer isolated-provider-token", request.authorization)
			assert.Equal(t, "gpt-4o", request.Model)
			assert.InDelta(t, 0.2, request.Temperature, 1e-9)
			assert.Equal(t, int64(321), request.MaxTokens)
			assert.Contains(t, request.Messages[0].Content, "Selected analyst instruction.")
		}
		assert.Equal(t, "tool", second.Messages[len(second.Messages)-1].Role)
		assert.Contains(t, second.Messages[len(second.Messages)-1].Content, "model switching not configured for this runtime")
		userCount := 0
		for _, message := range first.Messages {
			if message.Role == "user" {
				userCount++
			}
		}
		wantTurns := 1
		if i == 1 {
			wantTurns = 2
		}
		assert.Equal(t, wantTurns, userCount, "only the same A2A context retains history")
		sess, err := store.GetSessionByOrigin(t.Context(), contextID, "a2a")
		require.NoError(t, err)
		assert.Equal(t, rc.WorkingDir, sess.WorkingDir)
		assert.Equal(t, [4]int{7, 0, 0, 0}, [4]int{sess.MaxIterations, sess.MaxConsecutiveToolCalls, sess.MaxOldToolCallTokens, sess.MaxToolResultTokens},
			"the store retains max_iterations but not the other three limits")
		assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())
		assert.False(t, sess.IsToolsApproved())
		assert.Nil(t, sess.Termination())
		assert.InDelta(t, float64(wantTurns)*0.0015, sess.TotalCost(), 1e-9)
		input, output := sess.Usage()
		assert.Equal(t, int64(100), input)
		assert.Equal(t, int64(50), output)
	}
	updated := store.updatedSessions()
	require.Len(t, updated, 3)
	assert.Equal(t, updated[0].ID, updated[1].ID, "a new invocation runtime resumes the persisted conversation")
	assert.Equal(t, [4]int{7, 3, 1100, 900}, [4]int{updated[0].MaxIterations, updated[0].MaxConsecutiveToolCalls, updated[0].MaxOldToolCallTokens, updated[0].MaxToolResultTokens})
	assert.Equal(t, [4]int{7, 3, 1100, 900}, [4]int{updated[1].MaxIterations, updated[1].MaxConsecutiveToolCalls, updated[1].MaxOldToolCallTokens, updated[1].MaxToolResultTokens},
		"A2A must reapply selected-agent limits when resuming a stored context")
	assert.Equal(t, [4]int{7, 3, 1100, 900}, [4]int{updated[2].MaxIterations, updated[2].MaxConsecutiveToolCalls, updated[2].MaxOldToolCallTokens, updated[2].MaxToolResultTokens})
	assert.NotSame(t, updated[0], updated[2])
	for _, sess := range updated {
		assert.True(t, sess.NonInteractive)
	}
	assert.Same(t, initial, ag.Model(t.Context()), "all conversations reuse the startup provider")
}
