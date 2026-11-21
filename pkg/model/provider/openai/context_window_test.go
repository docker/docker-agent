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

// captureChatRequest sends one chat completion through the client and returns
// the raw request body, so tests can assert on the exact wire payload.
func captureChatRequest(t *testing.T, cfg latest.ModelConfig) []byte {
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

	cfg.BaseURL = server.URL
	cfg.TokenKey = "MY_TOKEN"
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})
	client, err := NewClient(t.Context(), &cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
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
	return body
}

// TestModelLevelContextWindowIsNotForwarded pins that the model-level context_window
// never reaches the request body: it is capacity metadata, not a parameter.
func TestModelLevelContextWindowIsNotForwarded(t *testing.T) {
	t.Parallel()

	size := int64(32_768)
	body := captureChatRequest(t, latest.ModelConfig{
		Provider:      "openai",
		Model:         "my-custom-model",
		ContextWindow: &size,
	})
	assert.NotContains(t, string(body), "context_window", "model-level context_window must not be forwarded")
	assert.NotContains(t, string(body), "32768", "the window value must not appear in the request")
}

// TestProviderOptsContextSizeIsIgnoredOnOpenAI pins that provider_opts.context_size
// is DMR-only: on an OpenAI-compatible provider it neither clamps max_tokens
// nor reaches the request body.
func TestProviderOptsContextSizeIsIgnoredOnOpenAI(t *testing.T) {
	t.Parallel()

	maxTokens := int64(262144)
	body := captureChatRequest(t, latest.ModelConfig{
		Provider:     "openai",
		Model:        "my-custom-model",
		MaxTokens:    &maxTokens,
		ProviderOpts: map[string]any{"context_size": 262144},
	})

	var req struct {
		MaxTokens *int64 `json:"max_tokens"`
	}
	require.NoError(t, json.Unmarshal(body, &req))
	require.NotNil(t, req.MaxTokens)
	assert.Equal(t, int64(262144), *req.MaxTokens, "a provider_opts window must not clamp a non-DMR request")
	assert.NotContains(t, string(body), "context_size")
}
