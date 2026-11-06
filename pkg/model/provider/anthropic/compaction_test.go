package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func compactionClient(srv *scriptedServer, opts map[string]any) *Client {
	return newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1", ProviderOpts: opts})
}

// compactableHistory is a system prompt plus a tool-using exchange whose
// assistant turn replays from its raw response.
func compactableHistory() []chat.Message {
	call := toolCall("c1", "read")
	asst := chat.Message{Role: chat.MessageRoleAssistant, Content: "reading", ReasoningContent: "look", ToolCalls: []tools.ToolCall{call}}
	asst.AttachProviderState(&chat.ProviderState{Provider: providerStateName, MessageID: "msg_0", Content: json.RawMessage(
		`[{"type":"thinking","thinking":"look","signature":"sig0"},{"type":"text","text":"reading"},{"type":"tool_use","id":"c1","name":"read","input":{"path":"a"}}]`)})
	return []chat.Message{
		system("You are terse."), user("u1"), asst,
		{Role: chat.MessageRoleTool, ToolCallID: "c1", Content: "file body"},
		{Role: chat.MessageRoleAssistant, Content: "done"},
		user("u2"),
	}
}

func TestCompactConversation_EmitsSignedBlock(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, compactionReply("msg_c", "  Summary of the work.  ", "sig-c"))
	client := compactionClient(srv, nil)
	hist := compactableHistory()
	before := rawJSON(t, hist)

	result, err := client.CompactConversation(t.Context(), hist, cuTools("read"), "Keep file paths.")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, before, rawJSON(t, hist), "the history is not rewritten")

	req := srv.request(t, 0)
	assert.Equal(t, "/v1/messages", req.path)
	assert.Contains(t, req.betas, nativeCompactionBeta)
	assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
	assert.Equal(t, map[string]any{"type": "summarize", "instructions": "Keep file paths."}, req.body["compaction"])
	assert.InDelta(t, 16000, req.body["max_tokens"], 0)
	assert.Equal(t, "claude-fable-5-1", req.body["model"])
	assert.Equal(t, []string{"You are terse."}, systemTexts(req.body))
	assert.Equal(t, []string{"read"}, toolNames(req.body))
	assert.Equal(t, []string{"user", "assistant", "user", "assistant", "user"}, roles(req.body))
	assert.JSONEq(t, `[{"type":"thinking","thinking":"look","signature":"sig0"},{"type":"text","text":"reading"},{"type":"tool_use","id":"c1","name":"read","input":{"path":"a"}}]`,
		rawJSON(t, withoutCacheControl(req.messages()[1]["content"])), "the compacted prompt is the conversation prompt")
	assert.Equal(t, "adaptive", req.thinking()["type"])
	assert.NotContains(t, req.body, "stream_options")

	assert.Equal(t, "  Summary of the work.  ", result.Summary, "trimming is the runtime's job")
	assert.Equal(t, providerStateName, result.Provider)
	assert.Equal(t, "claude-fable-5-1-20260901", result.Model)
	assert.Nil(t, result.RequestContext, "no cache-preserving baseline without the option")
	assert.Equal(t, chat.Usage{InputTokens: 5000, OutputTokens: 300, CacheWriteTokens: 100, CachedInputTokens: 4000}, result.Usage,
		"billed from usage.iterations, not the zero top-level totals")

	var block map[string]any
	require.NoError(t, json.Unmarshal(result.Block, &block))
	assert.Equal(t, "compaction", block["type"])
	assert.Equal(t, "  Summary of the work.  ", block["content"])
	assert.Equal(t, "sig-c", block["signature"])
	assert.Equal(t, "enc-msg_c", block["encrypted_content"])
}

func TestCompactConversation_UsageAccounting(t *testing.T) {
	t.Parallel()
	twoIterations := sseBody(
		messageStart("msg_c", nil),
		blockStart(0, map[string]any{"type": "compaction", "content": "S", "encrypted_content": "e", "signature": "s"}),
		blockStop(0),
		messageDelta("compaction", map[string]any{"output_tokens": 0, "iterations": []any{
			map[string]any{"type": "message", "model": "claude-fable-5-1", "input_tokens": 100, "output_tokens": 10, "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2},
			map[string]any{"type": "compaction", "input_tokens": 1000, "output_tokens": 200, "cache_creation_input_tokens": 3, "cache_read_input_tokens": 4},
		}}),
		messageStop,
	)

	t.Run("iterations are summed", func(t *testing.T) {
		t.Parallel()
		result, err := compactionClient(newScriptedServer(t, twoIterations), nil).CompactConversation(t.Context(), []chat.Message{user("u")}, nil, "")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, chat.Usage{InputTokens: 1100, OutputTokens: 210, CacheWriteTokens: 4, CachedInputTokens: 6}, result.Usage)
		assert.Equal(t, []chat.Usage{
			{InputTokens: 100, OutputTokens: 10, CacheWriteTokens: 1, CachedInputTokens: 2},
			{InputTokens: 1000, OutputTokens: 200, CacheWriteTokens: 3, CachedInputTokens: 4},
		}, result.Iterations, "each iteration is kept so it can be priced on its own")
	})

	t.Run("track_usage false reports nothing", func(t *testing.T) {
		t.Parallel()
		off := false
		client := newScriptedClient(newScriptedServer(t, twoIterations), latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1", TrackUsage: &off})
		result, err := client.CompactConversation(t.Context(), []chat.Message{user("u")}, nil, "")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, chat.Usage{}, result.Usage)
	})
}

func TestCompactConversation_Safeguards(t *testing.T) {
	t.Parallel()
	compactionBlock := func(content, signature any) ssestream.Event {
		return blockStart(0, map[string]any{"type": "compaction", "content": content, "encrypted_content": "e", "signature": signature})
	}
	for name, tc := range map[string]struct {
		reply   string
		wantErr string
		wantNil bool
	}{
		"stream ends before message_stop": {
			reply:   sseBody(messageStart("m", nil), compactionBlock("S", "s"), blockStop(0), messageDelta("compaction", nil)),
			wantErr: "incomplete compaction response",
		},
		"model answered instead of compacting": {
			reply:   textReply("m", "I cannot compact this."),
			wantErr: `unexpected compaction stop reason "end_turn"`,
		},
		"extra content block": {
			reply: sseBody(messageStart("m", nil), compactionBlock("S", "s"), blockStop(0),
				blockStart(1, map[string]any{"type": "text", "text": "and"}), blockStop(1), messageDelta("compaction", nil), messageStop),
			wantErr: "expected one compaction block",
		},
		"null content means compaction failed": {
			reply:   sseBody(messageStart("m", nil), compactionBlock(nil, nil), blockStop(0), messageDelta("compaction", nil), messageStop),
			wantNil: true,
		},
		"blank content": {
			reply:   sseBody(messageStart("m", nil), compactionBlock("  \n", "s"), blockStop(0), messageDelta("compaction", nil), messageStop),
			wantNil: true,
		},
		"unsigned block": {
			reply:   sseBody(messageStart("m", nil), compactionBlock("S", nil), blockStop(0), messageDelta("compaction", nil), messageStop),
			wantErr: "unsigned compaction block",
		},
		"refusal": {
			reply:   sseBody(messageStart("m", nil), messageDelta("refusal", nil), messageStop),
			wantErr: `unexpected compaction stop reason "refusal"`,
		},
		"empty signature": {
			reply:   sseBody(messageStart("m", nil), compactionBlock("S", ""), blockStop(0), messageDelta("compaction", nil), messageStop),
			wantErr: "unsigned compaction block",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := compactionClient(newScriptedServer(t, tc.reply), nil)
			result, err := client.CompactConversation(t.Context(), compactableHistory(), cuTools("read"), "")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, result)
				return
			}
			require.NoError(t, err)
			assert.Nil(t, result)
		})
	}

	t.Run("http error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"compaction not available"}}`))
		}))
		t.Cleanup(srv.Close)
		client := compactionClient(&scriptedServer{Server: srv}, nil)
		result, err := client.CompactConversation(t.Context(), compactableHistory(), nil, "")
		require.ErrorContains(t, err, "compaction not available")
		assert.Nil(t, result)
	})

	t.Run("only the Claude API", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, compactionReply("m", "S", "s"))
		client := newScriptedClient(srv, latest.ModelConfig{Provider: "bedrock", Model: "us.anthropic.claude-fable-5-1"})
		_, err := client.CompactConversation(t.Context(), compactableHistory(), nil, "")
		require.ErrorContains(t, err, "requires the Claude API")
		assert.Equal(t, 0, srv.count())
	})

	t.Run("nothing to compact", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, compactionReply("m", "S", "s"))
		result, err := compactionClient(srv, nil).CompactConversation(t.Context(), []chat.Message{system("only a prompt")}, nil, "")
		require.NoError(t, err)
		assert.Nil(t, result)
		assert.Equal(t, 0, srv.count())
	})

	t.Run("strict named tool is validated before the request", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, compactionReply("m", "S", "s"))
		client := compactionClient(srv, map[string]any{"strict_tools": []any{"read"}})
		_, err := client.CompactConversation(t.Context(), compactableHistory(), cuTools("read"), "")
		require.ErrorContains(t, err, `strict_tools: tool "read"`)
		assert.Equal(t, 0, srv.count())
	})
}

// summaryMessage is the synthetic prompt message the runtime builds from a
// compaction result.
func summaryMessage(result *chat.CompactionResult) chat.Message {
	msg := user(chat.SummaryMessageContent(result.Summary))
	msg.Compaction = result
	return msg
}

func TestCompaction_NextRequestReplaysBlock(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, compactionReply("msg_c", "Summary.", "sig-c"), textReply("msg_1", "continuing"))
	client := compactionClient(srv, map[string]any{"native_compaction": true})
	result, err := client.CompactConversation(t.Context(), compactableHistory(), cuTools("read"), "")
	require.NoError(t, err)

	hist := []chat.Message{system("You are terse."), summaryMessage(result), user("u3")}
	before := rawJSON(t, hist)
	reply := chatTurn(t, client, hist, cuTools("read"))
	assert.Equal(t, "continuing", reply.Content)
	assert.Equal(t, before, rawJSON(t, hist))

	req := srv.request(t, 1)
	assert.Contains(t, req.betas, nativeCompactionBeta)
	assert.Equal(t, []string{"You are terse."}, systemTexts(req.body))
	assert.Equal(t, []string{"user", "user"}, roles(req.body))
	content := req.messages()[0]["content"].([]any)
	require.Len(t, content, 1)
	assert.JSONEq(t, string(result.Block), rawJSON(t, content[0]), "the signed block goes back verbatim")
	assert.NotContains(t, req.raw, "Summary.\"}", "the readable summary is not sent alongside the block")
}

func TestCompaction_StandardPathWithoutBlock(t *testing.T) {
	t.Parallel()
	for name, compaction := range map[string]*chat.CompactionResult{
		"foreign provider": {Provider: "openai", Summary: "Summary.", Block: json.RawMessage(`{"type":"compaction"}`)},
		"summary only":     {Provider: providerStateName, Summary: "Summary."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"native_compaction": true}})
			chatTurn(t, client, []chat.Message{summaryMessage(compaction), user("u3")}, nil)

			req := srv.request(t, 0)
			assert.NotContains(t, req.betas, nativeCompactionBeta)
			assert.Equal(t, []string{"user", "user"}, roles(req.body))
			assert.Contains(t, req.raw, `"Session Summary: Summary."`, "the readable summary is sent as text")
		})
	}
}

func TestCompaction_EditedSummaryIsNotReplayed(t *testing.T) {
	t.Parallel()
	signed := &chat.CompactionResult{Provider: providerStateName, Summary: "S", Block: json.RawMessage(`{"type":"compaction","content":"S","encrypted_content":"e","signature":"s"}`)}
	srv := newScriptedServer(t, textReply("msg_1", "ok"))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"native_compaction": true}})
	redacted := summaryMessage(signed)
	redacted.Content = chat.SummaryMessageContent("[REDACTED]")
	chatTurn(t, client, []chat.Message{redacted, user("u3")}, nil)

	req := srv.request(t, 0)
	assert.NotContains(t, req.betas, nativeCompactionBeta)
	assert.Equal(t, []string{"user", "user"}, roles(req.body))
	assert.Contains(t, req.raw, `"Session Summary: [REDACTED]"`)
	assert.NotContains(t, req.raw, `"encrypted_content"`, "a rewritten summary must not resurrect the signed block")
}

func TestCompaction_RejectsMalformedHistory(t *testing.T) {
	t.Parallel()
	signed := &chat.CompactionResult{Provider: providerStateName, Summary: "S", Block: json.RawMessage(`{"type":"compaction","content":"S","encrypted_content":"e","signature":"s"}`)}
	for name, tc := range map[string]struct {
		hist    []chat.Message
		wantErr string
	}{
		"block after conversation": {
			hist:    []chat.Message{user("u1"), {Role: chat.MessageRoleAssistant, Content: "a1"}, summaryMessage(signed), user("u2")},
			wantErr: "signed compaction must precede conversation messages",
		},
		"invalid block json": {
			hist:    []chat.Message{summaryMessage(&chat.CompactionResult{Provider: providerStateName, Summary: "S", Block: json.RawMessage(`{"type":`)}), user("u2")},
			wantErr: "invalid compaction block",
		},
		"unsigned block": {
			hist:    []chat.Message{summaryMessage(&chat.CompactionResult{Provider: providerStateName, Summary: "S", Block: json.RawMessage(`{"type":"compaction","content":"S"}`)}), user("u2")},
			wantErr: "requires content and signature",
		},
		"wrong block type": {
			hist:    []chat.Message{summaryMessage(&chat.CompactionResult{Provider: providerStateName, Summary: "S", Block: json.RawMessage(`{"type":"text","text":"S","signature":"s"}`)}), user("u2")},
			wantErr: "requires content and signature",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			_, err := compactionClient(srv, map[string]any{"native_compaction": true}).CreateChatCompletionStream(t.Context(), tc.hist, nil)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, 0, srv.count())
		})
	}
}

func TestCompaction_OnlyBlockCanResume(t *testing.T) {
	t.Parallel()
	signed := &chat.CompactionResult{Provider: providerStateName, Summary: "S", Block: json.RawMessage(`{"type":"compaction","content":"S","signature":"s"}`)}
	srv := newScriptedServer(t, textReply("msg_1", "continued"))
	client := compactionClient(srv, map[string]any{cachePreservingUpdatesOpt: true, "native_compaction": true})
	chatTurn(t, client, []chat.Message{summaryMessage(signed)}, nil)
	request := srv.request(t, 0)
	assert.Equal(t, []string{"user"}, roles(request.body))
	assert.Contains(t, request.betas, nativeCompactionBeta)
}

// directCompaction is a signed block recorded by a Claude API client.
func directCompaction() *chat.CompactionResult {
	return &chat.CompactionResult{
		Provider: providerStateName, Summary: "Summary.",
		Block: json.RawMessage(`{"type":"compaction","content":"Summary.","signature":"sig-c"}`),
	}
}

// Only a client configured for native compaction replays the block; others,
// like Vertex, which cannot enable it, send the summary text.
func TestCompaction_NotReplayedWithoutNativeCompaction(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, textReply("msg_1", "continuing"))
	client := compactionClient(srv, nil)
	chatTurn(t, client, []chat.Message{system("You are terse."), summaryMessage(directCompaction()), user("u3")}, nil)

	req := srv.request(t, 0)
	assert.NotContains(t, req.betas, nativeCompactionBeta)
	assert.NotContains(t, req.raw, `"type":"compaction"`)
	assert.Contains(t, req.raw, "Summary.")
}
