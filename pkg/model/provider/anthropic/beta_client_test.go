package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// optionsFromStore builds ModelOptions carrying the given models.dev store.
func optionsFromStore(store *modelsdev.Store) options.ModelOptions {
	var mo options.ModelOptions
	options.WithModelsDevStore(store)(&mo)
	return mo
}

// TestCountAnthropicTokensBeta_Success tests successful token counting for beta API
func TestCountAnthropicTokensBeta_Success(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/messages/count_tokens", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("content-type"))
		assert.NotEmpty(t, r.Header.Get("x-api-key"))

		var payload map[string]any
		err := json.NewDecoder(r.Body).Decode(&payload)
		assert.NoError(t, err)
		assert.Equal(t, "claude-3-5-sonnet-20241022", payload["model"])
		assert.NotNil(t, payload["messages"])

		// Return mock response
		w.Header().Set("content-type", "application/json")
		err = json.NewEncoder(w).Encode(map[string]int64{"input_tokens": 150})
		assert.NoError(t, err)
	}))
	defer server.Close()

	messages := []MessageParam{
		{
			Role:    "user",
			Content: []ContentBlockParam{newTextBlock("Hello")},
		},
	}
	system := []TextBlockParam{
		{Text: "You are helpful"},
	}

	client := NewAPIClient(APIClientConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, nil)

	require.NoError(t, err)
	assert.Equal(t, int64(150), tokens)
}

func TestCountAnthropicTokensBeta_NoAPIKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "invalid api key"}}`))
	}))
	defer server.Close()

	var messages []MessageParam
	var system []TextBlockParam
	zero := 0
	client := NewAPIClient(APIClientConfig{
		APIKey:     "invalid-key",
		BaseURL:    server.URL,
		MaxRetries: &zero,
	})

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, nil)

	require.Error(t, err)
	assert.Equal(t, int64(0), tokens)
}

func TestCountAnthropicTokensBeta_ServerError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var messages []MessageParam
	var system []TextBlockParam
	zero := 0
	client := NewAPIClient(APIClientConfig{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		MaxRetries: &zero,
	})

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, nil)
	require.Error(t, err)
	assert.Equal(t, int64(0), tokens)
}

func TestCountAnthropicTokensBeta_WithTools(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		err := json.NewDecoder(r.Body).Decode(&payload)
		assert.NoError(t, err)

		assert.NotNil(t, payload["tools"])
		tools, ok := payload["tools"].([]any)
		assert.True(t, ok)
		assert.Len(t, tools, 1)

		w.Header().Set("content-type", "application/json")
		err = json.NewEncoder(w).Encode(map[string]int64{"input_tokens": 200})
		assert.NoError(t, err)
	}))
	defer server.Close()

	var messages []MessageParam
	var system []TextBlockParam
	tools := []ToolParam{
		{
			Name:        "test_tool",
			Description: new("A test tool"),
		},
	}

	client := NewAPIClient(APIClientConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, tools)

	require.NoError(t, err)
	assert.Equal(t, int64(200), tokens)
}

// TestClampMaxTokens_WithinLimit tests clamping when configured tokens are within limit
func TestClampMaxTokens_WithinLimit(t *testing.T) {
	t.Parallel()
	// Context limit: 200k, used: 50k, safety: 1k, remaining: 149k
	// Configured: 8k (within limit)
	result := clampMaxTokens(200000, 50000, 8000)
	assert.Equal(t, int64(8000), result)
}

// TestClampMaxTokens_ExceedsLimit tests clamping when configured tokens exceed remaining
func TestClampMaxTokens_ExceedsLimit(t *testing.T) {
	t.Parallel()
	// Context limit: 200k, used: 190k, safety: 1024, remaining: 8976
	// Configured: 16k (exceeds limit)
	result := clampMaxTokens(200000, 190000, 16000)
	assert.Equal(t, int64(8976), result)
}

// TestClampMaxTokens_MinimumOne tests clamping never returns less than 1
func TestClampMaxTokens_MinimumOne(t *testing.T) {
	t.Parallel()
	// Context limit: 200k, used: 199k, safety: 1k, remaining: 0 (would be negative)
	result := clampMaxTokens(200000, 199000, 8000)
	assert.Equal(t, int64(1), result)
}

// TestClampMaxTokens_ExactlyAtLimit tests clamping when used + safety equals limit
func TestClampMaxTokens_ExactlyAtLimit(t *testing.T) {
	t.Parallel()
	// Context limit: 200k, used: 199k, safety: 1k, remaining: 0
	result := clampMaxTokens(200000, 199000, 1000)
	assert.Equal(t, int64(1), result)
}

// TestContextLimit_FromModelsDev verifies the client prefers the models.dev
// context window and falls back to the conservative Claude 200k floor when the
// catalogue has no entry. Model-specific large windows (Fable, Opus 4.6+) come
// from the catalogue/snapshot, not from hard-coded name patterns.
func TestContextLimit_FromModelsDev(t *testing.T) {
	t.Parallel()
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-5": {Limit: modelsdev.Limit{Context: 1000000}},
					"claude-fable-5":    {Limit: modelsdev.Limit{Context: 1000000}},
				},
			},
		},
	})

	withStore := &Client{Config: base.Config{
		ModelConfig:  latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"},
		ModelOptions: optionsFromStore(store),
	}}
	assert.Equal(t, int64(1000000), withStore.contextLimit(t.Context()))

	// Unknown model falls back to the conservative Claude window.
	unknown := &Client{Config: base.Config{
		ModelConfig:  latest.ModelConfig{Provider: "anthropic", Model: "claude-future"},
		ModelOptions: optionsFromStore(store),
	}}
	assert.Equal(t, int64(modelinfo.DefaultAnthropicContextLimit), unknown.contextLimit(t.Context()))

	// Fable model: its 1M window is sourced from the catalogue, not a name pattern.
	fable := &Client{Config: base.Config{
		ModelConfig:  latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5"},
		ModelOptions: optionsFromStore(store),
	}}
	assert.Equal(t, int64(1_000_000), fable.contextLimit(t.Context()))

	// No store configured: conservative Claude window.
	noStore := &Client{Config: base.Config{
		ModelConfig: latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}}
	assert.Equal(t, int64(modelinfo.DefaultAnthropicContextLimit), noStore.contextLimit(t.Context()))
}

// TestExtractBetaSystemBlocks_SingleSystemMessage tests extracting system messages
func TestExtractBetaSystemBlocks_SingleSystemMessage(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: "You are a helpful assistant",
		},
	}

	blocks := extractSystemBlocks(msgs)

	require.Len(t, blocks, 1)
	assert.Equal(t, "You are a helpful assistant", blocks[0].Text)
}

// TestExtractBetaSystemBlocks_MultipleSystemMessages tests extracting multiple system messages
func TestExtractBetaSystemBlocks_MultipleSystemMessages(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: "You are helpful",
		},
		{
			Role:    chat.MessageRoleUser,
			Content: "Hello",
		},
		{
			Role:    chat.MessageRoleSystem,
			Content: "Be concise",
		},
	}

	blocks := extractSystemBlocks(msgs)

	require.Len(t, blocks, 2)
	assert.Equal(t, "You are helpful", blocks[0].Text)
	assert.Equal(t, "Be concise", blocks[1].Text)
}

// TestExtractBetaSystemBlocks_SkipsEmptyText tests that empty system text is skipped.
// System blocks are trimmed because YAML literal-block instructions always append a trailing newline.
func TestExtractBetaSystemBlocks_SkipsEmptyText(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: "   \n\t  ",
		},
		{
			Role:    chat.MessageRoleSystem,
			Content: "Valid system prompt",
		},
	}

	blocks := extractSystemBlocks(msgs)

	require.Len(t, blocks, 1)
	assert.Equal(t, "Valid system prompt", blocks[0].Text)
}

// TestExtractBetaSystemBlocks_MultiContent tests extracting from multi-content system messages
func TestExtractBetaSystemBlocks_MultiContent(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role: chat.MessageRoleSystem,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "Part 1"},
				{Type: chat.MessagePartTypeText, Text: "Part 2"},
			},
		},
	}

	blocks := extractSystemBlocks(msgs)

	require.Len(t, blocks, 2)
	assert.Equal(t, "Part 1", blocks[0].Text)
	assert.Equal(t, "Part 2", blocks[1].Text)
}

// TestConvertBetaMessages_UserMessage tests converting user messages
func TestConvertBetaMessages_UserMessage(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleUser,
			Content: "Hello, assistant!",
		},
	}

	converted, err := testClient().convertBetaMessages(t.Context(), msgs)
	require.NoError(t, err)

	require.Len(t, converted, 1)
	assert.Equal(t, "user", converted[0].Role)
	require.Len(t, converted[0].Content, 1)
}

// TestConvertBetaMessages_SkipsSystemMessages tests that system messages are skipped
func TestConvertBetaMessages_SkipsSystemMessages(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: "System prompt",
		},
		{
			Role:    chat.MessageRoleUser,
			Content: "User message",
		},
	}

	converted, err := testClient().convertBetaMessages(t.Context(), msgs)
	require.NoError(t, err)

	require.Len(t, converted, 1)
	assert.Equal(t, "user", converted[0].Role)
}

// TestConvertBetaMessages_AssistantMessage tests converting assistant messages
func TestConvertBetaMessages_AssistantMessage(t *testing.T) {
	t.Parallel()
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "I can help with that",
		},
	}

	converted, err := testClient().convertBetaMessages(t.Context(), msgs)
	require.NoError(t, err)

	require.Len(t, converted, 1)
	assert.Equal(t, "assistant", converted[0].Role)
}
