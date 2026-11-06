package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	base "github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockCompactor is a provider implementing contracts.ConversationCompactor.
// It records the last CompactConversation input and replies with a canned
// result or error.
type mockCompactor struct {
	id     string
	opts   map[string]any
	result *chat.CompactionResult
	err    error
	onCall func()

	mu       sync.Mutex
	calls    int
	messages []chat.Message
	tools    []tools.Tool
	prompt   string
}

func (m *mockCompactor) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(m.id) }

func (m *mockCompactor) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &mockStream{}, nil
}

func (m *mockCompactor) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{ProviderOpts: m.opts}}
}

func (m *mockCompactor) CompactConversation(_ context.Context, messages []chat.Message, requestTools []tools.Tool, instructions string) (*chat.CompactionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.onCall != nil {
		m.onCall()
	}
	m.messages = messages
	m.tools = requestTools
	m.prompt = instructions
	return m.result, m.err
}

func (m *mockCompactor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// tracedWrapper mimics the instrumentation wrapper: it hides the leaf behind
// Unwrap and does not itself implement ConversationCompactor.
type tracedWrapper struct{ inner provider.Provider }

func (t *tracedWrapper) ID() modelsdev.ID        { return t.inner.ID() }
func (t *tracedWrapper) BaseConfig() base.Config { return t.inner.BaseConfig() }
func (t *tracedWrapper) Unwrap() provider.Provider {
	return t.inner
}

func (t *tracedWrapper) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) (chat.MessageStream, error) {
	return t.inner.CreateChatCompletionStream(ctx, messages, requestTools)
}

type pricedModelStore struct {
	ModelStore
}

func (pricedModelStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	return &modelsdev.Model{
		Limit: modelsdev.Limit{Context: 100_000},
		Cost:  &modelsdev.Cost{Input: 1, Output: 10},
	}, nil
}

var nativeOpts = map[string]any{nativeCompactionOpt: true}

func nativeResult() *chat.CompactionResult {
	return &chat.CompactionResult{
		Summary:  "native summary",
		Block:    json.RawMessage(`{"type":"compaction","content":"opaque"}`),
		Provider: "anthropic",
		Model:    "claude-test",
		Usage:    chat.Usage{InputTokens: 1_000, OutputTokens: 100},
	}
}

func nativeTestSession() *session.Session {
	return session.New(session.WithID("native-compaction"), session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessage("old question")),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "old answer"}}),
		session.NewMessageItem(session.UserMessage("latest question")),
	}))
}

type compactionOutcome struct {
	outcome string
	summary *SessionSummaryEvent
	errors  []string
}

func runCompaction(t *testing.T, rt *LocalRuntime, sess *session.Session, prompt string) compactionOutcome {
	t.Helper()
	events := make(chan Event, 64)
	require.NoError(t, rt.compactWithReason(t.Context(), sess, prompt, compactionReasonManual, NewChannelSink(events)))
	close(events)

	var out compactionOutcome
	for ev := range events {
		switch e := ev.(type) {
		case *SessionCompactionEvent:
			if e.Status == "completed" {
				out.outcome = e.Outcome
			}
		case *SessionSummaryEvent:
			out.summary = e
		case *ErrorEvent:
			out.errors = append(out.errors, e.Error)
		}
	}
	return out
}

func newNativeRuntime(t *testing.T, root *agent.Agent, opts ...Opt) *LocalRuntime {
	t.Helper()
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		append([]Opt{WithSessionCompaction(false), WithModelStore(pricedModelStore{})}, opts...)...)
	require.NoError(t, err)
	return rt
}

func TestNativeCompactionReplacesWholePromptAndPersistsBlock(t *testing.T) {
	t.Parallel()

	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	root := agent.New("root", "system instruction", agent.WithModel(&tracedWrapper{inner: comp}))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()
	before := sess.ItemCount()

	got := runCompaction(t, rt, sess, "focus on decisions")

	assert.Equal(t, CompactionOutcomeApplied, got.outcome)
	assert.Empty(t, got.errors)
	require.Equal(t, 1, comp.callCount(), "the traced wrapper must be unwrapped to reach the compactor")
	assert.Equal(t, "focus on decisions", comp.prompt)

	// The compactor receives the assembled prompt, system instruction
	// included, and no canonical compaction prompt.
	require.NotEmpty(t, comp.messages)
	assert.Equal(t, chat.MessageRoleSystem, comp.messages[0].Role)
	assert.Equal(t, "system instruction", comp.messages[0].Content)
	for _, msg := range comp.messages {
		assert.NotContains(t, msg.Content, "summary", "no canonical compaction prompt: %q", msg.Content)
	}

	require.Len(t, sess.Messages, before+1)
	item := sess.Messages[before]
	assert.Equal(t, "native summary", item.Summary)
	assert.Equal(t, before, item.FirstKeptEntry, "whole prompt compacted: nothing before the snapshot is kept")
	assert.Equal(t, "anthropic/claude-test", item.Model)
	require.NotNil(t, item.Compaction)
	assert.JSONEq(t, `{"type":"compaction","content":"opaque"}`, string(item.Compaction.Block))
	assert.Equal(t, "anthropic", item.Compaction.Provider)
	require.NotNil(t, item.Usage)
	assert.Equal(t, int64(1_000), item.Usage.InputTokens)
	// 1000 input @ $1/M + 100 output @ $10/M.
	assert.InDelta(t, 0.002, item.Cost, 1e-9)
	assert.InDelta(t, 0.002, sess.TotalCost(), 1e-9)
	input, output := sess.Usage()
	assert.Positive(t, input)
	assert.Zero(t, output)

	require.NotNil(t, got.summary)
	assert.Equal(t, "native summary", got.summary.Summary)
	assert.InDelta(t, 0.002, got.summary.Cost, 1e-9)

	// The continuation carries only the summary; the producing provider sees
	// the block, everyone else the readable text.
	var conversation []chat.Message
	for _, msg := range sess.GetMessages(root) {
		if msg.Role != chat.MessageRoleSystem {
			conversation = append(conversation, msg)
		}
	}
	require.Len(t, conversation, 1)
	assert.Equal(t, session.SummaryMessageContent("native summary"), conversation[0].Content)
	require.NotNil(t, conversation[0].ReplayableCompaction("anthropic"))
	assert.Nil(t, conversation[0].ReplayableCompaction("openai"))
}

func TestNativeCompactionKeepsItemsAppendedAfterSnapshot(t *testing.T) {
	t.Parallel()

	sess := nativeTestSession()
	comp := &appendingCompactor{
		mockCompactor: &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()},
		sess:          sess,
	}
	root := agent.New("root", "test", agent.WithModel(comp))
	rt := newNativeRuntime(t, root)

	got := runCompaction(t, rt, sess, "")
	assert.Equal(t, CompactionOutcomeApplied, got.outcome)

	var conversation []string
	for _, msg := range sess.GetMessages(root) {
		if msg.Role != chat.MessageRoleSystem {
			conversation = append(conversation, msg.Content)
		}
	}
	assert.Equal(t, []string{session.SummaryMessageContent("native summary"), "late message"}, conversation,
		"an item appended after the snapshot is not covered by the summary and must be kept")
}

// appendingCompactor appends to the session while compaction is in flight,
// simulating a concurrent AddMessage between the prompt snapshot and persist.
type appendingCompactor struct {
	*mockCompactor

	sess *session.Session
}

func (a *appendingCompactor) CompactConversation(ctx context.Context, messages []chat.Message, requestTools []tools.Tool, instructions string) (*chat.CompactionResult, error) {
	a.sess.AddMessage(session.UserMessage("late message"))
	return a.mockCompactor.CompactConversation(ctx, messages, requestTools, instructions)
}

func TestNativeCompactionRequiresOptIn(t *testing.T) {
	t.Parallel()

	// Implements the interface but native_compaction is not set: the LLM
	// strategy must run instead.
	comp := &mockCompactor{id: "anthropic/claude-test", result: nativeResult()}
	summaryStream := newStreamBuilder().AddContent("llm summary").AddStopWithUsage(1, 1).Build()
	llm := &queueProvider{id: "anthropic/claude-test", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(comp), agent.WithCompactionModel(llm))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeApplied, got.outcome)
	assert.Zero(t, comp.callCount())
	last := sess.Messages[len(sess.Messages)-1]
	assert.Equal(t, "llm summary", last.Summary)
	assert.Nil(t, last.Compaction)
}

func TestNativeCompactionRejectsUnsupportedModel(t *testing.T) {
	t.Parallel()

	summaryStream := newStreamBuilder().AddContent("llm summary").AddStopWithUsage(1, 1).Build()
	prov := &providerOptsProvider{id: "openai/gpt-test", opts: nativeOpts}
	llm := &queueProvider{id: "openai/gpt-test", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithCompactionModel(llm))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()
	before := len(sess.Messages)

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeFailed, got.outcome)
	require.Len(t, got.errors, 1)
	assert.Contains(t, got.errors[0], "does not support provider-native compaction")
	assert.Len(t, sess.Messages, before, "an explicit opt-in must not silently fall back to the LLM strategy")
}

func TestNativeCompactionRunsBeforeLLMCallHooks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping test relying on POSIX shell commands on Windows")
	}

	hookCfg := &latest.HooksConfig{
		BeforeLLMCall: []latest.HookDefinition{{
			Type:    "command",
			Command: `echo '{"hook_specific_output":{"hook_event_name":"before_llm_call","updated_messages":[{"role":"user","content":"[REDACTED]"}]}}'`,
			Timeout: 5,
		}},
	}
	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	root := agent.New("root", "test", agent.WithModel(comp), agent.WithHooks(hookCfg))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeApplied, got.outcome)
	require.Equal(t, 1, comp.callCount())
	require.Len(t, comp.messages, 1, "the compactor must receive the hook rewrite, not the raw prompt")
	assert.Equal(t, "[REDACTED]", comp.messages[0].Content)
}

func TestNativeCompactionBlockedByBeforeLLMCallHook(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping test relying on POSIX shell commands on Windows")
	}

	hookCfg := &latest.HooksConfig{
		BeforeLLMCall: []latest.HookDefinition{{
			Type:    "command",
			Command: `echo '{"decision":"block","reason":"no outbound calls"}'`,
			Timeout: 5,
		}},
	}
	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	root := agent.New("root", "test", agent.WithModel(comp), agent.WithHooks(hookCfg))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()
	before := len(sess.Messages)

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeFailed, got.outcome)
	require.Len(t, got.errors, 1)
	assert.Contains(t, got.errors[0], "no outbound calls")
	assert.Zero(t, comp.callCount())
	assert.Len(t, sess.Messages, before)
}

func TestNativeCompactionRejectsForeignCompactionModel(t *testing.T) {
	t.Parallel()

	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	other := &mockProvider{id: "openai/gpt-test", stream: &mockStream{}}
	root := agent.New("root", "test", agent.WithModel(comp), agent.WithCompactionModel(other))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()
	before := len(sess.Messages)

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeFailed, got.outcome)
	require.Len(t, got.errors, 1)
	assert.Contains(t, got.errors[0], "compaction_model")
	assert.Zero(t, comp.callCount(), "an incompatible compaction_model must not be silently ignored")
	assert.Len(t, sess.Messages, before)
}

func TestNativeCompactionErrorLeavesTranscriptIntact(t *testing.T) {
	t.Parallel()

	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, err: errors.New("boom")}
	root := agent.New("root", "test", agent.WithModel(comp))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()
	before := len(sess.Messages)
	sess.SetTokensAndCost(500, 50, 0.1)

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeFailed, got.outcome)
	require.Len(t, got.errors, 1)
	assert.Contains(t, got.errors[0], "boom")
	assert.Nil(t, got.summary)
	assert.Len(t, sess.Messages, before)
	input, output := sess.Usage()
	assert.Equal(t, int64(500), input)
	assert.Equal(t, int64(50), output)
}

func TestNativeCompactionEmptySummaryIsNoop(t *testing.T) {
	t.Parallel()

	for name, result := range map[string]*chat.CompactionResult{
		"nil":        nil,
		"whitespace": {Summary: "  \n", Provider: "anthropic", Block: json.RawMessage(`{}`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: result}
			root := agent.New("root", "test", agent.WithModel(comp))
			rt := newNativeRuntime(t, root)
			sess := nativeTestSession()
			before := len(sess.Messages)

			got := runCompaction(t, rt, sess, "")

			assert.Equal(t, CompactionOutcomeSkipped, got.outcome)
			assert.Empty(t, got.errors)
			assert.Nil(t, got.summary)
			assert.Len(t, sess.Messages, before)
		})
	}
}

func TestNativeCompactionHookSummaryBypassesProvider(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping test relying on POSIX shell commands on Windows")
	}

	const hookSummary = "hook summary"
	hookCfg := &latest.HooksConfig{
		BeforeCompaction: []latest.HookDefinition{{
			Type:    "command",
			Command: `echo '{"hook_specific_output":{"hook_event_name":"before_compaction","summary":"` + hookSummary + `"}}'`,
			Timeout: 5,
		}},
	}
	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	root := agent.New("root", "test", agent.WithModel(comp), agent.WithHooks(hookCfg))
	rt := newNativeRuntime(t, root)
	sess := nativeTestSession()

	got := runCompaction(t, rt, sess, "")

	assert.Equal(t, CompactionOutcomeApplied, got.outcome)
	assert.Zero(t, comp.callCount(), "a hook-supplied summary must bypass native compaction")
	last := sess.Messages[len(sess.Messages)-1]
	assert.Equal(t, hookSummary, last.Summary)
	assert.Nil(t, last.Compaction)
}

func TestNativeCompactionSurvivesSQLiteReload(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	root := agent.New("root", "test", agent.WithModel(comp))
	rt := newNativeRuntime(t, root, WithSessionStore(store))
	sess := nativeTestSession()
	require.NoError(t, store.AddSession(t.Context(), sess))

	got := runCompaction(t, rt, sess, "")
	require.Equal(t, CompactionOutcomeApplied, got.outcome)
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	reloaded, err := reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)

	require.Len(t, reloaded.Messages, 4)
	item := reloaded.Messages[3]
	assert.Equal(t, "native summary", item.Summary)
	assert.Equal(t, 3, item.FirstKeptEntry)
	require.NotNil(t, item.Compaction)
	assert.Equal(t, sess.Messages[3].Compaction, item.Compaction)

	// Resuming on a provider that did not produce the block gets the
	// readable summary and no replayable payload.
	for _, msg := range reloaded.GetMessages(root) {
		if msg.Compaction != nil {
			assert.Equal(t, session.SummaryMessageContent("native summary"), msg.Content)
			assert.Nil(t, msg.ReplayableCompaction("openai"))
			assert.NotNil(t, msg.ReplayableCompaction("anthropic"))
			return
		}
	}
	t.Fatal("expected the reloaded prompt to carry the compaction on its summary message")
}

func TestPendingNativeCompactionTools(t *testing.T) {
	t.Parallel()
	sess := nativeTestSession()
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "one"}, {ID: "two"}}}})
	assert.True(t, hasPendingCompactionTools(sess))
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "one", Content: "done"}})
	assert.True(t, hasPendingCompactionTools(sess))
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "two", Content: "done"}})
	assert.False(t, hasPendingCompactionTools(sess))
}

// Each native compaction iteration is a request priced on its own: two
// iterations below a context tier must not be priced as one above it.
func TestNativeCompactionPricesEachIteration(t *testing.T) {
	t.Parallel()

	result := nativeResult()
	result.Iterations = []chat.Usage{{InputTokens: 60_000}, {InputTokens: 60_000}}
	result.Usage = chat.Usage{InputTokens: 120_000}
	comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: result}
	root := agent.New("root", "system instruction", agent.WithModel(&tracedWrapper{inner: comp}))
	tiered := modelsdev.Cost{Input: 1, Tiers: []modelsdev.CostTier{{
		Rates: modelsdev.Rates{Input: 5}, Tier: modelsdev.TierSpec{Type: "context", Size: 100_000},
	}}}
	rt := newNativeRuntime(t, root, WithModelStore(mockModelStoreWithCost{cost: tiered}))
	sess := nativeTestSession()

	got := runCompaction(t, rt, sess, "")

	require.Equal(t, CompactionOutcomeApplied, got.outcome)
	require.NotNil(t, got.summary)
	assert.InDelta(t, 2*60_000*1/1e6, got.summary.Cost, 1e-9, "base rate for each iteration, not the tier rate on the sum")
	assert.Equal(t, int64(120_000), sess.Messages[len(sess.Messages)-1].Usage.InputTokens, "token accounting keeps the total")
}

func TestCompactionCostUnpricedIteration(t *testing.T) {
	t.Parallel()

	res := &chat.CompactionResult{Iterations: []chat.Usage{{InputTokens: 10}, {InputTokens: 10}}}
	assert.Nil(t, compactionCost(res, nil, modelsdev.ParseIDOrZero("test/unknown"), base.Config{}), "an unpriced iteration leaves the compaction unpriced, not free")
}
