package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestCompactionControllerWaitsAtBlockingThreshold(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	prov := &mockCompactor{id: "anthropic/claude-test", opts: map[string]any{nativeCompactionOpt: true}, result: nativeResult(), onCall: func() { close(entered); <-release }}
	root := agent.New("root", "help", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := nativeTestSession()
	sess.SetUsage(75_000, 0)
	c := newCompactionController(rt, sess, EventSinkFunc(func(Event) {}), &userconfig.Settings{BackgroundCompaction: true})
	defer c.Close(t.Context())
	c.BeforeTurn(t.Context(), root, 100_000)
	require.NotNil(t, c.job)
	<-entered
	sess.SetUsage(95_000, 0)
	before := sess.OwnMessageCount()
	sess.AddMessage(session.UserMessage("new tail"))
	go func() { close(release) }()
	c.AfterMessages(t.Context(), root, 100_000, before)
	assert.Nil(t, c.job)
	assert.Equal(t, "native summary", sess.LastSummary())
	assert.Equal(t, 1, prov.callCount(), "reuse the background summary rather than compacting twice")
}

func TestCompactionControllerRejectsConcurrentCompaction(t *testing.T) {
	t.Parallel()
	for _, background := range []bool{false, true} {
		for _, reason := range []string{compactionReasonManual, compactionReasonOverflow} {
			t.Run(fmt.Sprintf("background=%t/%s", background, reason), func(t *testing.T) {
				t.Parallel()
				entered, release := make(chan struct{}), make(chan struct{})
				prov := &mockCompactor{id: "anthropic/claude-test", opts: map[string]any{nativeCompactionOpt: true}, result: nativeResult()}
				prov.onCall = func() { close(entered); <-release }
				root := agent.New("root", "help", agent.WithModel(prov))
				rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, rt.Close()) })
				sess := nativeTestSession()
				sess.SetUsage(75_000, 0)
				sink := EventSinkFunc(func(Event) {})
				c := newCompactionController(rt, sess, sink, &userconfig.Settings{BackgroundCompaction: background})
				defer c.Close(t.Context())
				c.BeforeTurn(t.Context(), root, 100_000)
				finished := make(chan struct{})
				if background {
					require.NotNil(t, c.job)
				} else {
					go func() {
						defer close(finished)
						assert.NoError(t, rt.compactWithReason(t.Context(), sess, "", compactionReasonManual, sink))
					}()
				}
				<-entered
				assert.Nil(t, rt.startBackgroundCompaction(t.Context(), sess, root), "background requests also respect the guard")
				if reason == compactionReasonManual {
					require.ErrorIs(t, rt.compactWithReason(t.Context(), sess, "custom instructions", reason, sink), ErrCompactionInProgress)
					entry := rt.registerLiveSession(sess)
					require.ErrorIs(t, rt.CompactLiveSession(t.Context(), sess.ID, "", sink), ErrCompactionInProgress)
					rt.finishLiveSession(t.Context(), entry)
				} else {
					_, span := noop.NewTracerProvider().Tracer("test").Start(t.Context(), "overflow")
					defer span.End()
					count := 0
					var got []Event
					outcome := rt.handleStreamError(t.Context(), sess, root, modelerrors.NewContextOverflowError(errors.New("too long")), 100_000, &count, span, EventSinkFunc(func(e Event) { got = append(got, e) }))
					assert.Equal(t, streamErrorFatal, outcome)
					assert.Contains(t, got, ErrorForSession(sess.ID, ErrCompactionInProgress.Error()))
				}
				close(release)
				if background {
					c.collect(t.Context(), true)
				} else {
					<-finished
				}
				assert.Nil(t, c.job)
				assert.Equal(t, "native summary", sess.LastSummary())
				assert.Equal(t, 1, prov.callCount(), "must not send a second compaction")
				prov.onCall = nil
				assert.NoError(t, rt.compactWithReason(t.Context(), sess, "", reason, sink), "guard is released after completion")
			})
		}
	}
}

func TestCompactionControllerCloseCancelsAndJoins(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	job := &backgroundCompaction{cancel: cancel, done: done}
	go func() { <-ctx.Done(); close(done) }()
	c := &compactionController{job: job, events: EventSinkFunc(func(Event) {})}
	c.Close(t.Context())
	assert.Nil(t, c.job)
	select {
	case <-done:
	default:
		t.Fatal("worker must finish before Close returns")
	}
	c.Close(t.Context())
}

func TestCompactionControllerSkipsDisabledAndPendingTools(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"disabled setting", "disabled agent", "disabled runtime", "pending tools"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			prov := &mockCompactor{id: "anthropic/claude-test", opts: map[string]any{nativeCompactionOpt: true}, result: nativeResult()}
			root := agent.New("root", "help", agent.WithModel(prov), agent.WithSessionCompaction(mode != "disabled agent"))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(mode != "disabled runtime"), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			sess := nativeTestSession()
			sess.SetUsage(75_000, 0)
			if mode == "pending tools" {
				sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "pending", Function: tools.FunctionCall{Name: "test"}}}}))
			}
			c := newCompactionController(rt, sess, EventSinkFunc(func(Event) {}), &userconfig.Settings{BackgroundCompaction: mode != "disabled setting"})
			defer c.Close(t.Context())
			c.BeforeTurn(t.Context(), root, 100_000)
			assert.Nil(t, c.job)
			assert.Zero(t, prov.callCount())
		})
	}
}
