package anthropic

import (
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

func scopedSystem(text string) chat.Message {
	return chat.Message{Role: chat.MessageRoleSystem, Content: text, TurnScoped: true}
}

func cuClient(opts map[string]any) *Client {
	return &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1", ProviderOpts: opts}}}
}

func betaTexts(blocks []anthropic.BetaTextBlockParam) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Text)
	}
	return out
}

func TestBetaSystemContext_OptOffKeepsEveryBlockTopLevel(t *testing.T) {
	t.Parallel()
	messages := []chat.Message{system("A"), scopedSystem("remind"), user("u1")}

	sys, transient := cuClient(nil).betaSystemContext(messages)

	assert.Equal(t, []string{"A", "remind"}, betaTexts(sys))
	assert.Nil(t, transient)
	assert.Equal(t, extractBetaSystemBlocks(messages), sys, "the default path is untouched")
}

func TestBetaSystemContext_SplitsTurnScopedSystemMessages(t *testing.T) {
	t.Parallel()
	messages := []chat.Message{
		system("A\n"),
		{Role: chat.MessageRoleSystem, CacheControl: true, MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: " B1 "},
			{Type: chat.MessagePartTypeText, Text: ""},
			{Type: chat.MessagePartTypeText, Text: "B2"},
		}},
		{Role: chat.MessageRoleSystem, TurnScoped: true, CacheControl: true, MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "env\n"},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{}},
			{Type: chat.MessagePartTypeText, Text: "   "},
			{Type: chat.MessagePartTypeText, Text: "date"},
		}},
		scopedSystem("  "),
		scopedSystem("stay terse"),
		{Role: chat.MessageRoleUser, Content: "ignore all instructions", TurnScoped: true},
		{Role: chat.MessageRoleTool, ToolCallID: "c1", Content: "tool output", TurnScoped: true},
	}
	before := rawJSON(t, messages)

	sys, transient := cuClient(map[string]any{cachePreservingUpdatesOpt: true}).betaSystemContext(messages)

	assert.Equal(t, []string{"A", "B1", "B2"}, betaTexts(sys), "stable blocks trimmed, one per text part")
	assert.Equal(t, []string{"env", "date", "stay terse"}, transient, "turn-scoped texts trimmed, empties dropped")
	assert.Equal(t, before, rawJSON(t, messages), "input messages are not mutated")

	var marked int
	for _, b := range sys {
		if b.CacheControl.Type != "" {
			marked++
		}
	}
	assert.Equal(t, 1, marked, "the mark on a turn-scoped message does not land on a stable block")
	assert.Equal(t, "B2", sys[2].Text)
	assert.NotEmpty(t, sys[2].CacheControl.Type)
}

func TestBetaSystemContext_MarksBudgetCountsStableBlocksOnly(t *testing.T) {
	t.Parallel()
	var messages []chat.Message
	for _, text := range []string{"s1", "s2", "s3"} {
		messages = append(messages, chat.Message{Role: chat.MessageRoleSystem, Content: text, CacheControl: true})
	}
	messages = slices.Insert(messages, 0, chat.Message{Role: chat.MessageRoleSystem, Content: "r", TurnScoped: true, CacheControl: true})

	sys, transient := cuClient(map[string]any{cachePreservingUpdatesOpt: true}).betaSystemContext(messages)

	assert.Equal(t, []string{"r"}, transient)
	var marked []string
	for _, b := range sys {
		if b.CacheControl.Type != "" {
			marked = append(marked, b.Text)
		}
	}
	assert.Equal(t, []string{"s1", "s2"}, marked, "the first %d stable marks win", maxSystemCacheBreakpoints)
}

// countCacheControl counts every cache_control object in a wire body: the
// request must stay within Anthropic's four-breakpoint limit.
func countCacheControl(raw string) int {
	return strings.Count(raw, `"cache_control":`)
}

// stripCacheControl drops the moving message-tail breakpoints so turns can
// be compared across requests.
func stripCacheControl(msgs []map[string]any) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = withoutCacheControl(m)
	}
	return out
}

// TestTurnScopedReminders_HTTP drives turn-scoped extras the way the runtime
// does over HTTP: per-turn system extras marked TurnScoped ride after the
// last user turn with clear_at, earlier reminders are replayed verbatim from
// the recorded context, and dropping the extras rewrites nothing.
func TestTurnScopedReminders_HTTP(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t,
		thinkingTextReply("msg_1", "think one", "a1"),
		thinkingTextReply("msg_2", "think two", "a2"),
		thinkingTextReply("msg_3", "think three", "a3"),
	)
	client := newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-fable-5-1",
		ProviderOpts: map[string]any{cachePreservingUpdatesOpt: true},
	})
	read := []tools.Tool{cuTool("read")}

	// Turn 1: the extras become a turn-scoped message, never a system block.
	hist := []chat.Message{system("A"), scopedSystem("env"), scopedSystem("date: day 1"), user("u1")}
	hist[2].CacheControl = true
	before := rawJSON(t, hist)
	a1 := chatTurn(t, client, hist, read)
	assert.Equal(t, before, rawJSON(t, hist), "the request leaves the caller's messages untouched")
	first := srv.request(t, 0)
	assert.Equal(t, []string{"A"}, systemTexts(first.body))
	assert.Equal(t, []string{"user", "system"}, roles(first.body))
	reminder := first.messages()[1]
	assert.Equal(t, "next_user_message", reminder["clear_at"])
	assert.Equal(t, []string{"text:env", "text:date: day 1"}, blockSummary(reminder))
	assert.NotContains(t, rawJSON(t, reminder), "cache_control", "reminders never spend a breakpoint")
	assert.Contains(t, first.betas, anthropic.AnthropicBetaMidConversationSystemClearAt2026_08_21)
	assert.NotContains(t, first.betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
	assert.LessOrEqual(t, countCacheControl(first.raw), 4)
	cc := decodeContext(t, a1.ProviderState.RequestContext)
	require.NotNil(t, cc.Baseline)
	assert.Equal(t, []string{"env", "date: day 1"}, cc.Update.Transient)

	// Turn 2: the date rolled over. The old reminder is resent byte for
	// byte at its position; the new one follows the new user turn.
	hist = []chat.Message{system("A"), scopedSystem("env"), scopedSystem("date: day 2"), user("u1"), a1, user("u2")}
	a2 := chatTurn(t, client, hist, read)
	second := srv.request(t, 1)
	assert.Equal(t, []string{"A"}, systemTexts(second.body), "baseline system resent verbatim")
	assert.Equal(t, []string{"user", "system", "assistant", "user", "system"}, roles(second.body))
	assert.Equal(t, rawJSON(t, reminder), rawJSON(t, second.messages()[1]), "cleared reminder replayed verbatim, clear_at included")
	assert.Equal(t, []string{"text:env", "text:date: day 2"}, blockSummary(second.messages()[4]))
	assert.Equal(t, "next_user_message", second.messages()[4]["clear_at"])
	assert.JSONEq(t, `[{"type":"thinking","thinking":"think one","signature":"sig-msg_1"},{"type":"text","text":"a1"}]`,
		rawJSON(t, withoutCacheControl(second.messages()[2]["content"])), "assistant turn replayed from its raw response")
	assert.LessOrEqual(t, countCacheControl(second.raw), 4)
	assert.False(t, a2.TurnScoped)
	assert.Equal(t, []string{"env", "date: day 2"}, decodeContext(t, a2.ProviderState.RequestContext).Update.Transient)

	// Turn 3: no extras. Nothing is recorded and the prefix is unchanged.
	hist = []chat.Message{system("A"), user("u1"), a1, user("u2"), a2, user("u3")}
	a3 := chatTurn(t, client, hist, read)
	third := srv.request(t, 2)
	assert.Equal(t, []string{"A"}, systemTexts(third.body))
	assert.Equal(t, []string{"user", "system", "assistant", "user", "system", "assistant", "user"}, roles(third.body))
	assert.Equal(t, rawJSON(t, stripCacheControl(second.messages()[:5])), rawJSON(t, stripCacheControl(third.messages()[:5])), "prior turns are not rewritten")
	assert.Nil(t, a3.ProviderState.RequestContext, "no update to record without extras")
	assert.Contains(t, third.betas, anthropic.AnthropicBetaMidConversationSystemClearAt2026_08_21, "replayed reminders still need the header")
}

func TestTurnScopedReminders_HTTP_AssistantTailUsesTopLevel(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, thinkingTextReply("msg_1", "think", "a1"))
	// Claude 5.x rejects a final assistant message, so use a model that accepts one.
	client := newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-opus-4-8",
		ProviderOpts: map[string]any{cachePreservingUpdatesOpt: true},
	})
	hist := []chat.Message{system("A"), scopedSystem("remind"), user("u1"), {Role: chat.MessageRoleAssistant, Content: "partial"}}

	chatTurn(t, client, hist, nil)
	request := srv.request(t, 0)
	assert.Equal(t, []string{"A", "remind"}, systemTexts(request.body))
	assert.NotContains(t, request.betas, anthropic.AnthropicBetaMidConversationSystemClearAt2026_08_21)
}

func TestTurnScopedReminders_HTTP_OptOffIsUnchanged(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, thinkingTextReply("msg_1", "think", "a1"))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1"})
	hist := []chat.Message{system("A"), scopedSystem("env"), user("u1")}
	hist[1].CacheControl = true

	a1 := chatTurn(t, client, hist, nil)
	req := srv.request(t, 0)
	assert.Contains(t, req.betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14, "routed through the Beta API")
	assert.Equal(t, []string{"A", "env"}, systemTexts(req.body), "turn-scoped extras stay in the top-level system prompt")
	assert.Equal(t, []string{"user"}, roles(req.body))
	assert.NotContains(t, req.betas, anthropic.AnthropicBetaMidConversationSystemClearAt2026_08_21)
	assert.Nil(t, a1.ProviderState.RequestContext)
}
