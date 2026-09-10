package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

// TestSessionGetMessages_WithFirstKeptEntry covers the session-side
// reconstruction of a compacted conversation: when a Summary item has
// FirstKeptEntry set, the session's GetMessages must surface the summary
// followed by the kept tail. This is what makes the compactor's
// FirstKeptEntry actually work in the next LLM turn.
func TestSessionGetMessages_WithFirstKeptEntry(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("skipping test relying on POSIX shell commands on Windows")
	}

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

	rt, err := NewLocalRuntime(t.Context(), tm,
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
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("skipping test relying on POSIX shell commands on Windows")
	}

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

	rt, err := NewLocalRuntime(t.Context(), tm,
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
	assert.InDelta(t, 0.0, summaryEvent.Cost, 0.0001,
		"the summary event carries the same zero cost")
}

// TestDoCompactAfterHookFires verifies that after_compaction fires
// when a summary was applied (LLM-path or hook-path), and that the
// hook receives the produced summary text together with the
// *pre-compaction* token counts (so observability handlers can
// express "compacted from X to Y").
func TestDoCompactAfterHookFires(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("skipping test relying on POSIX shell commands on Windows")
	}

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

	rt, err := NewLocalRuntime(t.Context(), tm,
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

type failingCompactionStore struct {
	session.Store

	err error
}

func (s failingCompactionStore) PersistCompaction(context.Context, *session.Session, int64, int64, session.Item) error {
	return s.err
}

func TestDoCompactInMemoryStoreAppendsSummaryOnce(t *testing.T) {
	store := session.NewInMemorySessionStore()
	summaryStream := newStreamBuilder().AddContent("one summary").AddStopWithUsage(1, 1).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithSessionStore(store),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithID("compact-memory"), session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessage("hi")),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))
	require.NoError(t, store.AddSession(t.Context(), sess))

	events := make(chan Event, 32)
	rt.Summarize(t.Context(), sess, "", NewChannelSink(events))
	close(events)
	for range events {
	}

	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	var summaries int
	for _, item := range reloaded.Messages {
		if item.Summary != "" {
			summaries++
		}
	}
	assert.Equal(t, 1, summaries)
	assert.Same(t, sess, reloaded, "aliasing store must mutate the live session exactly once")
}

func TestDoCompactPersistenceFailureReportsFailedWithoutApplying(t *testing.T) {
	base := session.NewInMemorySessionStore()
	persistErr := errors.New("disk full")
	store := failingCompactionStore{Store: base, err: persistErr}
	summaryStream := newStreamBuilder().AddContent("lost summary").AddStopWithUsage(1, 1).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithSessionStore(store),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)

	sess := session.New(session.WithID("compact-fails"), session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessage("hi")),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))
	require.NoError(t, base.AddSession(t.Context(), sess))
	before := len(sess.Messages)

	events := make(chan Event, 32)
	rt.Summarize(t.Context(), sess, "", NewChannelSink(events))
	close(events)
	var outcome string
	var sawError, sawSummary bool
	for ev := range events {
		switch e := ev.(type) {
		case *SessionCompactionEvent:
			if e.Status == "completed" {
				outcome = e.Outcome
			}
		case *ErrorEvent:
			sawError = true
		case *SessionSummaryEvent:
			sawSummary = true
		}
	}
	assert.Equal(t, CompactionOutcomeFailed, outcome)
	assert.True(t, sawError)
	assert.False(t, sawSummary, "an unpersisted summary must not be announced as successful")
	assert.Len(t, sess.Messages, before, "failed persistence must leave live continuation unchanged")
}

func TestDoCompactPersistsSummaryForSQLiteReload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	summaryStream := newStreamBuilder().AddContent("persisted summary").AddStopWithUsage(12, 3).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithSessionStore(store),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := session.New(
		session.WithID("compact-reload"),
		session.WithMessages([]session.Item{
			session.NewMessageItem(session.UserMessage("old question")),
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "old answer"}}),
		}),
	)
	require.NoError(t, store.AddSession(t.Context(), sess))

	events := make(chan Event, 32)
	rt.Summarize(t.Context(), sess, "", NewChannelSink(events))
	close(events)
	for range events {
	}
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	reloaded, err := reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, reloaded.Messages, 3)
	assert.Equal(t, "persisted summary", reloaded.Messages[2].Summary)

	messages := reloaded.GetMessages(root)
	var continuation strings.Builder
	for _, msg := range messages {
		continuation.WriteString(msg.Content)
	}
	assert.Contains(t, continuation.String(), "persisted summary")
	assert.NotContains(t, continuation.String(), "old question", "reload must continue from the compacted view")
}

func TestDoCompactObservedRunDoesNotDuplicateSummary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	summaryStream := newStreamBuilder().AddContent("one summary").AddStopWithUsage(1, 1).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithSessionStore(store),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithID("compact-dedup"), session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessage("hi")),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))
	require.NoError(t, store.AddSession(t.Context(), sess))

	inner := make(chan Event, 32)
	observed := rt.observe(t.Context(), sess, inner)
	rt.compactWithReason(t.Context(), sess, "", compactionReasonManual, NewChannelSink(inner))
	close(inner)
	for range observed {
	}

	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var summaries int
	for _, item := range reloaded.Messages {
		if item.Summary != "" {
			summaries++
		}
	}
	assert.Equal(t, 1, summaries, "intrinsic persistence and the observer must not both append the summary")
}

// TestDoCompactNoHooksMatchesPriorBehavior is a regression guard: with
// no compaction-related hooks configured, compactWithReason must still
// emit the same SessionCompaction started/completed pair that all
// existing UIs depend on.
func TestDoCompactNoHooksMatchesPriorBehavior(t *testing.T) {
	t.Parallel()

	summaryStream := newStreamBuilder().
		AddContent("summary").
		AddStopWithUsage(1, 1).
		Build()

	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
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

// TestDoCompactSummaryEventCarriesCost pins the cost plumbing of the LLM
// compaction path: the summarization stream's billed cost
// (compactor.Result.Cost) must land on the applied summary item and on the
// emitted SessionSummaryEvent — the value the persistence observer stores.
// The compaction sub-runtime inherits the parent's model store, which is
// what prices the summary call here.
func TestDoCompactSummaryEventCarriesCost(t *testing.T) {
	t.Parallel()

	summaryStream := newStreamBuilder().AddContent("the summary").AddStopWithUsage(100, 50).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}),
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
	for ev := range events {
		if e, ok := ev.(*SessionSummaryEvent); ok {
			summaryEvent = e
		}
	}
	require.NotNil(t, summaryEvent, "expected a SessionSummary event from the LLM path")

	// The summary stream billed 100 input × $10/M + 50 output × $20/M = $0.002.
	last := sess.Messages[len(sess.Messages)-1]
	require.Equal(t, "the summary", last.Summary)
	assert.InDelta(t, 0.002, last.Cost, 1e-9,
		"the summary item records the summarization stream's cost")
	assert.InDelta(t, last.Cost, summaryEvent.Cost, 1e-9,
		"the emitted event must carry the same cost that was applied to the session")
}
