package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

// testClient creates a minimal Client for testing convertMessages.
func testClient() *Client {
	return &Client{}
}

func TestCreateChatCompletionStream_ErrorOnEmptyMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("request should not have been sent")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-sonnet-4-5-20250929",
			},
		},
		clientFn: func(_ context.Context) (anthropic.Client, error) {
			return anthropic.NewClient(
				option.WithAPIKey("test-key"),
				option.WithBaseURL(server.URL),
			), nil
		},
	}

	tests := []struct {
		name     string
		messages []chat.Message
	}{
		{
			name:     "nil messages",
			messages: nil,
		},
		{
			name:     "empty messages",
			messages: []chat.Message{},
		},
		{
			name: "only system messages",
			messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "You are helpful."},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.CreateChatCompletionStream(t.Context(), tt.messages, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no messages to send after conversion")
		})
	}
}

// TestConvertMessages_SkipEmptySystemText documents that the converter no longer
// filters whitespace-only system messages — normalizeMessageContent in the session
// layer does this before messages reach any provider converter.
func TestConvertMessages_SkipEmptySystemText(t *testing.T) {
	msgs := []chat.Message{{
		Role:    chat.MessageRoleSystem,
		Content: "   \n\t  ",
	}}

	// System messages are extracted into system blocks by extractSystemBlocks;
	// that helper now passes them through as-is. The session layer is
	// responsible for not sending whitespace-only system messages.
	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	// System messages are not included in the anthropic message list (they go
	// to extractSystemBlocks instead), so out is still empty.
	assert.Empty(t, out)
}

// TestConvertMessages_SkipEmptyUserText_NoMultiContent documents that whitespace
// filtering is now the session layer's responsibility, not the converter's.
func TestConvertMessages_SkipEmptyUserText_NoMultiContent(t *testing.T) {
	msgs := []chat.Message{{
		Role:    chat.MessageRoleUser,
		Content: "   \n\t  ",
	}}

	// The converter forwards the message as-is; normalizeMessageContent in the
	// session layer drops it before it reaches the converter in real usage.
	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestConvertMessages_UserMultiContent_SkipEmptyText_KeepImage(t *testing.T) {
	msgs := []chat.Message{{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "   "},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,AAAA"}},
		},
	}}

	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "user", m["role"])
	// The converter now forwards all parts as-is. normalizeMessageContent in the
	// session layer strips whitespace-only text parts before calling the converter
	// in real usage, so both parts appear here when tested directly.
	content, ok := m["content"].([]any)
	require.True(t, ok)
	assert.Len(t, content, 2)
}

// TestConvertMessages_SkipEmptyAssistantText_NoToolCalls documents that the
// converter no longer filters whitespace-only assistant messages.
func TestConvertMessages_SkipEmptyAssistantText_NoToolCalls(t *testing.T) {
	msgs := []chat.Message{{
		Role:    chat.MessageRoleAssistant,
		Content: "  \t\n  ",
	}}

	// The converter forwards the message; normalizeMessageContent in the session
	// layer drops whitespace-only assistant messages before they reach here.
	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestConvertMessages_AssistantToolCalls_NoText_IncludesToolUse(t *testing.T) {
	msgs := []chat.Message{{
		Role:    chat.MessageRoleAssistant,
		Content: "   ",
		ToolCalls: []tools.ToolCall{
			{ID: "tool-1", Function: tools.FunctionCall{Name: "do_thing", Arguments: "{\"x\":1}"}},
		},
	}}

	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "assistant", m["role"])
	content, ok := m["content"].([]any)
	require.True(t, ok)
	// The whitespace text content is now included alongside the tool_use block
	// because the converter no longer strips it. The session layer would have
	// already cleaned this up in real usage via normalizeMessageContent.
	assert.Len(t, content, 2)
	cb, ok := content[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "tool_use", cb["type"])
}

func TestSystemMessages_AreExtractedAndNotInMessageList(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "  system rules here  "},
		{Role: chat.MessageRoleUser, Content: "hi"},
	}

	// System blocks should be extracted
	sys := extractSystemBlocks(msgs)
	require.Len(t, sys, 1)
	assert.Equal(t, "system rules here", strings.TrimSpace(sys[0].Text))

	// System role messages must not appear in the anthropic messages list
	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestSystemMessages_MultipleExtractedAndExcludedFromMessageList(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: " sys A "},
		{Role: chat.MessageRoleSystem, Content: "\n sys B \t"},
		{Role: chat.MessageRoleUser, Content: "hello"},
	}

	sys := extractSystemBlocks(msgs)
	require.Len(t, sys, 2)
	assert.Equal(t, "sys A", strings.TrimSpace(sys[0].Text))
	assert.Equal(t, "sys B", strings.TrimSpace(sys[1].Text))

	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestSystemMessages_InterspersedExtractedAndExcluded(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: " S1 "},
		{Role: chat.MessageRoleUser, Content: "U1"},
		{Role: chat.MessageRoleAssistant, Content: "A1"},
		{Role: chat.MessageRoleSystem, Content: "S2"},
		{Role: chat.MessageRoleUser, Content: " U2 "},
	}

	// All system messages should be extracted in order of appearance
	sys := extractSystemBlocks(msgs)
	require.Len(t, sys, 2)
	assert.Equal(t, "S1", strings.TrimSpace(sys[0].Text))
	assert.Equal(t, "S2", strings.TrimSpace(sys[1].Text))

	// Converted messages must exclude system roles and preserve order of others
	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 3)
	expectedRoles := []string{"user", "assistant", "user"}
	for i, expected := range expectedRoles {
		b, err := json.Marshal(out[i])
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		assert.Equal(t, expected, m["role"])
	}
}

func TestConvertMessages_GroupToolResults_AfterAssistantToolUse(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "start"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "tool-1", Function: tools.FunctionCall{Name: "t1", Arguments: "{}"}},
				{ID: "tool-2", Function: tools.FunctionCall{Name: "t2", Arguments: "{}"}},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "tool-1", Content: "r1"},
		{Role: chat.MessageRoleTool, ToolCallID: "tool-2", Content: "r2"},
		{Role: chat.MessageRoleUser, Content: "ok"},
	}

	converted, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	// Expect: user(start), assistant(tool_use), user(grouped tool_result), user(ok)
	require.Len(t, converted, 4)

	b, err := json.Marshal(converted[2])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "user", m["role"])
	content, ok := m["content"].([]any)
	require.True(t, ok)

	// Collect tool_result IDs
	ids := make(map[string]struct{})
	for _, c := range content {
		if cb, ok := c.(map[string]any); ok {
			if cb["type"] == "tool_result" {
				if id, _ := cb["tool_use_id"].(string); id != "" {
					ids[id] = struct{}{}
				}
			}
		}
	}
	assert.Contains(t, ids, "tool-1")
	assert.Contains(t, ids, "tool-2")
}

// TestCountAnthropicTokens_Success tests successful token counting for standard API
func TestCountAnthropicTokens_Success(t *testing.T) {
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

	messages := []anthropic.MessageParam{
		{
			Role: anthropic.MessageParamRoleUser,
			Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "Hello"}},
			},
		},
	}
	system := []anthropic.TextBlockParam{
		{Text: "You are helpful"},
	}

	client := anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL),
	)

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, nil)

	require.NoError(t, err)
	assert.Equal(t, int64(150), tokens)
}

// TestCountAnthropicTokens_NoAPIKey tests error when API key is missing
func TestCountAnthropicTokens_NoAPIKey(t *testing.T) {
	// Use a test server that returns 401 Unauthorized
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "invalid api key"}}`))
	}))
	defer server.Close()

	var messages []anthropic.MessageParam
	var system []anthropic.TextBlockParam

	client := anthropic.NewClient(
		option.WithAPIKey("invalid-key"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0), // Disable retries to speed up test
	)

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, nil)

	require.Error(t, err)
	assert.Equal(t, int64(0), tokens)
}

// TestCountAnthropicTokens_ServerError tests error handling for server errors
func TestCountAnthropicTokens_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var messages []anthropic.MessageParam
	var system []anthropic.TextBlockParam

	client := anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0), // Disable retries to speed up test
	)

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, nil)

	require.Error(t, err)
	assert.Equal(t, int64(0), tokens)
}

// TestCountAnthropicTokens_WithTools tests token counting includes tools
func TestCountAnthropicTokens_WithTools(t *testing.T) {
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

	var messages []anthropic.MessageParam
	var system []anthropic.TextBlockParam
	aiTools := []anthropic.ToolUnionParam{
		{OfTool: &anthropic.ToolParam{
			Name:        "test_tool",
			Description: anthropic.String("A test tool"),
		}},
	}

	client := anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL),
	)

	tokens, err := countAnthropicTokens(t.Context(), client, "claude-3-5-sonnet-20241022", messages, system, aiTools)

	require.NoError(t, err)
	assert.Equal(t, int64(200), tokens)
}

// TestExtractSystemBlocks_SingleSystemMessage tests extracting system messages
func TestExtractSystemBlocks_SingleSystemMessage(t *testing.T) {
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

// TestExtractSystemBlocks_MultipleSystemMessages tests extracting multiple system messages
func TestExtractSystemBlocks_MultipleSystemMessages(t *testing.T) {
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

// TestExtractSystemBlocks_SkipsEmptyText tests that empty/whitespace-only system text is skipped.
// System blocks are trimmed because YAML literal-block instructions (instruction: |) always
// append a trailing newline that should not be sent to the API.
func TestExtractSystemBlocks_SkipsEmptyText(t *testing.T) {
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

// TestExtractSystemBlocks_MultiContent tests extracting from multi-content system messages
func TestExtractSystemBlocks_MultiContent(t *testing.T) {
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

func TestExtractSystemBlocksCacheControl(t *testing.T) {
	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: "instructions",
		},
		{
			Role:         chat.MessageRoleSystem,
			Content:      "tools",
			CacheControl: true,
		},
		{
			Role:    chat.MessageRoleSystem,
			Content: "date",
		},
		{
			Role:         chat.MessageRoleSystem,
			Content:      "last",
			CacheControl: true,
		},
	}

	blocks := extractSystemBlocks(msgs)

	require.Len(t, blocks, 4)
	assert.Equal(t, "instructions", blocks[0].Text)
	assert.Empty(t, string(blocks[0].CacheControl.Type))
	assert.Empty(t, string(blocks[0].CacheControl.TTL))

	assert.Equal(t, "tools", blocks[1].Text)
	assert.Equal(t, "ephemeral", string(blocks[1].CacheControl.Type))
	assert.Empty(t, string(blocks[1].CacheControl.TTL))

	assert.Equal(t, "date", blocks[2].Text)
	assert.Empty(t, string(blocks[2].CacheControl.Type))
	assert.Empty(t, string(blocks[2].CacheControl.TTL))

	assert.Equal(t, "last", blocks[3].Text)
	assert.Equal(t, "ephemeral", string(blocks[3].CacheControl.Type))
	assert.Empty(t, string(blocks[3].CacheControl.TTL))
}

func TestExtractSystemBlocks_EmptyContentWithCacheControl(t *testing.T) {
	t.Parallel()

	// An empty system message with CacheControl must not panic.
	// Since extractSystemBlocks now trims system content, an empty/whitespace-only
	// message produces no block, so CacheControl has nothing to apply to.
	msgs := []chat.Message{
		{
			Role:         chat.MessageRoleSystem,
			Content:      "",
			CacheControl: true,
		},
	}

	// Must not panic; the empty block is skipped.
	blocks := extractSystemBlocks(msgs)
	assert.Empty(t, blocks)
}
