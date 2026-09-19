package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestCompactIfNeeded_IgnoresSubSessionTokens is a regression test for
// issue #2871: in a multi-agent run, the tokens accumulated inside a
// transfer_task sub-session were counted by the proactive compaction
// trigger (GetAllMessages recurses into sub-sessions) even though they
// never enter the parent's prompt (GetMessages skips sub-session items).
// The phantom tokens made the parent compact its own tiny conversation;
// with everything fitting the keep budget that meant "compact
// everything, keep nothing" — the agent's next prompt was just the
// summary and it halted with a confused "no conversation history" reply.
func TestCompactIfNeeded_IgnoresSubSessionTokens(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("build the app"))
	messageCountBefore := len(sess.OwnMessages())

	// Simulate a completed transfer_task tool call: a sub-session holding
	// far more content than the parent's context limit, plus a small
	// tool-result message on the parent itself.
	sub := session.New(session.WithUserMessage("subtask"))
	sub.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: strings.Repeat("z", 600_000), // ~150k estimated tokens
	}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:      chat.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{{ID: "t1", Function: tools.FunctionCall{Name: "transfer_task"}}},
	}))
	sess.AddSubSession(sub)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "t1",
		Content:    "subtask done",
	}))

	events := make(chan Event, 16)
	rt.compactIfNeeded(t.Context(), sess, root, 100_000, messageCountBefore, NewChannelSink(events))
	close(events)

	for ev := range events {
		_, isCompaction := ev.(*SessionCompactionEvent)
		assert.False(t, isCompaction,
			"sub-session tokens must not trigger compaction of the parent session")
	}
}

// TestCompactIfNeeded_TriggersOnOwnMessages pins the complementary case:
// large tool results recorded directly on the session still trigger the
// proactive compaction.
func TestCompactIfNeeded_TriggersOnOwnMessages(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("build the app"))
	messageCountBefore := len(sess.OwnMessages())

	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:      chat.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{{ID: "t1", Function: tools.FunctionCall{Name: "shell"}}},
	}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "t1",
		Content:    strings.Repeat("z", 600_000), // ~150k estimated tokens
	}))

	events := make(chan Event, 16)
	rt.compactIfNeeded(t.Context(), sess, root, 100_000, messageCountBefore, NewChannelSink(events))
	close(events)

	sawCompaction := false
	for ev := range events {
		if _, ok := ev.(*SessionCompactionEvent); ok {
			sawCompaction = true
		}
	}
	assert.True(t, sawCompaction, "large own tool results must still trigger compaction")
}

func TestCompactIfNeeded_TriggersOnReplayedReasoningHistory(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("continue"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		Content:          "working",
		ReasoningContent: strings.Repeat("r", 350_000),
	}))
	// This is the last provider-reported prompt size from before reasoning
	// replay was enabled. The stored history estimate must override it.
	sess.SetUsage(50_000, 1_000)

	events := make(chan Event, 16)
	rt.compactIfNeeded(t.Context(), sess, root, 100_000, len(sess.OwnMessages()), NewChannelSink(events))
	close(events)

	var compacted bool
	for ev := range events {
		if _, ok := ev.(*SessionCompactionEvent); ok {
			compacted = true
		}
	}
	assert.True(t, compacted, "stored replayable reasoning must contribute to the compaction trigger")
}

func TestCompactIfNeeded_IgnoresReasoningRemovedByCompaction(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("old request"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		Content:          "old answer",
		ReasoningContent: strings.Repeat("r", 350_000),
	}))
	sess.ApplyCompaction(20_000, 5_000, session.Item{
		Summary:        "The old work was summarized.",
		FirstKeptEntry: len(sess.OwnMessages()),
	})
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleUser, Content: "continue"}))

	events := make(chan Event, 16)
	rt.compactIfNeeded(t.Context(), sess, root, 100_000, len(sess.OwnMessages()), NewChannelSink(events))
	close(events)

	for ev := range events {
		_, compacted := ev.(*SessionCompactionEvent)
		assert.False(t, compacted, "reasoning replaced by a compaction summary must not trigger another compaction")
	}
}

// TestCompactIfNeeded_CustomThreshold verifies that the agent's configured
// compaction_threshold replaces the 0.9 default in the proactive trigger:
// the same session content that stays under the default threshold triggers
// compaction once a lower threshold is configured.
func TestCompactIfNeeded_CustomThreshold(t *testing.T) {
	t.Parallel()

	// ~150k chars ≈ ~37.5k estimated tokens: 37.5% of the 100k window —
	// under the 0.9 default, over a 0.25 threshold.
	buildSession := func() (*session.Session, int) {
		sess := session.New(session.WithUserMessage("build the app"))
		before := len(sess.OwnMessages())
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{{ID: "t1", Function: tools.FunctionCall{Name: "shell"}}},
		}))
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
			Role:       chat.MessageRoleTool,
			ToolCallID: "t1",
			Content:    strings.Repeat("z", 150_000),
		}))
		return sess, before
	}

	run := func(t *testing.T, agentOpts ...agent.Opt) bool {
		t.Helper()
		prov := &mockProvider{id: "test/model", stream: &mockStream{}}
		root := agent.New("root", "agent", append([]agent.Opt{agent.WithModel(prov)}, agentOpts...)...)
		tm := team.New(team.WithAgents(root))

		rt, err := NewLocalRuntime(t.Context(), tm,
			WithSessionCompaction(true),
			WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
		require.NoError(t, err)

		sess, before := buildSession()
		events := make(chan Event, 16)
		rt.compactIfNeeded(t.Context(), sess, root, 100_000, before, NewChannelSink(events))
		close(events)

		for ev := range events {
			if _, ok := ev.(*SessionCompactionEvent); ok {
				return true
			}
		}
		return false
	}

	t.Run("default threshold does not trigger at 37% usage", func(t *testing.T) {
		t.Parallel()
		assert.False(t, run(t), "37%% usage must not trigger compaction at the 0.9 default")
	})

	t.Run("lower threshold triggers on the same session", func(t *testing.T) {
		t.Parallel()
		assert.True(t, run(t, agent.WithCompactionThreshold(0.25)),
			"37%% usage must trigger compaction with compaction_threshold: 0.25")
	})

	t.Run("threshold of 1 suppresses the trigger even at high usage", func(t *testing.T) {
		t.Parallel()
		prov := &mockProvider{id: "test/model", stream: &mockStream{}}
		root := agent.New("root", "agent", agent.WithModel(prov), agent.WithCompactionThreshold(1))
		tm := team.New(team.WithAgents(root))

		rt, err := NewLocalRuntime(t.Context(), tm,
			WithSessionCompaction(true),
			WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("build the app"))
		before := len(sess.OwnMessages())
		sess.InputTokens = 95_000 // over the 0.9 default, under 1.0

		events := make(chan Event, 16)
		rt.compactIfNeeded(t.Context(), sess, root, 100_000, before, NewChannelSink(events))
		close(events)

		for ev := range events {
			_, isCompaction := ev.(*SessionCompactionEvent)
			assert.False(t, isCompaction, "95%% usage must not trigger compaction with compaction_threshold: 1")
		}
	})
}

// TestCompactIfNeeded_AgentSessionCompactionDisabled verifies that an agent
// with `session_compaction: false` never auto-compacts, even when the
// runtime-level session-compaction option is on and the context usage is
// far past the threshold.
func TestCompactIfNeeded_AgentSessionCompactionDisabled(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	root := agent.New("root", "agent", agent.WithModel(prov), agent.WithSessionCompaction(false))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("build the app"))
	messageCountBefore := len(sess.OwnMessages())

	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:      chat.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{{ID: "t1", Function: tools.FunctionCall{Name: "shell"}}},
	}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "t1",
		Content:    strings.Repeat("z", 600_000), // ~150k estimated tokens
	}))

	events := make(chan Event, 16)
	rt.compactIfNeeded(t.Context(), sess, root, 100_000, messageCountBefore, NewChannelSink(events))
	close(events)

	for ev := range events {
		_, isCompaction := ev.(*SessionCompactionEvent)
		assert.False(t, isCompaction,
			"session_compaction: false must suppress the proactive compaction trigger")
	}
}

// TestCompactIfNeeded_UsageCalibratedTrigger pins the estimator
// reconciliation from issue #3437: identical fresh tool results either
// trigger or don't trigger proactive compaction depending on what the
// session's provider-reported usage said about earlier, similar
// content.
//
// Both sessions carry ~43k provider-counted tokens and add a 120k-char
// tool result (heuristic ≈ 34k tokens, uncalibrated total ≈ 77k of the
// 100k window — under the 90% threshold). The calibrated session's
// anchors additionally prove the heuristic undercounts this content 2×
// (prompt deltas of 40k tokens for 70k-char results), pushing the
// estimate past the threshold.
func TestCompactIfNeeded_UsageCalibratedTrigger(t *testing.T) {
	t.Parallel()

	const contextLimit = 100_000

	newRuntime := func(t *testing.T) (*LocalRuntime, *agent.Agent) {
		t.Helper()
		prov := &mockProvider{id: "test/model", stream: &mockStream{}}
		root := agent.New("root", "agent", agent.WithModel(prov))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
			WithSessionCompaction(true),
			WithModelStore(mockModelStoreWithLimit{limit: contextLimit}))
		require.NoError(t, err)
		return rt, root
	}

	assistantMsg := func(usage *chat.Usage) *session.Message {
		return session.NewAgentMessage("root", &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   "running tools",
			Model:     "test/model",
			Usage:     usage,
			ToolCalls: []tools.ToolCall{{ID: "t1", Function: tools.FunctionCall{Name: "shell"}}},
		})
	}
	toolMsg := func(size int) *session.Message {
		return session.NewAgentMessage("root", &chat.Message{
			Role:       chat.MessageRoleTool,
			ToolCallID: "t1",
			Content:    strings.Repeat("z", size),
		})
	}

	runScenario := func(t *testing.T, calibrated bool) bool {
		t.Helper()
		rt, root := newRuntime(t)

		sess := session.New(session.WithUserMessage("build the app"))
		var anchors []*chat.Usage
		if calibrated {
			// 70k-char tool result between the anchors: heuristic says
			// ~20_005 tokens, the prompt delta says 40_010 → scale 2.0.
			anchors = []*chat.Usage{
				{InputTokens: 3_000, OutputTokens: 100},
				{InputTokens: 43_110, OutputTokens: 100},
			}
		}
		usageAt := func(i int) *chat.Usage {
			if anchors == nil {
				return nil
			}
			return anchors[i]
		}
		sess.AddMessage(assistantMsg(usageAt(0)))
		sess.AddMessage(toolMsg(70_000))
		sess.AddMessage(assistantMsg(usageAt(1)))
		sess.SetUsage(43_110, 100)

		messageCountBefore := len(sess.OwnMessages())
		sess.AddMessage(toolMsg(120_000))

		events := make(chan Event, 16)
		rt.compactIfNeeded(t.Context(), sess, root, contextLimit, messageCountBefore, NewChannelSink(events))
		close(events)

		for ev := range events {
			if _, ok := ev.(*SessionCompactionEvent); ok {
				return true
			}
		}
		return false
	}

	t.Run("provider-calibrated estimate crosses the threshold", func(t *testing.T) {
		t.Parallel()
		assert.True(t, runScenario(t, true),
			"usage anchors proving 2× undercount must push the estimate past 90%")
	})

	t.Run("without usage anchors the heuristic stays below the threshold", func(t *testing.T) {
		t.Parallel()
		assert.False(t, runScenario(t, false),
			"same content without usage history must not trigger compaction")
	})
}
