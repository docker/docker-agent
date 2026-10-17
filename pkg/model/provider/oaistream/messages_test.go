package oaistream

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestConvertMultiContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		multiContent []chat.MessagePart
		wantCount    int
	}{
		{
			name:         "empty",
			multiContent: []chat.MessagePart{},
			wantCount:    0,
		},
		{
			name: "text only",
			multiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "Hello"},
				{Type: chat.MessagePartTypeText, Text: "World"},
			},
			wantCount: 2,
		},
		{
			name: "with image",
			multiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "Check this out"},
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "http://example.com/img.png"}},
			},
			wantCount: 2,
		},
		{
			name: "image with nil URL produces no part",
			multiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeImageURL, ImageURL: nil},
			},
			wantCount: 0,
		},
		{
			// The converter forwards all parts as-is; normalizeMessageContent in the
			// session layer strips whitespace-only text parts before real usage.
			name: "whitespace-only text part forwarded as-is",
			multiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "   "},
				{Type: chat.MessagePartTypeText, Text: "hello"},
			},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ConvertMultiContent(t.Context(), tt.multiContent, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)
			assert.Len(t, result, tt.wantCount)
		})
	}
}

func TestConvertMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []chat.Message
		want     int // expected number of converted messages
	}{
		{
			name:     "empty",
			messages: []chat.Message{},
			want:     0,
		},
		{
			name: "system message",
			messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "You are helpful"},
			},
			want: 1,
		},
		{
			name: "user message",
			messages: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "Hello"},
			},
			want: 1,
		},
		{
			name: "assistant message",
			messages: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "Hi there!"},
			},
			want: 1,
		},
		{
			name: "skip empty assistant message",
			messages: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "   "},
			},
			want: 0,
		},
		{
			name: "assistant with tool calls not skipped",
			messages: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "", ToolCalls: []tools.ToolCall{{ID: "1"}}},
			},
			want: 1,
		},
		{
			name:     "reasoning-only assistant message not skipped",
			messages: []chat.Message{{Role: chat.MessageRoleAssistant, ReasoningContent: "thinking"}},
			want:     1,
		},
		{
			name:     "whitespace-only reasoning assistant message skipped",
			messages: []chat.Message{{Role: chat.MessageRoleAssistant, ReasoningContent: "   "}},
			want:     0,
		},
		{
			name: "tool message",
			messages: []chat.Message{
				{Role: chat.MessageRoleTool, Content: "Result", ToolCallID: "call_123"},
			},
			want: 1,
		},
		{
			name: "full conversation",
			messages: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "You are helpful"},
				{Role: chat.MessageRoleUser, Content: "Hello"},
				{Role: chat.MessageRoleAssistant, Content: "Hi!"},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ConvertMessages(t.Context(), tt.messages, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)
			assert.Len(t, result, tt.want)
		})
	}
}

func TestMergeConsecutiveMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []openai.ChatCompletionMessageParamUnion
		want     int
	}{
		{
			name:     "empty",
			messages: []openai.ChatCompletionMessageParamUnion{},
			want:     0,
		},
		{
			name: "single message",
			messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage("Hello"),
			},
			want: 1,
		},
		{
			name: "two consecutive system messages",
			messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage("You are helpful"),
				openai.SystemMessage("Be concise"),
			},
			want: 1,
		},
		{
			name: "two consecutive user messages",
			messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("Hello"),
				openai.UserMessage("World"),
			},
			want: 1,
		},
		{
			name: "alternating roles",
			messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage("System"),
				openai.UserMessage("User"),
				openai.UserMessage("User2"),
			},
			want: 2, // System merged alone, two users merged
		},
		{
			name: "no merging needed",
			messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage("System"),
				openai.UserMessage("User"),
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := MergeConsecutiveMessages(tt.messages)
			assert.Len(t, result, tt.want)
		})
	}
}

func TestJSONSchema_MarshalJSON(t *testing.T) {
	t.Parallel()

	schema := JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type": "string",
			},
		},
	}

	data, err := schema.MarshalJSON()
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"object"`)
	assert.Contains(t, string(data), `"properties"`)
}

func TestConvertToolImagesAsText(t *testing.T) {
	t.Parallel()
	const imageURL = "data:image/png;base64,AQID"
	tests := []struct {
		name string
		part chat.MessagePart
		want string
	}{
		{name: "image URL", part: chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: imageURL}}, want: imageURL},
		{name: "image document", part: chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "image.png", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1, 2, 3}}}}, want: imageURL},
		{name: "nil image URL", part: chat.MessagePart{Type: chat.MessagePartTypeImageURL}},
		{name: "nil document", part: chat.MessagePart{Type: chat.MessagePartTypeDocument}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			messages := []chat.Message{
				{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "A"}, {ID: "B"}}},
				{Role: chat.MessageRoleTool, ToolCallID: "A", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "Read image file"}, tt.part}},
				{Role: chat.MessageRoleTool, ToolCallID: "B", Content: "Done"},
			}
			result := ConvertMessagesWithCaps(t.Context(), messages, modelinfo.CapsWith(true, true, false, false))
			require.Len(t, result, 3)
			require.NotNil(t, result[1].OfTool)
			assert.Equal(t, "A", result[1].OfTool.ToolCallID)
			parts := result[1].OfTool.Content.OfArrayOfContentParts
			if tt.want == "" {
				require.Len(t, parts, 1)
			} else {
				require.Len(t, parts, 2)
				assert.Equal(t, tt.want, parts[1].Text)
			}
			assert.Equal(t, "Read image file", parts[0].Text)
			require.NotNil(t, result[2].OfTool)
			assert.Equal(t, "B", result[2].OfTool.ToolCallID)
		})
	}
}

// TestConvertMessages_ReasoningContentWireFormat covers issue #4363. Assertions are on
// the serialized request body, not on the SDK struct: a field that openai-go omits is
// invisible to struct-level assertions, and llama.cpp rejects an assistant message that
// carries neither content nor tool_calls.
func TestConvertMessages_ReasoningContentWireFormat(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})

	tests := []struct {
		name string
		msg  chat.Message
		want map[string]any
	}{
		{
			// A reasoning model that exhausts its output budget mid-reasoning leaves an
			// assistant turn with no content and no tool calls. Dropping it loses the
			// reasoning; keeping it with no content is rejected by the backend.
			name: "reasoning-only turn replays reasoning with empty content",
			msg: chat.Message{
				Role:             chat.MessageRoleAssistant,
				ReasoningContent: "the answer requires two steps",
			},
			want: map[string]any{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "the answer requires two steps",
			},
		},
		{
			name: "reasoning alongside text",
			msg: chat.Message{
				Role:             chat.MessageRoleAssistant,
				Content:          "Paris.",
				ReasoningContent: "France's capital, from memory",
			},
			want: map[string]any{
				"role":              "assistant",
				"content":           "Paris.",
				"reasoning_content": "France's capital, from memory",
			},
		},
		{
			// content stays absent here: tool_calls satisfies the backend's requirement.
			name: "reasoning alongside tool calls",
			msg: chat.Message{
				Role:             chat.MessageRoleAssistant,
				ReasoningContent: "I should call the weather tool",
				ToolCalls: []tools.ToolCall{{
					ID: "call_1",
					Function: tools.FunctionCall{
						Name:      "weather",
						Arguments: `{"city":"Paris"}`,
					},
				}},
			},
			want: map[string]any{
				"role":              "assistant",
				"reasoning_content": "I should call the weather tool",
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "weather",
						"arguments": `{"city":"Paris"}`,
					},
				}},
			},
		},
		{
			name: "no stored reasoning adds no extension",
			msg:  chat.Message{Role: chat.MessageRoleAssistant, Content: "Hi there!"},
			want: map[string]any{
				"role":    "assistant",
				"content": "Hi there!",
			},
		},
		{
			name: "whitespace-only reasoning is not replayed",
			msg: chat.Message{
				Role:             chat.MessageRoleAssistant,
				Content:          "Hi there!",
				ReasoningContent: "   ",
			},
			want: map[string]any{
				"role":    "assistant",
				"content": "Hi there!",
			},
		},
		{
			name: "reasoning is replayed on a multi-content turn",
			msg: chat.Message{
				Role:             chat.MessageRoleAssistant,
				MultiContent:     []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "answer"}},
				ReasoningContent: "reasoning",
			},
			want: map[string]any{
				"role":              "assistant",
				"content":           []any{map[string]any{"type": "text", "text": "answer"}},
				"reasoning_content": "reasoning",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := ConvertMessages(t.Context(), []chat.Message{tt.msg}, modelsdev.ID{}, store, nil)
			require.Len(t, result, 1)
			require.NotNil(t, result[0].OfAssistant)

			data, err := json.Marshal(result[0])
			require.NoError(t, err)

			var got map[string]any
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, tt.want, got, "serialized assistant message: %s", data)
		})
	}
}

// TestConvertMessages_AssistantAlwaysMarshalable asserts every assistant turn that
// survives conversion serializes to a shape llama.cpp accepts: either content or
// tool_calls must be present.
func TestConvertMessages_AssistantAlwaysMarshalable(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})

	messages := []chat.Message{
		{Role: chat.MessageRoleAssistant, ReasoningContent: "thinking"},
		{Role: chat.MessageRoleAssistant, Content: "text"},
		{Role: chat.MessageRoleAssistant, MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "http://example.com/i.png"}}}},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call_1", Function: tools.FunctionCall{Name: "f", Arguments: "{}"}}}},
	}

	result := ConvertMessages(t.Context(), messages, modelsdev.ID{}, store, nil)
	require.Len(t, result, len(messages))

	for i, converted := range result {
		data, err := json.Marshal(converted)
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal(data, &got))
		_, hasContent := got["content"]
		_, hasToolCalls := got["tool_calls"]
		assert.True(t, hasContent || hasToolCalls,
			"message %d rejected by llama.cpp: assistant message must contain either 'content' or 'tool_calls', got %s", i, data)
	}
}
