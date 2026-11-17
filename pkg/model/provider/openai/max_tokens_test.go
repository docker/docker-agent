package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// TestChatCompletions_ClampsMaxTokensToContextWindow is a regression test for
// https://github.com/docker/docker-agent/issues/3387. A self-hosted vLLM user
// set max_tokens equal to the model's context window (262144). vLLM requires
// prompt_tokens + max_tokens to fit the window, so reserving the whole window
// for output leaves no room for the prompt and even a "hello" is rejected with
// a "maximum context length" error. When the window is known (here via the
// model-level context_window) the client must clamp max_tokens to leave headroom
// for the prompt.
func TestChatCompletions_ClampsMaxTokensToContextWindow(t *testing.T) {
	t.Parallel()

	got, ok := requestMaxTokens(t, 262144, 262144)
	require.True(t, ok, "max_tokens must be present in the request")
	assert.Equal(t, int64(262144-1024), got, "max_tokens must be clamped to leave prompt headroom (see #3387)")
}

// TestChatCompletions_DoesNotClampWhenWindowUnknown documents the reporter's
// actual state: the model is not in models.dev and no context_window is set, so
// the window is unknown at request time. The client must NOT fabricate a window
// and must forward max_tokens verbatim; surfacing the misconfiguration is left
// to the load-time warning and the user setting context_window.
func TestChatCompletions_DoesNotClampWhenWindowUnknown(t *testing.T) {
	t.Parallel()

	got, ok := requestMaxTokens(t, 262144, 0)
	require.True(t, ok)
	assert.Equal(t, int64(262144), got, "with no discoverable context window, max_tokens must be sent verbatim")
}

// TestChatCompletions_DoesNotClampMaxTokensBelowWindow ensures a sensible
// output cap passes through untouched when a window is known.
func TestChatCompletions_DoesNotClampMaxTokensBelowWindow(t *testing.T) {
	t.Parallel()

	got, ok := requestMaxTokens(t, 4096, 262144)
	require.True(t, ok)
	assert.Equal(t, int64(4096), got, "a reasonable max_tokens must pass through unchanged")
}

func TestClampMaxTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured int64
		window     int64
		want       int64
	}{
		{"unknown window passes through", 262144, 0, 262144},
		{"negative window passes through", 500, -1, 500},
		{"equal to window is clamped", 262144, 262144, 262144 - 1024},
		{"above window minus headroom is clamped", 300000, 262144, 262144 - 1024},
		{"below window minus headroom unchanged", 4096, 262144, 4096},
		{"window smaller than headroom floors at 1", 100, 512, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, clampMaxTokens(t.Context(), tt.configured, tt.window, "test-model"))
		})
	}
}

// requestMaxTokens drives a chat-completions request (against a mock server)
// for a self-hosted openai-compatible model configured with the given
// max_tokens and provider_opts, and returns the max_tokens field observed in
// the request body (and whether it was present at all).
func requestMaxTokens(t *testing.T, maxTokens, contextWindow int64) (int64, bool) {
	t.Helper()

	var (
		body []byte
		mu   sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		writeSSEResponse(w)
	}))
	defer server.Close()

	requestCfg := latest.ModelConfig{
		Provider:  "openai",
		Model:     "Qwen/Qwen3.6-35B",
		TokenKey:  "MY_TOKEN",
		BaseURL:   server.URL,
		MaxTokens: &maxTokens,
	}
	if contextWindow > 0 {
		requestCfg.ContextWindow = &contextWindow
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), &requestCfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, body, "chat/completions should have been called")

	var req struct {
		MaxTokens *int64 `json:"max_tokens"`
	}
	require.NoError(t, json.Unmarshal(body, &req))
	if req.MaxTokens == nil {
		return 0, false
	}
	return *req.MaxTokens, true
}
