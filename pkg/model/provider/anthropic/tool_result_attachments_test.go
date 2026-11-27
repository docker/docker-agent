package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestParallelToolAttachmentsStayInNativeResults(t *testing.T) {
	t.Parallel()
	for _, beta := range []bool{false, true} {
		name := "standard"
		if beta {
			name = "beta"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			messages := []chat.Message{
				{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{
					{ID: "A", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
					{ID: "B", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
				}},
				{Role: chat.MessageRoleTool, ToolCallID: "B", MultiContent: chat.BuildToolResultMultiContent("image B", []tools.MediaContent{{MimeType: "image/png", Data: "AQID"}}, nil)},
				{Role: chat.MessageRoleTool, ToolCallID: "A", IsError: true, MultiContent: chat.BuildToolResultMultiContent("error A", nil, []tools.DocumentContent{{Name: "report.pdf", MimeType: "application/pdf", Data: "JVBERg=="}})},
				{Role: chat.MessageRoleUser, Content: "continue"},
			}
			client := testAttachmentClient()
			var converted any
			var err error
			if beta {
				converted, err = client.convertBetaMessages(t.Context(), messages)
			} else {
				converted, err = client.convertMessages(t.Context(), messages)
			}
			require.NoError(t, err)
			raw, err := json.Marshal(converted)
			require.NoError(t, err)
			var wire []struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string           `json:"type"`
					ToolUseID string           `json:"tool_use_id"`
					IsError   bool             `json:"is_error"`
					Content   []map[string]any `json:"content"`
				} `json:"content"`
			}
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.Len(t, wire, 3)
			assert.Equal(t, "user", wire[1].Role)
			require.Len(t, wire[1].Content, 2)
			for i, id := range []string{"B", "A"} {
				result := wire[1].Content[i]
				assert.Equal(t, "tool_result", result.Type)
				assert.Equal(t, id, result.ToolUseID)
				assert.Equal(t, id == "A", result.IsError)
				require.Len(t, result.Content, 2)
				source := result.Content[1]["source"].(map[string]any)
				if id == "B" {
					assert.Equal(t, "image", result.Content[1]["type"])
					assert.Equal(t, "AQID", source["data"])
				} else {
					assert.Equal(t, "document", result.Content[1]["type"])
					assert.Equal(t, "JVBERg==", source["data"])
				}
			}
		})
	}
}

func TestBetaDocumentPartsInMessagesAndToolResults(t *testing.T) {
	t.Parallel()
	client := testAttachmentClient()
	parts := []chat.MessagePart{
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "note", MimeType: "text/plain", Source: chat.DocumentSource{InlineText: "hi"}}},
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1}}}},
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: []byte{2}}}},
	}
	userBlocks, err := client.convertUserMultiContent(t.Context(), parts)
	require.NoError(t, err)
	result, err := client.convertToolResultBlock(t.Context(), &chat.Message{MultiContent: parts})
	require.NoError(t, err)
	assert.Equal(t, "tool_result", result.Type)
	const want = `[
		{"type":"text","text":"<document-note-text-plain>\nhi\n</document-note-text-plain>"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQ=="}},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"Ag=="}}
	]`
	assert.JSONEq(t, want, rawJSON(t, userBlocks))
	assert.JSONEq(t, want, rawJSON(t, result.Content))
}

func TestConvertToolResultBlockUsesClaudeCapabilityFallback(t *testing.T) {
	t.Parallel()
	client := testAttachmentClientWithStore(nil)
	pdf := []byte("%PDF")
	msg := &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "toolu_1",
		Content:    "created report",
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "created report"},
			{
				Type: chat.MessagePartTypeDocument,
				Document: &chat.Document{
					Name:     "report.pdf",
					MimeType: "application/pdf",
					Source:   chat.DocumentSource{InlineData: pdf},
				},
			},
		},
	}

	block, err := client.convertToolResultBlock(t.Context(), msg)
	require.NoError(t, err)
	assert.Equal(t, "tool_result", block.Type)
	content, ok := block.Content.([]ContentBlockParam)
	require.True(t, ok)
	require.Len(t, content, 2)
	assert.Equal(t, "document", content[1].Type)
}

func TestConvertToolResultBlockIncludesDocumentAttachments(t *testing.T) {
	t.Parallel()
	client := testAttachmentClient()
	pdf := []byte("%PDF")
	msg := &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "toolu_1",
		Content:    "created report",
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "created report"},
			{
				Type: chat.MessagePartTypeDocument,
				Document: &chat.Document{
					Name:     "report.pdf",
					MimeType: "application/pdf",
					Source:   chat.DocumentSource{InlineData: pdf},
				},
			},
		},
	}

	block, err := client.convertToolResultBlock(t.Context(), msg)
	require.NoError(t, err)
	assert.Equal(t, "tool_result", block.Type)
	content, ok := block.Content.([]ContentBlockParam)
	require.True(t, ok)
	require.Len(t, content, 2)
	assert.Equal(t, "text", content[0].Type)
	assert.Equal(t, "document", content[1].Type)
	require.NotNil(t, content[1].Source)
	assert.Equal(t, base64.StdEncoding.EncodeToString(pdf), content[1].Source.Data)
}

func TestConvertToolResultBlockNeverSendsEmptyContent(t *testing.T) {
	t.Parallel()
	client := testAttachmentClient()
	msg := &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "toolu_1",
		Content:    "   ",
	}

	block, err := client.convertToolResultBlock(t.Context(), msg)
	require.NoError(t, err)
	assert.Equal(t, "tool_result", block.Type)
	content, ok := block.Content.([]ContentBlockParam)
	require.True(t, ok)
	require.Len(t, content, 1)
	assert.Equal(t, "(no output)", content[0].Text)
}

func TestConvertToolResultBlockIncludesImageDocumentAttachments(t *testing.T) {
	t.Parallel()
	client := testAttachmentClient()
	image := []byte("PNG")
	msg := &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "toolu_1",
		Content:    "screenshot",
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "screenshot"},
			{
				Type: chat.MessagePartTypeDocument,
				Document: &chat.Document{
					Name:     "screenshot.png",
					MimeType: "image/png",
					Source:   chat.DocumentSource{InlineData: image},
				},
			},
		},
	}

	block, err := client.convertToolResultBlock(t.Context(), msg)
	require.NoError(t, err)
	assert.Equal(t, "tool_result", block.Type)
	content, ok := block.Content.([]ContentBlockParam)
	require.True(t, ok)
	require.Len(t, content, 2)
	assert.Equal(t, "image", content[1].Type)
	require.NotNil(t, content[1].Source)
	assert.Equal(t, base64.StdEncoding.EncodeToString(image), content[1].Source.Data)
	assert.Equal(t, "image/png", content[1].Source.MediaType)
}

func testAttachmentClient() *Client {
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-test": {
						Modalities: modelsdev.Modalities{Input: []string{"text", "image", "pdf"}},
					},
				},
			},
		},
	})

	return testAttachmentClientWithStore(store)
}

func testAttachmentClientWithStore(store *modelsdev.Store) *Client {
	var modelOptions options.ModelOptions
	if store != nil {
		options.WithModelsDevStore(store)(&modelOptions)
	}

	return &Client{Config: base.Config{
		ModelConfig: latest.ModelConfig{
			Provider: "anthropic",
			Model:    "claude-test",
		},
		ModelOptions: modelOptions,
	}}
}
