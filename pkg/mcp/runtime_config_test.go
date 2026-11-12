package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

const mcpRuntimeConfigYAML = `agents:
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
    token_key: MCP_RUNTIME_TEST_TOKEN
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

type mcpRuntimeConfigSource struct{ config.Source }

func (mcpRuntimeConfigSource) EncryptedConfig() string { return "source-encrypted-config" }

type mcpRuntimeProviderRequest struct {
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int64   `json:"max_tokens"`
	Messages    []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	authorization string
}

func newMCPRuntimeConfig(t *testing.T) (*config.RuntimeConfig, string, <-chan mcpRuntimeProviderRequest) {
	t.Helper()
	requests := make(chan mcpRuntimeProviderRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		var request mcpRuntimeProviderRequest
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
			content, _ := json.Marshal(last.Content)
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", content)
		case last.Content == "workspace":
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"marker.txt\"}"}}]}}]}`+"\n\n")
		default:
			_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"switch","type":"function","function":{"name":"change_model","arguments":"{\"model\":\"alternative\"}"}}]}}]}`+"\n\n")
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	catalogModel := modelsdev.Model{ToolCall: true, Temperature: true, Limit: modelsdev.Limit{Context: 64000, Output: 2048}}
	rc := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"MCP_RUNTIME_TEST_TOKEN": "isolated-provider-token"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"private": {Models: map[string]modelsdev.Model{"gpt-4o": catalogModel, "gpt-4o-mini": catalogModel}},
		}}),
	}
	return rc, fmt.Sprintf(mcpRuntimeConfigYAML, server.URL), requests
}

// Baseline omissions via tools/call: load-time clients survive, but model switching and wallets do not.
func TestMCPRuntimeConfig_ServerRetainsProviderButOmitsSwitchingAndBudgets(t *testing.T) {
	t.Parallel()
	rc, yaml, requests := newMCPRuntimeConfig(t)
	rc.MCPToolName = "ask-analyst"
	filename := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(filename, []byte(yaml), 0o600))
	server, cleanup, err := createMCPServer(t.Context(), filename, "analyst", rc)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	httpServer := httptest.NewServer(newStreamableHTTPHandler(server))
	t.Cleanup(httpServer.Close)
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "config-test", Version: "1"}, nil)
	conn, err := client.Connect(t.Context(), &mcpsdk.StreamableClientTransport{Endpoint: httpServer.URL, HTTPClient: httpServer.Client()}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	listed, err := conn.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	assert.Equal(t, "ask-analyst", listed.Tools[0].Name)

	for _, prompt := range []string{"first question", "second question"} {
		result, err := conn.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "ask-analyst", Arguments: map[string]any{"message": prompt}})
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.Len(t, result.Content, 1)
		text, ok := result.Content[0].(*mcpsdk.TextContent)
		require.True(t, ok)
		assert.Contains(t, text.Text, "model switching not configured for this runtime")
		require.Len(t, requests, 2, "one-token manifest wallets currently do not block the second model call")
		for i := range 2 {
			request := <-requests
			assert.Equal(t, "Bearer isolated-provider-token", request.authorization)
			assert.Equal(t, "gpt-4o", request.Model)
			assert.InDelta(t, 0.2, request.Temperature, 1e-9)
			assert.Equal(t, int64(321), request.MaxTokens)
			assert.Contains(t, request.Messages[0].Content, "Selected analyst instruction.")
			if i == 0 {
				require.Len(t, request.Messages, 2, "each tool invocation starts a fresh conversation")
				assert.Equal(t, prompt, request.Messages[1].Content)
			}
		}
	}
}

func TestMCPRuntimeConfig_LoadedClientAndToolCallSessionSettings(t *testing.T) {
	t.Parallel()
	rc, yaml, requests := newMCPRuntimeConfig(t)
	source := mcpRuntimeConfigSource{config.NewBytesSource("resolved-agent.yaml", []byte(yaml))}
	tm, err := teamloader.Load(t.Context(), source, rc, loaderdefaults.Opts()...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tm.StopToolSets(t.Context())) })
	ag, err := tm.Agent("analyst")
	require.NoError(t, err)
	initial := ag.Model(t.Context())
	cfg := initial.BaseConfig()
	assert.Equal(t, "MCP_RUNTIME_TEST_TOKEN", cfg.ModelConfig.TokenKey)
	assert.Equal(t, "model", cfg.ModelConfig.ProviderOpts["test_option"])
	assert.Equal(t, "retained", cfg.ModelConfig.ProviderOpts["inherited_option"])
	assert.Same(t, rc.EnvProviderOverride, cfg.Env)
	assert.Same(t, rc.ModelsDevStoreOverride, cfg.ModelOptions.ModelsDevStore())
	assert.Equal(t, "source-encrypted-config", cfg.ModelOptions.EncryptedConfig())
	assert.Empty(t, rc.EncryptedConfig)

	sess := newToolCallSession(ag, "question", session.SafetyPolicyBalanced, rc.WorkingDir)
	assert.Equal(t, [4]int{7, 3, 1100, 900}, [4]int{sess.MaxIterations, sess.MaxConsecutiveToolCalls, sess.MaxOldToolCallTokens, sess.MaxToolResultTokens})
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())
	assert.False(t, sess.IsToolsApproved())
	assert.True(t, sess.NonInteractive)
	assert.Equal(t, rc.WorkingDir, sess.WorkingDir)
	require.NoError(t, os.WriteFile(filepath.Join(rc.WorkingDir, "marker.txt"), []byte("loaded workspace"), 0o600))
	handler := createToolHandler(tm, "analyst", session.SafetyPolicyBalanced, rc.WorkingDir)
	for range 2 {
		_, output, err := handler(t.Context(), nil, ToolInput{Message: "workspace"})
		require.NoError(t, err)
		assert.Equal(t, "loaded workspace", output.Response)
	}
	assert.Same(t, initial, ag.Model(t.Context()), "invocation runtimes reuse the startup provider")
	assert.Len(t, requests, 4)
}
