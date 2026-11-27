package oaistream

import (
	"encoding/json"
	"testing"

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
		messages []ChatMessage
		want     int
	}{
		{
			name:     "empty",
			messages: []ChatMessage{},
			want:     0,
		},
		{
			name: "single message",
			messages: []ChatMessage{
				SystemMessage("Hello"),
			},
			want: 1,
		},
		{
			name: "two consecutive system messages",
			messages: []ChatMessage{
				SystemMessage("You are helpful"),
				SystemMessage("Be concise"),
			},
			want: 1,
		},
		{
			name: "two consecutive user messages",
			messages: []ChatMessage{
				UserMessage("Hello"),
				UserMessage("World"),
			},
			want: 1,
		},
		{
			name: "alternating roles",
			messages: []ChatMessage{
				SystemMessage("System"),
				UserMessage("User"),
				UserMessage("User2"),
			},
			want: 2, // System merged alone, two users merged
		},
		{
			name: "no merging needed",
			messages: []ChatMessage{
				SystemMessage("System"),
				UserMessage("User"),
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

func TestMergeMixedContentPreservesOrder(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"user", "system"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			var messages []ChatMessage
			if role == "user" {
				messages = []ChatMessage{
					UserMessage("before"),
					UserMultiMessage([]ContentPart{TextContentPart("part one")}),
					UserMessage("between"),
					UserMultiMessage([]ContentPart{TextContentPart("part two")}),
					UserMessage("after"),
				}
			} else {
				messages = []ChatMessage{
					SystemMessage("before"),
					SystemMultiMessage([]ContentPart{TextContentPart("part one")}),
					SystemMessage("between"),
					SystemMultiMessage([]ContentPart{TextContentPart("part two")}),
					SystemMessage("after"),
				}
			}
			merged := MergeConsecutiveMessages(messages)
			require.Len(t, merged, 1)
			var texts []string
			for _, part := range merged[0].ContentParts {
				texts = append(texts, part.Text)
			}
			assert.Equal(t, []string{"before", "part one", "between", "part two", "after"}, texts)
		})
	}
}

func TestMergeConsecutiveMessages_ExactContent(t *testing.T) {
	t.Parallel()

	merged := MergeConsecutiveMessages([]ChatMessage{
		SystemMessage("first system"),
		SystemMessage("second system"),
		UserMessage("first user"),
		UserMessage("second user"),
	})

	require.Len(t, merged, 2)
	assert.Equal(t, "system", merged[0].Role)
	assert.Equal(t, "first system\nsecond system", *merged[0].Content)
	assert.Equal(t, "user", merged[1].Role)
	assert.Equal(t, "first user\nsecond user", *merged[1].Content)
}

func TestMergeConsecutiveMessages_KeepsAssistantAndToolTurns(t *testing.T) {
	t.Parallel()

	assistant := ChatMessage{
		Role:    "assistant",
		Content: new("calling tools"),
		ToolCalls: []ToolCallParam{{
			ID:   "call-a",
			Type: "function",
			Function: FunctionCallParam{
				Name: "read_file", Arguments: `{"path":"a"}`,
			},
		}},
	}
	toolA := ChatMessage{
		Role:       "tool",
		ToolCallID: "call-a",
		Content:    new("result a"),
	}
	toolB := ChatMessage{
		Role:       "tool",
		ToolCallID: "call-b",
		Content:    new("result b"),
	}

	input := []ChatMessage{assistant, toolA, toolB}
	merged := MergeConsecutiveMessages(input)

	require.Len(t, merged, len(input))
	for i := range input {
		assert.JSONEq(t, mustMarshalMessage(t, input[i]), mustMarshalMessage(t, merged[i]))
	}
}

func TestMergeConsecutiveMessages_DoesNotHoistLateSystem(t *testing.T) {
	t.Parallel()

	input := []ChatMessage{
		SystemMessage("leading"),
		UserMessage("question"),
		SystemMessage("late"),
		UserMessage("follow-up"),
	}
	merged := MergeConsecutiveMessages(input)

	require.Len(t, merged, len(input))
	for i := range input {
		assert.JSONEq(t, mustMarshalMessage(t, input[i]), mustMarshalMessage(t, merged[i]))
	}
}

func TestMergeConsecutiveMessages_PreservesMultimodalOrder(t *testing.T) {
	t.Parallel()

	const imageURL = "https://example.com/image.png"
	first := UserMultiMessage([]ContentPart{
		TextContentPart("before"),
		ImageContentPart(ImageURLParam{URL: imageURL, Detail: "high"}),
	})
	second := UserMessage("after")

	merged := MergeConsecutiveMessages([]ChatMessage{first, second})
	require.Len(t, merged, 1)
	assert.Equal(t, "user", merged[0].Role)

	parts := merged[0].ContentParts
	require.Len(t, parts, 3)
	assert.Equal(t, "text", parts[0].Type)
	assert.Equal(t, "before", parts[0].Text)
	require.NotNil(t, parts[1].ImageURL)
	assert.Equal(t, imageURL, parts[1].ImageURL.URL)
	assert.Equal(t, "text", parts[2].Type)
	assert.Equal(t, "after", parts[2].Text)
}

func TestMergeConsecutiveMessages_PreservesSingletonMetadata(t *testing.T) {
	t.Parallel()

	system := SystemMessage("system with metadata")
	system.ExtraFields = map[string]any{"scope": "agent"}
	user := UserMessage("user with name")
	user.Name = "alice"

	merged := MergeConsecutiveMessages([]ChatMessage{system, user})

	require.Len(t, merged, 2)
	assert.Equal(t, map[string]any{"scope": "agent"}, merged[0].ExtraFields)
	assert.Equal(t, "alice", merged[1].Name)
	assert.JSONEq(t, mustMarshalMessage(t, system), mustMarshalMessage(t, merged[0]))
	assert.JSONEq(t, mustMarshalMessage(t, user), mustMarshalMessage(t, merged[1]))
}

func TestMergeConsecutiveMessages_MetadataIsABarrier(t *testing.T) {
	t.Parallel()

	withExtra := SystemMessage("first")
	withExtra.ExtraFields = map[string]any{"scope": "agent"}
	plain := SystemMessage("second")

	merged := MergeConsecutiveMessages([]ChatMessage{withExtra, plain})

	require.Len(t, merged, 2)
	assert.Equal(t, "first", *merged[0].Content)
	assert.Equal(t, "second", *merged[1].Content)
	assert.Equal(t, map[string]any{"scope": "agent"}, merged[0].ExtraFields)
}

func TestMergeConsecutiveMessages_PartMetadataIsABarrier(t *testing.T) {
	t.Parallel()

	part := TextContentPart("first")
	part.ExtraFields = map[string]any{"tenant": "a"}
	withMetadata := UserMultiMessage([]ContentPart{part})
	plain := UserMessage("second")

	merged := MergeConsecutiveMessages([]ChatMessage{withMetadata, plain})

	require.Len(t, merged, 2)
	require.Len(t, merged[0].ContentParts, 1)
	assert.Equal(t, map[string]any{"tenant": "a"}, merged[0].ContentParts[0].ExtraFields)
	assert.Equal(t, "second", *merged[1].Content)
}

func TestMergeConsecutiveMessages_Idempotent(t *testing.T) {
	t.Parallel()

	input := []ChatMessage{
		SystemMessage("system a"),
		SystemMessage("system b"),
		UserMessage("user a"),
		UserMessage("user b"),
	}
	once := MergeConsecutiveMessages(input)
	twice := MergeConsecutiveMessages(once)

	assert.JSONEq(t, mustMarshalMessages(t, once), mustMarshalMessages(t, twice))
}

func TestMergeConsecutiveMessages_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	input := []ChatMessage{
		SystemMessage("system a"),
		SystemMessage("system b"),
		UserMultiMessage([]ContentPart{
			TextContentPart("look"),
			ImageContentPart(ImageURLParam{URL: "https://example.com/i.png"}),
		}),
		UserMessage("thanks"),
	}
	before := mustMarshalMessages(t, input)

	_ = MergeConsecutiveMessages(input)

	assert.JSONEq(t, before, mustMarshalMessages(t, input))
}

func mustMarshalMessage(t *testing.T, msg ChatMessage) string {
	t.Helper()
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	return string(data)
}

func mustMarshalMessages(t *testing.T, messages []ChatMessage) string {
	t.Helper()
	data, err := json.Marshal(messages)
	require.NoError(t, err)
	return string(data)
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

func TestConvertToolImagesAsAttachments(t *testing.T) {
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
			if tt.want == "" {
				require.Len(t, result, 3)
			} else {
				require.Len(t, result, 4)
				parts := result[3].ContentParts
				require.Len(t, parts, 2)
				assert.Contains(t, parts[0].Text, "tool_call_id=A")
				require.NotNil(t, parts[1].ImageURL)
				assert.Equal(t, tt.want, parts[1].ImageURL.URL)
			}
			assert.Equal(t, "tool", result[1].Role)
			assert.Equal(t, "A", result[1].ToolCallID)
			parts := result[1].ContentParts
			require.Len(t, parts, 1)
			assert.Equal(t, "Read image file", parts[0].Text)
			assert.Equal(t, "tool", result[2].Role)
			assert.Equal(t, "B", result[2].ToolCallID)
		})
	}
}

func TestToolAttachmentsFollowCompleteResults(t *testing.T) {
	t.Parallel()
	const imageURL = "data:image/png;base64,AQID"
	assistant := chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "A"}, {ID: "B"}}}
	image := func(id string) chat.Message {
		return chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "read " + id},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: imageURL, Detail: chat.ImageURLDetailHigh}},
		}}
	}
	failure := chat.Message{Role: chat.MessageRoleTool, ToolCallID: "A", Content: "read failed", IsError: true}
	user := chat.Message{Role: chat.MessageRoleUser, Content: "continue"}
	document := func(mime string, data []byte, text string) chat.Message {
		return chat.Message{Role: chat.MessageRoleTool, ToolCallID: "B", MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "document"},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "attachment", MimeType: mime, Source: chat.DocumentSource{InlineData: data, InlineText: text},
			}},
		}}
	}
	for _, tc := range []struct {
		name     string
		messages []chat.Message
		roles    []string
		origins  []string
	}{
		{"reverse completion", []chat.Message{assistant, image("B"), image("A")}, []string{"assistant", "tool", "tool", "user", "user"}, []string{"B", "A"}},
		{"tool error", []chat.Message{assistant, image("B"), failure}, []string{"assistant", "tool", "tool", "user"}, []string{"B"}},
		{"empty assistant", []chat.Message{assistant, image("B"), {Role: chat.MessageRoleAssistant, Content: " \n"}, image("A")}, []string{"assistant", "tool", "tool", "user", "user"}, []string{"B", "A"}},
		{"following user", []chat.Message{assistant, image("B"), failure, user}, []string{"assistant", "tool", "tool", "user", "user"}, []string{"B"}},
		{"consecutive batches", []chat.Message{assistant, failure, image("B"), assistant, image("A"), image("B")}, []string{"assistant", "tool", "tool", "user", "assistant", "tool", "tool", "user", "user"}, []string{"B", "A", "B"}},
		{"missing result is not invented", []chat.Message{assistant, image("A")}, []string{"assistant", "tool", "user"}, []string{"A"}},
		{"PDF", []chat.Message{assistant, document("application/pdf", []byte("%PDF"), ""), failure}, []string{"assistant", "tool", "tool", "user"}, []string{"B"}},
		{"text document", []chat.Message{assistant, document("text/plain", nil, "report"), failure}, []string{"assistant", "tool", "tool", "user"}, []string{"B"}},
		{"audio", []chat.Message{assistant, document("audio/wav", []byte{1, 2, 3}, ""), failure}, []string{"assistant", "tool", "tool", "user"}, []string{"B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before, err := json.Marshal(tc.messages)
			require.NoError(t, err)
			converted := ConvertMessagesWithCaps(t.Context(), tc.messages, modelinfo.CapsWith(true, true, true, false))
			var roles, origins, calls []string
			for _, msg := range converted {
				roles = append(roles, msg.Role)
				if msg.Role == "tool" {
					calls = append(calls, msg.ToolCallID)
					for _, part := range msg.ContentParts {
						assert.NotContains(t, part.Text, "data:")
					}
				}
				if msg.Role == "user" && len(msg.ContentParts) > 0 {
					parts := msg.ContentParts
					require.Len(t, parts, 2)
					require.Less(t, len(origins), len(tc.origins))
					id := tc.origins[len(origins)]
					assert.Contains(t, parts[0].Text, "tool_call_id="+id)
					origins = append(origins, id)
					if parts[1].ImageURL != nil {
						assert.Equal(t, imageURL, parts[1].ImageURL.URL)
						assert.Equal(t, "high", parts[1].ImageURL.Detail)
					}
				}
			}
			assert.Equal(t, tc.roles, roles)
			assert.Equal(t, tc.origins, origins)
			var expectedCalls []string
			for _, msg := range tc.messages {
				if msg.Role == chat.MessageRoleTool {
					expectedCalls = append(expectedCalls, msg.ToolCallID)
				}
			}
			assert.Equal(t, expectedCalls, calls)
			after, err := json.Marshal(tc.messages)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after))
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
			assert.Equal(t, "assistant", result[0].Role)

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
