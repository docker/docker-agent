package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

// chatgptTestToken builds an unsigned JWT carrying the ChatGPT account claim,
// the shape the middleware parses the account id from.
func chatgptTestToken(t *testing.T, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// capturedRequest records what the fake Codex backend received.
type capturedRequest struct {
	path   string
	header http.Header
	body   map[string]any
}

func startFakeCodexBackend(t *testing.T) (*httptest.Server, func() capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured capturedRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		captured = capturedRequest{path: r.URL.Path, header: r.Header.Clone(), body: body}
		mu.Unlock()
		writeResponsesSSEResponse(w)
	}))
	t.Cleanup(server.Close)

	return server, func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
}

func drainChatStream(t *testing.T, client *Client, messages []chat.Message) {
	t.Helper()
	stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

func TestChatGPTRequestShapeAndHeaders(t *testing.T) {
	server, captured := startFakeCodexBackend(t)

	token := chatgptTestToken(t, "acc_123")
	temperature := 0.5
	maxTokens := int64(32000)
	cfg := &latest.ModelConfig{
		Provider:    "chatgpt",
		Model:       "gpt-5.2",
		BaseURL:     server.URL,
		TokenKey:    chatgpt.TokenEnvVar,
		Temperature: &temperature,
		MaxTokens:   &maxTokens,
	}
	env := environment.NewMapEnvProvider(map[string]string{chatgpt.TokenEnvVar: token})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	drainChatStream(t, client, []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are a pirate."},
		{Role: chat.MessageRoleSystem, Content: "Answer briefly."},
		{Role: chat.MessageRoleUser, Content: "hi"},
	})

	got := captured()
	assert.Equal(t, "/responses", got.path, "the Codex backend only serves the Responses API")

	assert.Equal(t, "Bearer "+token, got.header.Get("Authorization"))
	assert.Equal(t, "acc_123", got.header.Get("chatgpt-account-id"))
	assert.Equal(t, "responses=experimental", got.header.Get("OpenAI-Beta"))
	assert.Equal(t, chatgpt.Originator, got.header.Get("originator"))
	assert.NotEmpty(t, got.header.Get("session_id"))

	assert.Equal(t, false, got.body["store"], "the backend requires store=false")
	assert.Equal(t, "You are a pirate.\n\nAnswer briefly.", got.body["instructions"], "system messages move into instructions")
	assert.NotContains(t, got.body, "temperature", "sampling params are dropped")
	assert.NotContains(t, got.body, "max_output_tokens", "output caps are dropped")

	input, ok := got.body["input"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, input)
	for _, item := range input {
		msg, ok := item.(map[string]any)
		require.True(t, ok)
		assert.NotEqual(t, "system", msg["role"], "no system message remains in the input")
	}
}

func TestChatGPTIgnoresExplicitChatCompletionsAPIType(t *testing.T) {
	server, captured := startFakeCodexBackend(t)

	cfg := &latest.ModelConfig{
		Provider: "chatgpt",
		Model:    "gpt-5.2",
		BaseURL:  server.URL,
		TokenKey: chatgpt.TokenEnvVar,
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{
		chatgpt.TokenEnvVar: chatgptTestToken(t, "acc_1"),
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	drainChatStream(t, client, []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}})

	assert.Equal(t, "/responses", captured().path)
}

func TestChatGPTDefaultInstructionsWhenNoSystemMessage(t *testing.T) {
	server, captured := startFakeCodexBackend(t)

	cfg := &latest.ModelConfig{
		Provider: "chatgpt",
		Model:    "gpt-5.2",
		BaseURL:  server.URL,
		TokenKey: chatgpt.TokenEnvVar,
	}
	env := environment.NewMapEnvProvider(map[string]string{
		chatgpt.TokenEnvVar: chatgptTestToken(t, "acc_1"),
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	drainChatStream(t, client, []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}})

	assert.Equal(t, chatgptDefaultInstructions, captured().body["instructions"])
}

func TestChatGPTKeepsToolCallItemsInInput(t *testing.T) {
	server, captured := startFakeCodexBackend(t)

	cfg := &latest.ModelConfig{
		Provider: "chatgpt",
		Model:    "gpt-5.2",
		BaseURL:  server.URL,
		TokenKey: chatgpt.TokenEnvVar,
	}
	env := environment.NewMapEnvProvider(map[string]string{
		chatgpt.TokenEnvVar: chatgptTestToken(t, "acc_1"),
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	// A full agent turn: system + user + assistant tool call + tool result.
	drainChatStream(t, client, []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are an agent."},
		{Role: chat.MessageRoleUser, Content: "list files"},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: tools.FunctionCall{
				Name:      "shell",
				Arguments: `{"cmd":"ls"}`,
			},
		}}},
		{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "README.md"},
	})

	got := captured()
	assert.Equal(t, "You are an agent.", got.body["instructions"])

	input, ok := got.body["input"].([]any)
	require.True(t, ok)

	var types []string
	for _, item := range input {
		msg, ok := item.(map[string]any)
		require.True(t, ok)
		if typ, _ := msg["type"].(string); typ != "" {
			types = append(types, typ)
		}
		assert.NotEqual(t, "system", msg["role"], "no system message remains in the input")
	}
	// The tool-call exchange must survive the instructions extraction: the
	// backend rejects an orphaned function_call/function_call_output pair.
	assert.Contains(t, types, "function_call")
	assert.Contains(t, types, "function_call_output")
}

func TestChatGPTNotSignedInFailsFastWithGuidance(t *testing.T) {
	cfg := &latest.ModelConfig{
		Provider: "chatgpt",
		Model:    "gpt-5.2",
	}

	_, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), invocation.DockerAgent()+" setup")
	assert.Contains(t, err.Error(), chatgpt.TokenEnvVar)
}

func TestChatGPTFallsBackToStoredLogin(t *testing.T) {
	token := chatgptTestToken(t, "acc_stored")
	path := filepath.Join(t.TempDir(), "chatgpt-auth.json")
	creds := fmt.Sprintf(`{"access_token":%q,"expires_at":%q}`, token, time.Now().Add(time.Hour).Format(time.RFC3339))
	require.NoError(t, os.WriteFile(path, []byte(creds), 0o600))
	restore := chatgpt.SetCredentialsPathForTests(path)
	defer restore()

	server, captured := startFakeCodexBackend(t)

	cfg := &latest.ModelConfig{
		Provider: "chatgpt",
		Model:    "gpt-5.2",
		BaseURL:  server.URL,
	}

	// The env provider knows nothing about the token: the client falls back
	// to the stored login (embedders without the chatgpt-login source).
	client, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)

	drainChatStream(t, client, []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}})

	got := captured()
	assert.Equal(t, "Bearer "+token, got.header.Get("Authorization"))
	assert.Equal(t, "acc_stored", got.header.Get("chatgpt-account-id"))
}
