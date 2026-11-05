package providers_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/tools"
)

// reloadedClaudeAPIHistory is a conversation whose tool turn was recorded by
// Sonnet 5.5 on the Claude API, then saved and reloaded, and that continues
// with an attachment. The redacted_thinking block exists only in the raw
// state, so it reaches the wire only through raw replay.
func reloadedClaudeAPIHistory(t *testing.T) []chat.Message {
	t.Helper()
	turn := chat.Message{
		Role: chat.MessageRoleAssistant, ReasoningContent: "plan", ThinkingSignature: "sig-1",
		ToolCalls: []tools.ToolCall{{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{}`}}},
	}
	turn.AttachProviderState(&chat.ProviderState{
		Provider: "anthropic", Version: chat.ProviderStateVersion, Model: "claude-sonnet-5-5", MessageID: "msg_1",
		Content: json.RawMessage(`[{"type":"redacted_thinking","data":"r1"},{"type":"thinking","thinking":"plan","signature":"sig-1"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`),
	})
	history := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "u1"},
		turn,
		{Role: chat.MessageRoleTool, ToolCallID: "t1", Content: "ok"},
		{Role: chat.MessageRoleAssistant, Content: "a1"},
		{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "u2"},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "spec.pdf", MimeType: "application/pdf",
				Source: chat.DocumentSource{InlineData: []byte("%PDF-1.4\n%%EOF\n")},
			}},
		}},
	}

	raw, err := json.Marshal(history)
	require.NoError(t, err)
	var reloaded []chat.Message
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	return reloaded
}

// embeddedCatalog serves the embedded models.dev snapshot without network, as
// the store production passes to every client: Bedrock reads attachment
// support from it.
func embeddedCatalog() options.Opt {
	return options.WithModelsDevStore(modelsdev.NewDatabaseStore(embedded.Snapshot()))
}

// wireBlocks returns the content blocks of the i-th message on the wire: 1 is
// the recorded tool turn, 4 the new user turn.
func wireBlocks(t *testing.T, req recorded, i int) []map[string]any {
	t.Helper()
	messages, _ := req.body["messages"].([]any)
	require.Len(t, messages, 5)
	msg, ok := messages[i].(map[string]any)
	require.True(t, ok)
	content, _ := msg["content"].([]any)
	blocks := make([]map[string]any, 0, len(content))
	for _, c := range content {
		block, ok := c.(map[string]any)
		require.True(t, ok)
		blocks = append(blocks, block)
	}
	require.NotEmpty(t, blocks)
	return blocks
}

// After a reload, a model or transport switch keeps sending the recorded
// thinking: the API drops what the new model or account cannot read, and
// stripping it client-side would lose it for good.
func TestClaude5SwitchAfterReload(t *testing.T) {
	t.Parallel()

	anthropicEnv := map[string]string{"ANTHROPIC_API_KEY": "test-key"}
	bedrockEnv := map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "test-token", "AWS_REGION": "us-east-1"}
	vertexToken := options.WithTokenSource(func(context.Context) (string, error) { return "vertex-token", nil })

	anthropicCases := map[string]struct {
		cfg  latest.ModelConfig
		env  map[string]string
		opts []options.Opt
	}{
		"same model": {
			cfg: latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5-5"},
			env: anthropicEnv,
		},
		"model switch to haiku 5.5": {
			cfg: latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5"},
			env: anthropicEnv,
		},
		"transport switch to vertex": {
			cfg: latest.ModelConfig{
				Provider: "google", Model: "claude-sonnet-5-5",
				ProviderOpts: map[string]any{"publisher": "anthropic", "project": "test-project", "location": "us-east5"},
			},
			opts: []options.Opt{vertexToken},
		},
	}
	for name, tc := range anthropicCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := captureHistoryRequest(t, tc.cfg, tc.env, reloadedClaudeAPIHistory(t), tc.opts...)

			turn := wireBlocks(t, req, 1)
			require.Len(t, turn, 3)
			assert.Equal(t, map[string]any{"type": "redacted_thinking", "data": "r1"}, turn[0], "the raw blocks are replayed, not rebuilt")
			assert.Equal(t, "thinking", turn[1]["type"])
			assert.Equal(t, "plan", turn[1]["thinking"])
			assert.Equal(t, "sig-1", turn[1]["signature"])
			assert.Equal(t, "tool_use", turn[2]["type"])

			assert.Contains(t, strings.Join(req.header.Values("anthropic-beta"), ","), "thinking-binding-controls-2026-08-01")
			assert.Equal(t, "drop_block", field(req.body, "thinking", "block_binding", "prefix_mismatch_behavior"))
			assert.True(t, slices.ContainsFunc(wireBlocks(t, req, 4), func(b map[string]any) bool { return b["type"] == "document" }), "the new turn carries the attachment")
		})
	}

	t.Run("transport switch to bedrock", func(t *testing.T) {
		t.Parallel()
		req := captureHistoryRequest(t, latest.ModelConfig{Provider: "amazon-bedrock", Model: "anthropic.claude-sonnet-5-5"}, bedrockEnv, reloadedClaudeAPIHistory(t), embeddedCatalog())

		turn := wireBlocks(t, req, 1)
		require.Len(t, turn, 2, "the Claude API's redacted block has no Converse form")
		assert.Equal(t, "plan", field(turn[0], "reasoningContent", "reasoningText", "text"))
		assert.Equal(t, "sig-1", field(turn[0], "reasoningContent", "reasoningText", "signature"), "recorded thinking is replayed as signed reasoning")
		assert.Contains(t, turn[1], "toolUse")
		assert.True(t, slices.ContainsFunc(wireBlocks(t, req, 4), func(b map[string]any) bool { return b["document"] != nil }), "the new turn carries the attachment")
	})
}
