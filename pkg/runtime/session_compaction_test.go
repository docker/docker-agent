package runtime

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// TestSessionGetMessages_WithFirstKeptEntry covers the session-side
// reconstruction of a compacted conversation: when a Summary item has
// FirstKeptEntry set, the session's GetMessages must surface the summary
// followed by the kept tail. This is what makes the compactor's
// FirstKeptEntry actually work in the next LLM turn.
func TestSessionGetMessages_WithFirstKeptEntry(t *testing.T) {
	items := []session.Item{
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleUser, Content: "m1"},
		}),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "m2"},
		}),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleUser, Content: "m3"},
		}),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "m4"},
		}),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleUser, Content: "m5"},
		}),
	}

	// Add summary that says "first kept entry is index 3" (m4).
	// So we expect: [system...] + [summary] + [m4, m5]
	items = append(items, session.Item{
		Summary:        "This is a summary of m1-m3",
		FirstKeptEntry: 3, // index of m4 in the Messages slice
	})

	sess := session.New(session.WithMessages(items))
	a := agent.New("test", "test instruction")

	messages := sess.GetMessages(a)

	var conversationMessages []chat.Message
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleSystem {
			conversationMessages = append(conversationMessages, msg)
		}
	}

	require.Len(t, conversationMessages, 3, "expected summary + 2 kept messages")
	assert.Contains(t, conversationMessages[0].Content, "Session Summary:")
	assert.Equal(t, "m4", conversationMessages[1].Content)
	assert.Equal(t, "m5", conversationMessages[2].Content)
}

// TestSessionGetMessages_SummaryWithoutFirstKeptEntry pins backward
// compatibility: a summary with no FirstKeptEntry must still work, with
// the conversation continuing from messages that follow the summary item.
func TestSessionGetMessages_SummaryWithoutFirstKeptEntry(t *testing.T) {
	items := []session.Item{
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleUser, Content: "m1"},
		}),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "m2"},
		}),
		{Summary: "This is a summary"},
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleUser, Content: "m3"},
		}),
	}

	sess := session.New(session.WithMessages(items))
	a := agent.New("test", "test instruction")

	messages := sess.GetMessages(a)

	var conversationMessages []chat.Message
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleSystem {
			conversationMessages = append(conversationMessages, msg)
		}
	}

	require.Len(t, conversationMessages, 2)
	assert.Contains(t, conversationMessages[0].Content, "Session Summary:")
	assert.Equal(t, "m3", conversationMessages[1].Content)
}

// TestDoCompactBeforeHookDeniesSkipsCompaction verifies that a
// before_compaction hook returning exit code 2 (deny) prevents any
// compaction work: no SessionCompactionEvent, no Summary item appended
// to the session, and no model call.
func TestDoCompactBeforeHookDeniesSkipsCompaction(t *testing.T) {
	denyingHooks := &latest.HooksConfig{
		BeforeCompaction: []latest.HookDefinition{
			{Type: "command", Command: "echo 'denied for safety' >&2; exit 2", Timeout: 5},
		},
	}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "test",
		agent.WithModel(prov),
		agent.WithHooks(denyingHooks),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))
	originalLen := len(sess.Messages)

	events := make(chan Event, 32)
	rt.compactWithReason(t.Context(), sess, "", compactionReasonManual, NewChannelSink(events))
	close(events)

	var sawCompactionEvent, sawSummaryEvent bool
	for ev := range events {
		switch ev.(type) {
		case *SessionCompactionEvent:
			sawCompactionEvent = true
		case *SessionSummaryEvent:
			sawSummaryEvent = true
		}
	}

	assert.False(t, sawCompactionEvent,
		"a denied before_compaction must not emit SessionCompaction events")
	assert.False(t, sawSummaryEvent,
		"a denied before_compaction must not emit a summary event")
	assert.Len(t, sess.Messages, originalLen,
		"a denied before_compaction must leave the session unmodified")
}

// TestDoCompactBeforeHookSuppliesSummary verifies that a
// before_compaction hook returning HookSpecificOutput.Summary causes
// the runtime to apply that summary verbatim and to skip the LLM-based
// summarization (no new model call).
func TestDoCompactBeforeHookSuppliesSummary(t *testing.T) {
	const customSummary = "custom hook-supplied summary"
	jsonOutput := `{"hook_specific_output":{"hook_event_name":"before_compaction","summary":"` + customSummary + `"}}`

	hookCfg := &latest.HooksConfig{
		BeforeCompaction: []latest.HookDefinition{
			{Type: "command", Command: "echo '" + jsonOutput + "'", Timeout: 5},
		},
	}

	// The provider must NOT be called — if it is, we'll consume from
	// the (empty) mockStream and the test will catch it.
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "test",
		agent.WithModel(prov),
		agent.WithHooks(hookCfg),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))

	events := make(chan Event, 32)
	rt.compactWithReason(t.Context(), sess, "", compactionReasonManual, NewChannelSink(events))
	close(events)

	var summaryEvent *SessionSummaryEvent
	var compactionStartCount, compactionDoneCount int
	for ev := range events {
		switch e := ev.(type) {
		case *SessionCompactionEvent:
			switch e.Status {
			case "started":
				compactionStartCount++
			case "completed":
				compactionDoneCount++
			}
		case *SessionSummaryEvent:
			summaryEvent = e
		}
	}

	require.NotNil(t, summaryEvent, "expected a SessionSummary event")
	assert.Equal(t, customSummary, summaryEvent.Summary,
		"the runtime must apply the hook-supplied summary verbatim")
	assert.Equal(t, 1, compactionStartCount, "expected exactly one compaction-started event")
	assert.Equal(t, 1, compactionDoneCount, "expected exactly one compaction-completed event")

	last := sess.Messages[len(sess.Messages)-1]
	assert.Equal(t, customSummary, last.Summary,
		"the session must record the hook-supplied summary as its last item")
	assert.InDelta(t, 0.0, last.Cost, 0.0001,
		"hook-supplied summaries cost nothing — no LLM was called")
}

// TestDoCompactAfterHookFires verifies that after_compaction fires
// when a summary was applied (LLM-path or hook-path), and that the
// hook receives the produced summary text together with the
// *pre-compaction* token counts (so observability handlers can
// express "compacted from X to Y").
func TestDoCompactAfterHookFires(t *testing.T) {
	dir := t.TempDir()
	logFile := dir + "/after.log"

	const customSummary = "summary from the before hook"
	beforeJSON := `{"hook_specific_output":{"hook_event_name":"before_compaction","summary":"` + customSummary + `"}}`

	hookCfg := &latest.HooksConfig{
		BeforeCompaction: []latest.HookDefinition{
			{Type: "command", Command: "echo '" + beforeJSON + "'", Timeout: 5},
		},
		AfterCompaction: []latest.HookDefinition{
			// Capture summary plus pre-compaction tokens; if the runtime
			// regresses to passing post-compaction values we'll see
			// input_tokens == EstimateMessageTokens(summary) instead of
			// the pre-compaction 1234.
			{Type: "command", Command: "cat | jq -r '\"\\(.summary)|\\(.input_tokens)|\\(.output_tokens)\"' > " + logFile, Timeout: 5},
		},
	}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "test",
		agent.WithModel(prov),
		agent.WithHooks(hookCfg),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
	}))
	// Seed pre-compaction token counts so we can verify the hook
	// receives them rather than the post-compaction values (which
	// would be approximately EstimateMessageTokens(summary) and 0).
	sess.InputTokens = 1234
	sess.OutputTokens = 567

	events := make(chan Event, 32)
	rt.compactWithReason(t.Context(), sess, "", compactionReasonThreshold, NewChannelSink(events))
	close(events)
	for range events {
	}

	logged, readErr := os.ReadFile(logFile)
	require.NoError(t, readErr, "after_compaction hook must have run and produced the log file")
	assert.Equal(t, customSummary+"|1234|567\n", string(logged),
		"after_compaction must receive the produced summary and the *pre-compaction* token counts")
}

// TestDoCompactNoHooksMatchesPriorBehavior is a regression guard: with
// no compaction-related hooks configured, compactWithReason must still
// emit the same SessionCompaction started/completed pair that all
// existing UIs depend on.
func TestDoCompactNoHooksMatchesPriorBehavior(t *testing.T) {
	summaryStream := newStreamBuilder().
		AddContent("summary").
		AddStopWithUsage(1, 1).
		Build()

	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))

	events := make(chan Event, 32)
	rt.compactWithReason(t.Context(), sess, "", compactionReasonManual, NewChannelSink(events))
	close(events)

	var startCount, doneCount int
	var summaryEvent *SessionSummaryEvent
	for ev := range events {
		switch e := ev.(type) {
		case *SessionCompactionEvent:
			switch e.Status {
			case "started":
				startCount++
			case "completed":
				doneCount++
			}
		case *SessionSummaryEvent:
			summaryEvent = e
		}
	}

	assert.Equal(t, 1, startCount, "expected exactly one started event")
	assert.Equal(t, 1, doneCount, "expected exactly one completed event")
	require.NotNil(t, summaryEvent, "expected a SessionSummary event from the LLM path")
	assert.Equal(t, "summary", summaryEvent.Summary)
}
