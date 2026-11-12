package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime/compactor"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestBackgroundCompactionOverlapsAndPreservesTail(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	prov := &mockCompactor{id: "anthropic/claude-test", opts: map[string]any{nativeCompactionOpt: true}, result: nativeResult(), onCall: func() { close(entered); <-release }}
	root := agent.New("root", "help", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := nativeTestSession()
	sess.ParentID = "parent-session"
	sess.NonInteractive = true
	sess.ExcludedTools = []string{"excluded"}
	sess.AllowedTools = []string{"allowed"}
	sess.SetUsage(60_000, 0)
	ctx := t.Context()
	var events []Event
	sink := EventSinkFunc(func(e Event) { events = append(events, e) })
	c := newCompactionController(rt, sess, sink, &userconfig.Settings{BackgroundCompaction: true, BackgroundCompactionThreshold: 60})
	defer c.Close(ctx)
	c.BeforeTurn(ctx, root, 100_000)
	assert.Nil(t, c.job, "threshold is strictly greater than the configured percentage")
	sess.SetUsage(60_001, 0)
	c.BeforeTurn(ctx, root, 100_000)
	require.NotNil(t, c.job)
	b := c.job
	assert.Equal(t, sess.ParentID, b.snapshot.ParentID)
	assert.True(t, b.snapshot.NonInteractive)
	assert.Equal(t, sess.ExcludedTools, b.snapshot.ExcludedTools)
	assert.Equal(t, sess.AllowedTools, b.snapshot.AllowedTools)
	assert.NotSame(t, sess.Messages[0].Message, b.snapshot.Messages[0].Message)
	sess.ExcludedTools[0] = "changed"
	assert.Equal(t, "excluded", b.snapshot.ExcludedTools[0])
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("compactor did not start")
	}
	// Completion cannot mutate the live session while the next model/tool turn runs.
	sess.AddMessage(session.UserMessage("message added while compacting"))
	c.BeforeTurn(ctx, root, 100_000)
	assert.Same(t, b, c.job, "an in-flight job is reused")
	assert.Empty(t, sess.LastSummary())
	close(release)
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("compactor did not finish")
	}
	assert.Empty(t, sess.LastSummary())
	c.BeforeTurn(ctx, root, 100_000)
	assert.Nil(t, c.job)
	assert.Equal(t, "native summary", sess.LastSummary())
	messages := sess.GetMessages(root)
	require.GreaterOrEqual(t, len(messages), 2)
	assert.Equal(t, "message added while compacting", messages[len(messages)-1].Content)
	input, _ := sess.Usage()
	assert.Positive(t, input)
	var summaries int
	for _, e := range events {
		if _, ok := e.(*SessionSummaryEvent); ok {
			summaries++
		}
	}
	assert.Equal(t, 1, summaries)
	c.BeforeTurn(ctx, root, 100_000)
	assert.Nil(t, c.job)
}

func TestBackgroundCompactionRejectsStaleResults(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"compacted", "edited", "cancelled", "persistence failure"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			root := agent.New("root", "help", agent.WithModel(&mockProvider{id: "test/model", stream: &mockStream{}}))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			sess := nativeTestSession()
			snapshot := sess.Clone()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			close(done)
			_, jobCancel := context.WithCancel(ctx)
			b := &backgroundCompaction{done: done, cancel: jobCancel, snapshot: snapshot, agent: root, result: &compactor.Result{Summary: "stale", FirstKeptEntry: 3}}
			switch change {
			case "compacted":
				sess.ApplyCompaction(1, 0, session.Item{Summary: "newer"})
			case "edited":
				sess.Messages[0].Message.Message.Content = "edited"
			case "cancelled":
				cancel()
			case "persistence failure":
				rt.sessionStore = failingCompactionStore{Store: rt.sessionStore, err: errors.New("disk full")}
			}
			c := newCompactionController(rt, sess, EventSinkFunc(func(Event) {}), nil)
			c.job = b
			c.BeforeTurn(ctx, root, 100_000)
			assert.NotEqual(t, "stale", sess.LastSummary())
			assert.Nil(t, c.job)
		})
	}
}

func TestBackgroundCompactionDisabledByDefault(t *testing.T) {
	t.Parallel()
	assert.False(t, (&userconfig.Settings{}).BackgroundCompaction)
	assert.Equal(t, 70, (*userconfig.Settings)(nil).GetBackgroundCompactionThreshold())
	for _, value := range []int{-1, 0, 100} {
		assert.Equal(t, 70, (&userconfig.Settings{BackgroundCompactionThreshold: value}).GetBackgroundCompactionThreshold())
	}
	assert.Equal(t, 55, (&userconfig.Settings{BackgroundCompactionThreshold: 55}).GetBackgroundCompactionThreshold())
}

func TestBackgroundCompactionStopJoinsWorker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	b := &backgroundCompaction{done: done, cancel: cancel}
	go func() { <-ctx.Done(); b.Emit(Warning("cancelled", "root")); close(done) }()
	var events []Event
	b.stop(EventSinkFunc(func(e Event) { events = append(events, e) }))
	assert.Len(t, events, 1)
	assert.Nil(t, b.done)
	b.stop(EventSinkFunc(func(Event) { t.Fatal("already stopped") }))
}

func TestBackgroundCompactionLLMAndPersistence(t *testing.T) {
	t.Parallel()
	summaryStream := newStreamBuilder().AddContent("LLM summary").AddStopWithUsage(100, 20).Build()
	prov := &queueProvider{id: "test/model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "help", agent.WithModel(prov))
	store := openSessionStore(t, t.TempDir()+"/sessions.db")
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionStore(store), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := nativeTestSession()
	require.NoError(t, store.AddSession(t.Context(), sess))
	snapshot := sess.Clone()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	_, jobCancel := context.WithCancel(ctx)
	b := &backgroundCompaction{done: done, cancel: jobCancel, snapshot: snapshot, agent: root}
	go func() {
		defer close(done)
		b.result = rt.generateCompaction(ctx, snapshot, root, "", compactionReasonThreshold, b)
	}()
	<-done
	require.NotNil(t, b.result)
	tail := session.UserMessage("after the snapshot")
	sess.AddMessage(tail)
	_, err = store.AddMessage(t.Context(), sess.ID, tail)
	require.NoError(t, err)
	b.wait(ctx, rt, sess, EventSinkFunc(func(Event) {}))
	assert.Equal(t, "LLM summary", sess.LastSummary())
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "LLM summary", loaded.LastSummary())
	messages := loaded.GetMessages(root)
	assert.Equal(t, "after the snapshot", messages[len(messages)-1].Content)
}
