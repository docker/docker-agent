package anthropic

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// cuHarness drives applyConversationUpdates the way createBetaStream would:
// every turn converts the chat history, builds the top-level system, tools
// and effort, applies the helper and persists the returned context on the
// assistant reply.
type cuHarness struct {
	t     *testing.T
	model string
	hist  []chat.Message
}

type cuTurn struct {
	params anthropic.BetaMessageNewParams
	result *conversationUpdateResult
	body   map[string]any
}

func newCUHarness(t *testing.T, model string) *cuHarness {
	t.Helper()
	return &cuHarness{t: t, model: model}
}

func cuTool(name string) tools.Tool {
	return tools.Tool{
		Name:        name,
		Description: "Tool " + name,
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}
}

func cuTools(names ...string) []tools.Tool {
	out := make([]tools.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, cuTool(n))
	}
	return out
}

func (h *cuHarness) buildParams(system []string, requestTools []tools.Tool, effort string) anthropic.BetaMessageNewParams {
	h.t.Helper()
	converted, err := testClient().convertBetaMessages(h.t.Context(), h.hist)
	require.NoError(h.t, err)
	betaTools, err := convertBetaTools(requestTools)
	require.NoError(h.t, err)
	var sys []anthropic.BetaTextBlockParam
	for i, text := range system {
		block := anthropic.BetaTextBlockParam{Text: text}
		if i == len(system)-1 {
			block.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
		}
		sys = append(sys, block)
	}
	params := anthropic.BetaMessageNewParams{
		Model:     h.model,
		MaxTokens: 1024,
		System:    sys,
		Messages:  converted,
		Tools:     betaTools,
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaInterleavedThinking2025_05_14},
	}
	params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(effort)
	return params
}

func (h *cuHarness) apply(params anthropic.BetaMessageNewParams, transient ...string) (cuTurn, error) {
	h.t.Helper()
	res, err := applyConversationUpdates(h.t.Context(), h.model, &params, h.hist, transient)
	if err != nil {
		return cuTurn{}, err
	}
	return cuTurn{params: params, result: res, body: wire(h.t, params)}, nil
}

func (h *cuHarness) turn(system []string, requestTools []tools.Tool, effort string, transient ...string) cuTurn {
	h.t.Helper()
	turn, err := h.apply(h.buildParams(system, requestTools, effort), transient...)
	require.NoError(h.t, err)
	return turn
}

func (h *cuHarness) user(text string) {
	h.hist = append(h.hist, chat.Message{Role: chat.MessageRoleUser, Content: text})
}

// reply persists the assistant response of turn, carrying the returned
// request context like the runtime would. The raw response (a signed
// thinking block, the text, one tool_use per call) is recorded so the
// converter replays it rather than the flattened fields.
func (h *cuHarness) reply(turn cuTurn, text string, calls ...tools.ToolCall) {
	msg := chat.Message{Role: chat.MessageRoleAssistant, Content: text, ReasoningContent: "thinking about " + text, ToolCalls: calls}
	blocks := []string{fmt.Sprintf(`{"type":"thinking","thinking":%q,"signature":"sig"}`, msg.ReasoningContent)}
	if text != "" {
		blocks = append(blocks, fmt.Sprintf(`{"type":"text","text":%q}`, text))
	}
	for _, call := range calls {
		blocks = append(blocks, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, call.ID, call.Function.Name, call.Function.Arguments))
	}
	state := &chat.ProviderState{Provider: providerStateName, MessageID: "msg", Content: json.RawMessage("[" + strings.Join(blocks, ",") + "]")}
	msg.AttachProviderState(withRequestContext(state, turn.result.RequestContext))
	h.hist = append(h.hist, msg)
}

func (h *cuHarness) toolResult(callID, text string) {
	h.hist = append(h.hist, chat.Message{Role: chat.MessageRoleTool, ToolCallID: callID, Content: text})
}

func toolCall(id, name string) tools.ToolCall {
	return tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: name, Arguments: `{"path":"a"}`}}
}

func wire(t *testing.T, params anthropic.BetaMessageNewParams) map[string]any {
	t.Helper()
	data, err := params.MarshalJSON()
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(data, &body))
	return body
}

func wireBytes(t *testing.T, params anthropic.BetaMessageNewParams) string {
	t.Helper()
	data, err := params.MarshalJSON()
	require.NoError(t, err)
	return string(data)
}

func systemTexts(body map[string]any) []string {
	blocks, _ := body["system"].([]any)
	var out []string
	for _, b := range blocks {
		out = append(out, b.(map[string]any)["text"].(string))
	}
	return out
}

func toolNames(body map[string]any) []string {
	list, _ := body["tools"].([]any)
	var out []string
	for _, tl := range list {
		out = append(out, tl.(map[string]any)["name"].(string))
	}
	return out
}

func wireMessages(body map[string]any) []map[string]any {
	list, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		out = append(out, m.(map[string]any))
	}
	return out
}

func systemMessages(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range wireMessages(body) {
		if m["role"] == "system" {
			out = append(out, m)
		}
	}
	return out
}

func roles(body map[string]any) []string {
	var out []string
	for _, m := range wireMessages(body) {
		out = append(out, m["role"].(string))
	}
	return out
}

// blockSummary renders a system message's content as "text:...",
// "add:name", "remove:name" for compact assertions.
func blockSummary(msg map[string]any) []string {
	blocks, _ := msg["content"].([]any)
	out := []string{}
	for _, b := range blocks {
		block := b.(map[string]any)
		switch block["type"] {
		case "text":
			out = append(out, "text:"+block["text"].(string))
		case "tool_addition":
			out = append(out, "add:"+block["tool"].(map[string]any)["name"].(string))
		case "tool_removal":
			out = append(out, "remove:"+block["tool"].(map[string]any)["name"].(string))
		}
	}
	return out
}

func decodeContext(t *testing.T, raw json.RawMessage) conversationContext {
	t.Helper()
	var cc conversationContext
	require.NoError(t, json.Unmarshal(raw, &cc))
	return cc
}

func TestConversationUpdates_FirstTurnKeepsBaseline(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("hi")

	turn := h.turn([]string{"A", "B"}, cuTools("read", "write"), "high")

	assert.False(t, turn.result.Reset)
	assert.Equal(t, []string{"A", "B"}, systemTexts(turn.body))
	assert.Equal(t, []string{"read", "write"}, toolNames(turn.body))
	assert.Equal(t, "high", turn.body["output_config"].(map[string]any)["effort"])
	assert.Equal(t, []string{"user"}, roles(turn.body))
	assert.Equal(t, []anthropic.AnthropicBeta{anthropic.AnthropicBetaInterleavedThinking2025_05_14}, turn.params.Betas, "no conditional beta without updates")

	cc := decodeContext(t, turn.result.RequestContext)
	require.NotNil(t, cc.Baseline)
	assert.Len(t, cc.Baseline.System, 2)
	assert.Len(t, cc.Baseline.Tools, 2)
	assert.Equal(t, "high", cc.Baseline.Effort)
	assert.Nil(t, cc.Update)
	assert.JSONEq(t, string(turn.result.RequestContext), string(turn.result.CompactionContext))

	// The system block keeps its cache breakpoint; tools never carry one.
	blocks := turn.body["system"].([]any)
	assert.Contains(t, blocks[1], "cache_control")
	for _, tl := range turn.body["tools"].([]any) {
		assert.NotContains(t, tl, "cache_control")
	}
}

func TestConversationUpdates_RepeatRequestDeterministic(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("hi")
	first := h.turn([]string{"A"}, cuTools("read", "write"), "")
	h.reply(first, "ok")
	h.user("next")

	sys := []string{"A", "B"}
	one := h.turn(sys, cuTools("read"), "low", "remind")
	two := h.turn(sys, cuTools("read"), "low", "remind")

	assert.Equal(t, wireBytes(t, one.params), wireBytes(t, two.params))
	assert.Equal(t, string(one.result.RequestContext), string(two.result.RequestContext))
	assert.Equal(t, one.params.Betas, two.params.Betas)
	assert.False(t, one.result.Reset)
}

func TestConversationUpdates_AppendedSystem(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A", "B"}, cuTools("read"), "")
	h.reply(t1, "a1")
	h.user("u2")

	t2 := h.turn([]string{"A", "B", "C"}, cuTools("read"), "")
	assert.False(t, t2.result.Reset)
	assert.Equal(t, []string{"A", "B"}, systemTexts(t2.body), "top-level system is the baseline, byte for byte")
	assert.Equal(t, t1.body["system"], t2.body["system"])
	assert.Equal(t, []string{"user", "assistant", "user", "system"}, roles(t2.body))
	assert.Equal(t, []string{"text:C"}, blockSummary(systemMessages(t2.body)[0]))
	assert.NotContains(t, systemMessages(t2.body)[0], "clear_at")
	assert.Equal(t, []string{"C"}, decodeContext(t, t2.result.RequestContext).Update.Texts)
	assert.Equal(t, []anthropic.AnthropicBeta{anthropic.AnthropicBetaInterleavedThinking2025_05_14}, t2.params.Betas, "plain text needs no beta")
	h.reply(t2, "a2")
	h.user("u3")

	t3 := h.turn([]string{"A", "B", "C"}, cuTools("read"), "")
	assert.False(t, t3.result.Reset)
	assert.Equal(t, []string{"user", "assistant", "user", "system", "assistant", "user"}, roles(t3.body), "C replays before the assistant turn it preceded")
	assert.Nil(t, t3.result.RequestContext, "nothing new to record")
	h.reply(t3, "a3")
	h.user("u4")

	t4 := h.turn([]string{"A", "B", "C", "D"}, cuTools("read"), "")
	assert.Equal(t, []string{"user", "assistant", "user", "system", "assistant", "user", "assistant", "user", "system"}, roles(t4.body))
	assert.Equal(t, []string{"text:D"}, blockSummary(systemMessages(t4.body)[1]))
	assert.Equal(t, []string{"A", "B"}, systemTexts(t4.body))
}

func TestConversationUpdates_SystemReplacementResets(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A", "B"}, cuTools("read"), "")
	h.reply(t1, "a1")
	h.user("u2")
	t2 := h.turn([]string{"A", "B", "C"}, cuTools("read"), "")
	h.reply(t2, "a2")
	h.user("u3")

	for _, sys := range [][]string{{"A", "X", "C"}, {"A"}, {"A", "B"}} {
		t3, err := h.apply(h.buildParams(sys, cuTools("read"), ""))
		require.NoError(t, err)
		assert.True(t, t3.result.Reset, "%v", sys)
		assert.Equal(t, sys, systemTexts(t3.body), "the current system is sent, old instructions do not persist")
		assert.Empty(t, systemMessages(t3.body), "recorded updates are dropped with the old baseline")
		cc := decodeContext(t, t3.result.RequestContext)
		require.NotNil(t, cc.Baseline)
		assert.Len(t, cc.Baseline.System, len(sys))
	}

	t3 := h.turn([]string{"A", "X", "C"}, cuTools("read"), "")
	h.reply(t3, "a3")
	h.user("u4")
	t4 := h.turn([]string{"A", "X", "C"}, cuTools("read"), "")
	assert.False(t, t4.result.Reset, "the reset baseline is stable from then on")
	assert.Empty(t, systemMessages(t4.body))
}

func TestConversationUpdates_ToolAddRemove(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn(nil, cuTools("read", "write"), "")
	h.reply(t1, "a1")
	h.user("u2")

	t2 := h.turn(nil, cuTools("read"), "")
	assert.False(t, t2.result.Reset)
	assert.Equal(t, t1.body["tools"], t2.body["tools"], "the tools array never changes for a removal")
	assert.Equal(t, []string{"remove:write"}, blockSummary(systemMessages(t2.body)[0]))
	assert.Contains(t, t2.params.Betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
	assert.Equal(t, []string{"write"}, decodeContext(t, t2.result.RequestContext).Update.RemoveTools)
	h.reply(t2, "a2")
	h.user("u3")

	t3 := h.turn(nil, cuTools("read", "write"), "")
	assert.Equal(t, t1.body["tools"], t3.body["tools"])
	msgs := systemMessages(t3.body)
	require.Len(t, msgs, 2)
	assert.Equal(t, []string{"remove:write"}, blockSummary(msgs[0]))
	assert.Equal(t, []string{"add:write"}, blockSummary(msgs[1]), "a re-added declared tool is re-offered")
	h.reply(t3, "a3")
	h.user("u4")

	t4 := h.turn(nil, cuTools("read", "write", "search"), "")
	assert.False(t, t4.result.Reset)
	assert.Equal(t, []string{"read", "write", "search"}, toolNames(t4.body), "a new tool is declared at the end")
	assert.Equal(t, t1.body["tools"].([]any), t4.body["tools"].([]any)[:2], "the baseline tools are untouched")
	assert.Equal(t, true, t4.body["tools"].([]any)[2].(map[string]any)["defer_loading"], "declared withheld until the addition")
	assert.Equal(t, []string{"add:search"}, blockSummary(systemMessages(t4.body)[2]))
	cc := decodeContext(t, t4.result.RequestContext)
	assert.Len(t, cc.Update.DeclareTools, 1)
	assert.Equal(t, []string{"search"}, cc.Update.AddTools)
	h.reply(t4, "a4")
	h.user("u5")

	t5 := h.turn(nil, cuTools("read", "write", "search"), "")
	assert.Equal(t, t4.body["tools"], t5.body["tools"], "the extended tools array is stable")
	assert.Len(t, systemMessages(t5.body), 3)
	assert.Equal(t, "user", roles(t5.body)[len(roles(t5.body))-1])
	assert.Nil(t, t5.result.RequestContext)
	h.reply(t5, "a5")
	h.user("u6")

	t6 := h.turn(nil, cuTools("search"), "")
	assert.Equal(t, []string{"remove:read", "remove:write"}, blockSummary(systemMessages(t6.body)[3]), "removals follow declaration order")
}

func TestConversationUpdates_ToolDefinitionChangeResets(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn(nil, cuTools("read"), "")
	h.reply(t1, "a1")
	h.user("u2")

	changed := cuTool("read")
	changed.Description = "Reads a file, now with a longer description"
	t2 := h.turn(nil, []tools.Tool{changed}, "")
	assert.True(t, t2.result.Reset)
	assert.Equal(t, changed.Description, t2.body["tools"].([]any)[0].(map[string]any)["description"])
	assert.Empty(t, systemMessages(t2.body))
}

func TestConversationUpdates_DeferralAndCacheControlDoNotReset(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn(nil, cuTools("read", "write"), "")
	h.reply(t1, "a1", toolCall("c1", "read"))
	h.toolResult("c1", "contents")

	// The runtime's deferral tracker later marks write deferred, which moves
	// the tool-list cache breakpoint onto read and adds defer_loading.
	deferred := cuTools("read", "write")
	deferred[1].Deferred = true
	deferred[1].DeferredAtToolCallID = "c1"
	params := h.buildParams(nil, deferred, "")
	require.Contains(t, wireBytes(t, params), `"cache_control"`)
	t2, err := h.apply(params)
	require.NoError(t, err)
	assert.False(t, t2.result.Reset)
	assert.Empty(t, systemMessages(t2.body))
	assert.Equal(t, t1.body["tools"], t2.body["tools"], "baseline bytes are resent")
}

// TestConversationUpdates_BaselineToolsAreAlwaysLoaded covers a baseline
// fixed while the runtime had a tool deferred. The tool_reference that
// loads it lives in a tool result that a restart, an edit or a compaction
// can drop, so the baseline must not carry the deferral.
func TestConversationUpdates_BaselineToolsAreAlwaysLoaded(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read"), "")
	h.reply(t1, "a1", toolCall("c1", "read"))
	h.toolResult("c1", "x")

	deferred := cuTools("read", "write")
	deferred[1].Deferred = true
	deferred[1].DeferredAtToolCallID = "c1"
	params := h.buildParams([]string{"B"}, deferred, "")
	require.Contains(t, wireBytes(t, params), `"defer_loading":true`)
	t2, err := h.apply(params)
	require.NoError(t, err)
	require.True(t, t2.result.Reset, "the system rewrite restarts the baseline")
	for _, tl := range t2.body["tools"].([]any) {
		assert.NotContains(t, tl, "defer_loading")
	}
	for _, raw := range decodeContext(t, t2.result.RequestContext).Baseline.Tools {
		assert.NotContains(t, string(raw), "defer_loading")
	}
	h.reply(t2, "a2")

	// After a restart nothing is deferred and the load point is gone.
	h.hist = slices.Delete(h.hist, 1, 3)
	h.user("u2")
	t3 := h.turn([]string{"B"}, cuTools("read", "write"), "")
	assert.False(t, t3.result.Reset)
	assert.Equal(t, t2.body["tools"], t3.body["tools"], "write stays loaded")
	assert.Empty(t, systemMessages(t3.body))

	// A tool declared after the baseline is deferred until its addition
	// whether or not the runtime defers it.
	h.reply(t3, "a3")
	h.user("u3")
	t4 := h.turn([]string{"B"}, cuTools("read", "write", "search"), "")
	assert.False(t, t4.result.Reset)
	assert.Equal(t, true, t4.body["tools"].([]any)[2].(map[string]any)["defer_loading"])
	assert.Equal(t, []string{"add:search"}, blockSummary(systemMessages(t4.body)[0]))
}

func TestConversationUpdates_EffortChange(t *testing.T) {
	t.Parallel()

	t.Run("per-message effort on Opus 5", func(t *testing.T) {
		t.Parallel()
		h := newCUHarness(t, "claude-opus-5")
		h.user("u1")
		t1 := h.turn(nil, cuTools("read"), "high")
		h.reply(t1, "a1")
		h.user("u2")

		t2 := h.turn(nil, cuTools("read"), "low")
		assert.False(t, t2.result.Reset)
		assert.Equal(t, "high", t2.body["output_config"].(map[string]any)["effort"], "top-level effort stays at the baseline")
		msgs := systemMessages(t2.body)
		require.Len(t, msgs, 1)
		assert.Equal(t, []any{}, msgs[0]["content"], "effort-only messages carry empty content")
		assert.Equal(t, map[string]any{"effort": "low"}, msgs[0]["output_config"])
		assert.Contains(t, t2.params.Betas, anthropic.AnthropicBetaMidConversationOutputConfig2026_07_01)
		assert.NotContains(t, t2.params.Betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
		h.reply(t2, "a2")
		h.user("u3")

		t3 := h.turn(nil, cuTools("read"), "low")
		assert.Len(t, systemMessages(t3.body), 1, "the recorded change replays, nothing new is added")
		assert.Nil(t, t3.result.RequestContext)
		h.reply(t3, "a3")
		h.user("u4")

		t4 := h.turn(nil, cuTools("read"), "")
		assert.True(t, t4.result.Reset, "the model default cannot be named in a system message")
		assert.NotContains(t, t4.body, "output_config")
		assert.Empty(t, systemMessages(t4.body))
	})

	t.Run("top-level reset on Opus 4.8", func(t *testing.T) {
		t.Parallel()
		h := newCUHarness(t, "claude-opus-4-8")
		h.user("u1")
		t1 := h.turn(nil, cuTools("read"), "high")
		h.reply(t1, "a1")
		h.user("u2")

		t2 := h.turn(nil, cuTools("read"), "low")
		assert.True(t, t2.result.Reset)
		assert.Equal(t, "low", t2.body["output_config"].(map[string]any)["effort"])
		assert.Empty(t, systemMessages(t2.body))
	})

	t.Run("recorded per-message effort does not survive a model switch", func(t *testing.T) {
		t.Parallel()
		h := newCUHarness(t, "claude-opus-5")
		h.user("u1")
		t1 := h.turn(nil, cuTools("read"), "high")
		h.reply(t1, "a1")
		h.user("u2")
		t2 := h.turn(nil, cuTools("read"), "low")
		require.Equal(t, "low", decodeContext(t, t2.result.RequestContext).Update.Effort)
		h.reply(t2, "a2")
		h.user("u3")

		h.model = "claude-opus-4-8"
		t3 := h.turn(nil, cuTools("read"), "low")
		assert.True(t, t3.result.Reset)
		assert.Empty(t, systemMessages(t3.body), "the effort message is not replayed")
		assert.Equal(t, "low", t3.body["output_config"].(map[string]any)["effort"])
	})

	t.Run("unset is the model default, not a level", func(t *testing.T) {
		t.Parallel()
		h := newCUHarness(t, "claude-fable-5-1")
		h.user("u1")
		t1 := h.turn(nil, cuTools("read"), "")
		assert.NotContains(t, t1.body, "output_config")
		h.reply(t1, "a1")
		h.user("u2")

		t2 := h.turn(nil, cuTools("read"), "high")
		assert.False(t, t2.result.Reset)
		assert.NotContains(t, t2.body, "output_config", "the baseline keeps the default")
		assert.Equal(t, map[string]any{"effort": "high"}, systemMessages(t2.body)[0]["output_config"], "an explicit level is sent explicitly")
		h.reply(t2, "a2")
		h.user("u3")

		t3 := h.turn(nil, cuTools("read"), "")
		assert.True(t, t3.result.Reset, "back to the default is a reset, not an assumed level")
		assert.NotContains(t, t3.body, "output_config")
		assert.Empty(t, systemMessages(t3.body))
	})
}

func TestConversationUpdates_InvalidPlacement(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn(nil, cuTools("read", "write"), "high")
	h.reply(t1, "a1")
	// No trailing user turn: the history ends with the assistant.

	t2 := h.turn(nil, cuTools("read"), "high")
	assert.True(t, t2.result.Reset, "a content update cannot follow an assistant turn")
	assert.Equal(t, []string{"read"}, toolNames(t2.body))
	assert.Empty(t, systemMessages(t2.body))

	t3 := h.turn(nil, cuTools("read", "write"), "low")
	assert.False(t, t3.result.Reset, "an effort-only update is accepted anywhere")
	assert.Equal(t, []string{"user", "assistant", "system"}, roles(t3.body))

	_, err := h.apply(h.buildParams(nil, cuTools("read", "write"), "high"), "remind")
	require.ErrorContains(t, err, "end with a user turn")
}

func TestConversationUpdates_TransientMessages(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-fable-5-1")
	h.user("u1")

	t1 := h.turn(nil, cuTools("read"), "", "batch your reads")
	assert.False(t, t1.result.Reset)
	assert.Equal(t, []string{"user", "system"}, roles(t1.body))
	msg := systemMessages(t1.body)[0]
	assert.Equal(t, "next_user_message", msg["clear_at"])
	assert.Equal(t, []string{"text:batch your reads"}, blockSummary(msg))
	assert.NotContains(t, msg, "output_config")
	assert.Contains(t, t1.params.Betas, anthropic.AnthropicBetaMidConversationSystemClearAt2026_08_21)
	cc := decodeContext(t, t1.result.RequestContext)
	require.NotNil(t, cc.Baseline)
	assert.Equal(t, []string{"batch your reads"}, cc.Update.Transient)
	h.reply(t1, "a1", toolCall("c1", "read"))
	h.toolResult("c1", "data")

	t2 := h.turn(nil, cuTools("read"), "", "batch your reads")
	assert.Equal(t, []string{"user", "system", "assistant", "user", "system"}, roles(t2.body))
	first, last := systemMessages(t2.body)[0], systemMessages(t2.body)[1]
	assert.Equal(t, systemMessages(t1.body)[0], first, "a cleared reminder is resent verbatim")
	assert.Equal(t, "next_user_message", last["clear_at"])
	h.reply(t2, "a2")
	h.user("u2")

	t3 := h.turn(nil, cuTools("write"), "", "stay terse")
	msgs := systemMessages(t3.body)
	require.Len(t, msgs, 4)
	assert.Equal(t, []string{"add:write", "remove:read"}, blockSummary(msgs[2]), "persistent update precedes the transient one")
	assert.NotContains(t, msgs[2], "clear_at")
	assert.Equal(t, []string{"text:stay terse"}, blockSummary(msgs[3]))
	assert.Equal(t, "next_user_message", msgs[3]["clear_at"])
	assert.NotContains(t, msgs[3]["content"].([]any)[0], "cache_control")
}

func TestConversationUpdates_SessionReload(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read", "write"), "high", "r1")
	h.reply(t1, "a1", toolCall("c1", "read"))
	h.toolResult("c1", "x")
	t2 := h.turn([]string{"A", "B"}, cuTools("read"), "low")
	h.reply(t2, "a2")
	h.user("u2")
	t3 := h.turn([]string{"A", "B"}, cuTools("read", "search"), "low")
	h.reply(t3, "a3")
	h.user("u3")

	live := h.turn([]string{"A", "B"}, cuTools("read", "search"), "low")

	data, err := json.Marshal(h.hist)
	require.NoError(t, err)
	reloaded := newCUHarness(t, h.model)
	require.NoError(t, json.Unmarshal(data, &reloaded.hist))
	afterReload := reloaded.turn([]string{"A", "B"}, cuTools("read", "search"), "low")

	assert.Equal(t, wireBytes(t, live.params), wireBytes(t, afterReload.params))
	assert.Equal(t, live.params.Betas, afterReload.params.Betas)
	assert.False(t, afterReload.result.Reset)
}

func TestConversationUpdates_IndependentHistoriesOnSharedClient(t *testing.T) {
	t.Parallel()
	a := newCUHarness(t, "claude-opus-5")
	b := newCUHarness(t, "claude-opus-5")
	a.user("a1")
	b.user("b1")

	ta := a.turn([]string{"SA"}, cuTools("read"), "")
	tb := b.turn([]string{"SB"}, cuTools("write", "search"), "low")
	a.reply(ta, "ra")
	b.reply(tb, "rb")
	a.user("a2")
	b.user("b2")

	ta2 := a.turn([]string{"SA", "SA2"}, cuTools("read", "write"), "")
	tb2 := b.turn([]string{"SB"}, cuTools("write"), "low")

	assert.False(t, ta2.result.Reset)
	assert.False(t, tb2.result.Reset)
	assert.Equal(t, []string{"SA"}, systemTexts(ta2.body))
	assert.Equal(t, []string{"read", "write"}, toolNames(ta2.body))
	assert.Equal(t, []string{"text:SA2", "add:write"}, blockSummary(systemMessages(ta2.body)[0]))
	assert.Equal(t, []string{"SB"}, systemTexts(tb2.body))
	assert.Equal(t, []string{"write", "search"}, toolNames(tb2.body))
	assert.Equal(t, []string{"remove:search"}, blockSummary(systemMessages(tb2.body)[0]))
}

func TestConversationUpdates_PrefixEdit(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read"), "")
	h.reply(t1, "a1")
	h.user("u2")
	t2 := h.turn([]string{"A", "C"}, cuTools("read"), "")
	h.reply(t2, "a2")
	h.user("u3")
	t3 := h.turn([]string{"A", "C", "D"}, cuTools("read"), "")
	h.reply(t3, "a3")

	// The user edits u3: everything from there on is dropped and resent.
	h.hist = h.hist[:4]
	h.user("u3 edited")

	edited := h.turn([]string{"A", "C"}, cuTools("read"), "")
	assert.False(t, edited.result.Reset)
	assert.Equal(t, []string{"user", "assistant", "user", "system", "assistant", "user"}, roles(edited.body))
	assert.Equal(t, []string{"text:C"}, blockSummary(systemMessages(edited.body)[0]))
	assert.Nil(t, edited.result.RequestContext)

	grown := h.turn([]string{"A", "C", "D"}, cuTools("read"), "")
	assert.Equal(t, []string{"text:D"}, blockSummary(systemMessages(grown.body)[1]), "D is a fresh update again, not a replay")

	// Editing u1 leaves no assistant record: a fresh conversation.
	h.hist = h.hist[:0]
	h.user("u1 edited")
	fresh := h.turn([]string{"A", "C"}, cuTools("read"), "")
	assert.False(t, fresh.result.Reset)
	assert.Equal(t, []string{"A", "C"}, systemTexts(fresh.body))
	assert.NotNil(t, decodeContext(t, fresh.result.RequestContext).Baseline)
}

func TestConversationUpdates_CompactionBaseline(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read", "write"), "")
	h.reply(t1, "a1")
	h.user("u2")
	t2 := h.turn([]string{"A", "C"}, cuTools("read"), "low")
	h.reply(t2, "a2")
	h.user("u3")

	// The compaction request is the conversation request itself and gets
	// the same cache-preserving shape.
	compaction := h.turn([]string{"A", "C"}, cuTools("read"), "low")
	assert.Equal(t, []string{"A"}, systemTexts(compaction.body))
	assert.Len(t, systemMessages(compaction.body), 1)
	cc := decodeContext(t, compaction.result.CompactionContext)
	require.NotNil(t, cc.Baseline)
	assert.Len(t, cc.Baseline.System, 2, "the compaction baseline is the current prefix")
	assert.Len(t, cc.Baseline.Tools, 1)
	assert.Equal(t, "low", cc.Baseline.Effort)

	// Continue from the summary: the summary message carries the baseline.
	h.hist = []chat.Message{{
		Role:       chat.MessageRoleUser,
		Content:    "Session Summary: ...",
		Compaction: &chat.CompactionResult{Provider: providerStateName, Summary: "...", RequestContext: compaction.result.CompactionContext},
	}}
	h.user("u4")
	cont := h.turn([]string{"A", "C"}, cuTools("read"), "low")
	assert.False(t, cont.result.Reset, "the compaction context is the baseline, no replayed events")
	assert.Equal(t, []string{"A", "C"}, systemTexts(cont.body))
	assert.Equal(t, []string{"read"}, toolNames(cont.body))
	assert.Equal(t, "low", cont.body["output_config"].(map[string]any)["effort"])
	assert.Empty(t, systemMessages(cont.body))
	assert.Nil(t, cont.result.RequestContext)
	h.reply(cont, "a4")
	h.user("u5")

	next := h.turn([]string{"A", "C"}, cuTools("read", "write"), "low")
	assert.False(t, next.result.Reset)
	assert.Equal(t, []string{"read", "write"}, toolNames(next.body), "write is new to this baseline and gets declared")
	assert.Equal(t, []string{"add:write"}, blockSummary(systemMessages(next.body)[0]))

	// A foreign compaction result is ignored: with no baseline in the
	// prompt the orphaned record is dropped and the turn starts fresh.
	h.hist[0].Compaction.Provider = "openai"
	foreign := h.turn([]string{"A", "C"}, cuTools("read", "write"), "low")
	assert.False(t, foreign.result.Reset)
	assert.Empty(t, systemMessages(foreign.body))
	assert.Equal(t, []string{"read", "write"}, toolNames(foreign.body))
	assert.NotNil(t, decodeContext(t, foreign.result.RequestContext).Baseline)
}

// TestConversationUpdates_AlignsRawReplayedTurns checks the pairing against
// turns the converter replayed from their raw response (thinking, text and
// tool_use blocks built by the SDK), not from the flattened fields.
func TestConversationUpdates_AlignsRawReplayedTurns(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read", "write"), "")
	h.reply(t1, "", toolCall("c1", "read"), toolCall("c2", "write"))
	h.toolResult("c1", "x")
	h.toolResult("c2", "y")
	t2 := h.turn([]string{"A", "B"}, cuTools("read", "write"), "")
	assert.False(t, t2.result.Reset)
	h.reply(t2, "done")
	h.user("u2")

	params := h.buildParams([]string{"A", "B"}, cuTools("read", "write"), "")
	first := params.Messages[1]
	require.Equal(t, anthropic.BetaMessageParamRoleAssistant, first.Role)
	require.Len(t, first.Content, 3, "thinking + two tool_use blocks replayed raw")
	require.NotNil(t, first.Content[0].OfThinking)
	require.NotNil(t, first.Content[1].OfToolUse)

	t3, err := h.apply(params)
	require.NoError(t, err)
	assert.False(t, t3.result.Reset)
	assert.Equal(t, []string{"user", "assistant", "user", "system", "assistant", "user"}, roles(t3.body))
	assert.Equal(t, []string{"text:B"}, blockSummary(systemMessages(t3.body)[0]))
}

// TestConversationUpdates_IgnoresInvisibleAssistantTurns covers turns that
// exist on one side only: a native compaction block is an assistant wire
// message with no chat assistant behind it, and a thinking-only reply is a
// chat assistant the converter may drop.
func TestConversationUpdates_IgnoresInvisibleAssistantTurns(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read"), "")
	h.reply(t1, "a1")
	h.user("u2")
	t2 := h.turn([]string{"A", "C"}, cuTools("read"), "")
	h.reply(t2, "a2")
	h.user("u3")

	params := h.buildParams([]string{"A", "C"}, cuTools("read"), "")
	compaction := anthropic.BetaMessageParam{
		Role:    anthropic.BetaMessageParamRoleAssistant,
		Content: []anthropic.BetaContentBlockParamUnion{param.Override[anthropic.BetaContentBlockParamUnion](json.RawMessage(`{"type":"compaction","content":"..."}`))},
	}
	thinkingOnly := anthropic.BetaMessageParam{
		Role:    anthropic.BetaMessageParamRoleAssistant,
		Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaThinkingBlock("sig", "hmm")},
	}
	params.Messages = slices.Concat([]anthropic.BetaMessageParam{compaction}, params.Messages[:1], []anthropic.BetaMessageParam{thinkingOnly}, params.Messages[1:])

	turn, err := h.apply(params)
	require.NoError(t, err)
	assert.False(t, turn.result.Reset)
	assert.Equal(t, []string{"assistant", "user", "assistant", "assistant", "user", "system", "assistant", "user"}, roles(turn.body))

	// A record on a turn with no visible content has no reliable position.
	h.hist = append(h.hist, chat.Message{Role: chat.MessageRoleAssistant, ThinkingSignature: "sig"})
	h.hist[len(h.hist)-1].AttachProviderState(withRequestContext(nil, t2.result.RequestContext))
	h.user("u4")
	stale := h.turn([]string{"A", "C"}, cuTools("read"), "")
	assert.True(t, stale.result.Reset)
}

// TestConversationUpdates_StaleRecordResets covers a record that does not
// fit the baseline it is applied to (made against an older one, e.g. in a
// tail kept after a compaction). Replaying it would send tool changes the
// API rejects, so the baseline restarts instead.
func TestConversationUpdates_StaleRecordResets(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	first := h.turn(nil, cuTools("read"), "")
	baseline := decodeContext(t, first.result.RequestContext).Baseline
	require.Len(t, baseline.Tools, 1)

	for name, update := range map[string]*updateRecord{
		"declared twice":            {DeclareTools: baseline.Tools, AddTools: []string{"read"}},
		"added without declaration": {AddTools: []string{"search"}},
		"removed while not offered": {RemoveTools: []string{"write"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			baselineCtx, err := json.Marshal(conversationContext{Version: conversationContextVersion, Baseline: baseline})
			require.NoError(t, err)
			updateCtx, err := json.Marshal(conversationContext{Version: conversationContextVersion, Update: update})
			require.NoError(t, err)

			k := newCUHarness(t, h.model)
			k.hist = []chat.Message{{Role: chat.MessageRoleUser, Content: "summary", Compaction: &chat.CompactionResult{Provider: providerStateName, Summary: "s", RequestContext: baselineCtx}}}
			k.user("u2")
			k.reply(cuTurn{result: &conversationUpdateResult{RequestContext: updateCtx}}, "a2")
			k.user("u3")

			turn := k.turn(nil, cuTools("read"), "")
			assert.True(t, turn.result.Reset)
			assert.Empty(t, systemMessages(turn.body))
			assert.Equal(t, []string{"read"}, toolNames(turn.body))
		})
	}
}

func TestConversationUpdates_AlignmentMismatchResets(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read"), "")
	h.reply(t1, "a1")
	h.user("u2")
	t2 := h.turn([]string{"A", "C"}, cuTools("read"), "")
	h.reply(t2, "a2")
	h.user("u3")

	params := h.buildParams([]string{"A", "C"}, cuTools("read"), "")
	params.Messages = append(params.Messages[:1], params.Messages[2:]...) // drop the first assistant turn
	turn, err := h.apply(params)
	require.NoError(t, err)
	assert.True(t, turn.result.Reset)
	assert.Empty(t, systemMessages(turn.body))
	assert.Equal(t, []string{"A", "C"}, systemTexts(turn.body))
}

func TestConversationUpdates_IgnoresForeignAndMalformedContext(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	h.hist = append(h.hist, chat.Message{
		Role: chat.MessageRoleAssistant, Content: "a1",
		ProviderState: &chat.ProviderState{Provider: "openai", RequestContext: json.RawMessage(`{"v":1,"baseline":{"effort":"low"}}`)},
	})
	h.user("u2")
	h.hist = append(h.hist, chat.Message{
		Role: chat.MessageRoleAssistant, Content: "a2",
		ProviderState: &chat.ProviderState{Provider: providerStateName, RequestContext: json.RawMessage(`not json`)},
	})
	h.user("u3")
	h.hist = append(h.hist, chat.Message{
		Role: chat.MessageRoleAssistant, Content: "a3",
		ProviderState: &chat.ProviderState{Provider: providerStateName, RequestContext: json.RawMessage(`{"v":99,"baseline":{"effort":"low"}}`)},
	})
	h.user("u4")

	turn := h.turn([]string{"A"}, cuTools("read"), "high")
	assert.False(t, turn.result.Reset, "no usable baseline means a first turn")
	assert.NotNil(t, decodeContext(t, turn.result.RequestContext).Baseline)
	assert.Equal(t, "high", turn.body["output_config"].(map[string]any)["effort"])
}

func TestConversationUpdates_BetasNotDuplicated(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn(nil, cuTools("read", "write"), "")
	h.reply(t1, "a1")
	h.user("u2")

	params := h.buildParams(nil, cuTools("read"), "")
	params.Betas = append(params.Betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
	turn, err := h.apply(params)
	require.NoError(t, err)
	assert.Equal(t, []anthropic.AnthropicBeta{
		anthropic.AnthropicBetaInterleavedThinking2025_05_14,
		anthropic.AnthropicBetaMidConversationToolChanges2026_07_01,
	}, turn.params.Betas)
}

func TestConversationUpdates_DoesNotMutateInputs(t *testing.T) {
	t.Parallel()
	h := newCUHarness(t, "claude-opus-5")
	h.user("u1")
	t1 := h.turn([]string{"A"}, cuTools("read", "write"), "")
	h.reply(t1, "a1")
	h.user("u2")

	params := h.buildParams([]string{"A", "B"}, cuTools("read"), "low")
	converted, sys, betaTools := params.Messages, params.System, params.Tools
	before := wireBytes(t, params)
	histBefore, err := json.Marshal(h.hist)
	require.NoError(t, err)

	_, err = h.apply(params)
	require.NoError(t, err)

	after := wireBytes(t, anthropic.BetaMessageNewParams{Model: params.Model, MaxTokens: params.MaxTokens, Messages: converted, System: sys, Tools: betaTools, Betas: params.Betas, OutputConfig: params.OutputConfig})
	assert.Equal(t, before, after, "the caller's slices are left untouched")
	histAfter, err := json.Marshal(h.hist)
	require.NoError(t, err)
	assert.Equal(t, string(histBefore), string(histAfter))
}

func TestSupportsConversationUpdates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model           string
		updates, effort bool
	}{
		{"claude-opus-4-8", true, false},
		{"claude-opus-4-8-20260501", true, false},
		{"claude-opus-4.8", true, false},
		{"claude-opus-5", true, true},
		{"claude-opus-5-20260724", true, true},
		{"claude-opus-4-7", false, false},
		{"claude-opus-4-5-20251101", false, false},
		{"claude-fable-5", true, false},
		{"claude-fable-5-1", true, true},
		{"claude-mythos-5", true, false},
		{"claude-mythos-5-1", true, true},
		{"claude-sonnet-5", false, false},
		{"claude-sonnet-4-6", false, false},
		{"claude-haiku-4-5", false, false},
		{"claude-3-opus-20240229", false, false},
		{"us.anthropic.claude-opus-5-v1:0", true, true},
		{"global.anthropic.claude-fable-5-1", true, true},
		{"claude-opus-5@20260724", true, true},
		{"openrouter/anthropic/claude-opus-4-8", true, false},
		{"gpt-5", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.updates, supportsConversationUpdates(tt.model), "updates")
			assert.Equal(t, tt.effort, supportsPerMessageEffort(tt.model), "effort")
		})
	}
}

func TestValidateCachePreservingUpdates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     latest.ModelConfig
		wantErr string
	}{
		{name: "disabled", cfg: latest.ModelConfig{Model: "claude-sonnet-5"}},
		{name: "explicitly off", cfg: latest.ModelConfig{Model: "claude-sonnet-5", ProviderOpts: map[string]any{cachePreservingUpdatesOpt: false}}},
		{name: "supported", cfg: latest.ModelConfig{Model: "claude-opus-5", ProviderOpts: map[string]any{cachePreservingUpdatesOpt: true}}},
		{name: "sonnet 5", cfg: latest.ModelConfig{Model: "claude-sonnet-5", ProviderOpts: map[string]any{cachePreservingUpdatesOpt: true}}, wantErr: `"claude-sonnet-5" does not support cache_preserving_updates`},
		{name: "unsupported fallback", cfg: latest.ModelConfig{Model: "claude-fable-5-1", ProviderOpts: map[string]any{cachePreservingUpdatesOpt: true, "fallbacks": []any{"claude-opus-4-8", "claude-sonnet-4-6"}}}, wantErr: `"claude-sonnet-4-6" does not support`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCachePreservingUpdates(&tt.cfg)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestWithRequestContext(t *testing.T) {
	t.Parallel()
	ctx := json.RawMessage(`{"v":1}`)

	assert.Nil(t, withRequestContext(nil, nil))
	state := &chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(`[{"type":"text","text":"x"}]`)}
	assert.Same(t, state, withRequestContext(state, nil))

	sealed := withRequestContext(state, ctx)
	assert.Equal(t, ctx, sealed.RequestContext)
	assert.Nil(t, state.RequestContext, "the input is not mutated")
	assert.Equal(t, state.Content, sealed.Content)

	fromNil := withRequestContext(nil, ctx)
	assert.Equal(t, providerStateName, fromNil.Provider)
	assert.Equal(t, ctx, fromNil.RequestContext)
	msg := chat.Message{Role: chat.MessageRoleAssistant, Content: "x"}
	msg.AttachProviderState(fromNil)
	params, ok, err := betaReplayContent(&msg, false)
	require.NoError(t, err)
	assert.False(t, ok, "a content-less state falls back to the flattened fields")
	assert.Nil(t, params)
}

func TestCacheUpdatesRequireCompatibleEffortFallback(t *testing.T) {
	t.Parallel()
	err := validateCachePreservingUpdates(&latest.ModelConfig{Model: "claude-opus-5", ProviderOpts: map[string]any{
		cachePreservingUpdatesOpt: true, "fallbacks": []string{"claude-opus-4-8"},
	}})
	require.ErrorContains(t, err, "per-message effort")
}
