package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// drainEvents reads non-blocking from the events channel and returns
// everything currently buffered, so a test can collect what
// enforceMaxIterations / handleStreamError emitted before exiting.
func drainEvents(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func newTestRuntime(t *testing.T) (*LocalRuntime, *agent.Agent) {
	t.Helper()
	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt, root
}

// --- enforceMaxIterations tests ---

func TestEnforceMaxIterations_BelowLimit_Continues(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 8)

	newMax, decision := rt.enforceMaxIterations(t.Context(), sess, a, 3, 10, NewChannelSink(events))

	assert.Equal(t, iterationContinue, decision)
	assert.Equal(t, 10, newMax, "limit must be unchanged when below the cap")
	assert.Empty(t, drainEvents(events), "no events should fire below the cap")
}

func TestEnforceMaxIterations_DisabledLimit_Continues(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 8)

	// runtimeMaxIterations <= 0 disables the cap entirely.
	newMax, decision := rt.enforceMaxIterations(t.Context(), sess, a, 1_000_000, 0, NewChannelSink(events))

	assert.Equal(t, iterationContinue, decision)
	assert.Equal(t, 0, newMax)
	assert.Empty(t, drainEvents(events))
}

func TestEnforceMaxIterations_NonInteractive_AutoStops(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	sess.NonInteractive = true
	events := make(chan Event, 8)

	_, decision := rt.enforceMaxIterations(t.Context(), sess, a, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationStop, decision, "non-interactive must auto-stop when at the cap")

	got := drainEvents(events)
	require.NotEmpty(t, got)

	// First event is MaxIterationsReached; an assistant message-added
	// event must follow with the configured stop text.
	_, ok := got[0].(*MaxIterationsReachedEvent)
	require.True(t, ok, "first event must be MaxIterationsReachedEvent, got %T", got[0])

	var saw bool
	for _, ev := range got {
		if added, ok := ev.(*MessageAddedEvent); ok {
			saw = true
			assert.Contains(t, added.Message.Message.Content, "max_iterations limit (10)")
		}
	}
	assert.True(t, saw, "expected a MessageAddedEvent with the stop text")
}

func TestEnforceMaxIterations_Interactive_ApproveExtends(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 8)

	// Pre-load an approval onto the resume channel so enforceMaxIterations
	// returns immediately instead of blocking on user input.
	go func() { rt.resumeChan <- ResumeApprove() }()

	newMax, decision := rt.enforceMaxIterations(t.Context(), sess, a, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationContinue, decision)
	assert.Equal(t, 20, newMax, "approve must extend by 10 iterations beyond the current iteration")
}

func TestEnforceMaxIterations_Interactive_RejectStops(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 8)

	go func() { rt.resumeChan <- ResumeReject("no thanks") }()

	_, decision := rt.enforceMaxIterations(t.Context(), sess, a, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationStop, decision)

	got := drainEvents(events)
	var sawStopMessage bool
	for _, ev := range got {
		if added, ok := ev.(*MessageAddedEvent); ok {
			sawStopMessage = true
			assert.Contains(t, added.Message.Message.Content, "max_iterations limit (10)")
		}
	}
	assert.True(t, sawStopMessage, "rejection should emit the stop assistant message")
}

func TestEnforceMaxIterations_ContextCancelled_Stops(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 8)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, decision := rt.enforceMaxIterations(ctx, sess, a, 10, 10, NewChannelSink(events))
	assert.Equal(t, iterationStop, decision)
}

// --- handleStreamError tests ---

func TestHandleStreamError_ContextCanceled_Fatal(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 8)
	span := noop.NewTracerProvider().Tracer("t").Start
	_, sp := span(t.Context(), "x")

	overflowCount := 0
	outcome := rt.handleStreamError(t.Context(), sess, a, context.Canceled, 1000, &overflowCount, sp, NewChannelSink(events))

	assert.Equal(t, streamErrorFatal, outcome)
	assert.Empty(t, drainEvents(events), "context cancel should not emit any events")
	assert.Equal(t, 0, overflowCount, "context cancel should not bump overflow counter")
}

// errModelStore is a ModelStore that always returns an error from GetModel,
// keeping doCompact from progressing past its model-lookup guard. It lets
// handleStreamError tests exercise the retry branch without Summarize
// trying to drive a real compaction agent.
type errModelStore struct{ ModelStore }

func (errModelStore) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return nil, errors.New("no model")
}

func TestHandleStreamError_OverflowWithCompactionEnabled_Retries(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm, WithModelStore(errModelStore{}))
	require.NoError(t, err)

	sess := session.New()
	events := make(chan Event, 16)
	_, sp := noop.NewTracerProvider().Tracer("t").Start(t.Context(), "x")

	overflow := modelerrors.NewContextOverflowError(errors.New("too long"))
	overflowCount := 0

	outcome := rt.handleStreamError(t.Context(), sess, root, overflow, 1000, &overflowCount, sp, NewChannelSink(events))

	assert.Equal(t, streamErrorRetry, outcome)
	assert.Equal(t, 1, overflowCount, "overflow counter should bump on retry")

	got := drainEvents(events)
	var sawWarn bool
	for _, ev := range got {
		if w, ok := ev.(*WarningEvent); ok {
			sawWarn = true
			assert.Contains(t, w.Message, "exceeded the model's context window")
		}
	}
	assert.True(t, sawWarn, "expected a Warning event when retrying after overflow")
}

func TestHandleStreamError_OverflowExhausted_Fatal(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 16)
	_, sp := noop.NewTracerProvider().Tracer("t").Start(t.Context(), "x")

	overflow := modelerrors.NewContextOverflowError(errors.New("too long"))
	// Counter is already at the cap, so we must NOT retry again.
	overflowCount := rt.maxOverflowCompactions

	outcome := rt.handleStreamError(t.Context(), sess, a, overflow, 1000, &overflowCount, sp, NewChannelSink(events))

	assert.Equal(t, streamErrorFatal, outcome)
	assert.Equal(t, rt.maxOverflowCompactions, overflowCount, "exhausted path must not bump counter further")

	got := drainEvents(events)
	var sawError bool
	for _, ev := range got {
		if _, ok := ev.(*ErrorEvent); ok {
			sawError = true
		}
	}
	assert.True(t, sawError, "expected an ErrorEvent when overflow recovery is exhausted")
}

func TestHandleStreamError_OverflowWithCompactionDisabled_Fatal(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	sess := session.New()
	events := make(chan Event, 16)
	_, sp := noop.NewTracerProvider().Tracer("t").Start(t.Context(), "x")

	overflow := modelerrors.NewContextOverflowError(errors.New("too long"))
	overflowCount := 0

	outcome := rt.handleStreamError(t.Context(), sess, root, overflow, 1000, &overflowCount, sp, NewChannelSink(events))

	assert.Equal(t, streamErrorFatal, outcome, "overflow must be fatal when session compaction is disabled")
	assert.Equal(t, 0, overflowCount)
}

func TestHandleStreamError_GenericError_FatalAndEmitsError(t *testing.T) {
	t.Parallel()

	rt, a := newTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 16)
	_, sp := noop.NewTracerProvider().Tracer("t").Start(t.Context(), "x")

	overflowCount := 0
	outcome := rt.handleStreamError(t.Context(), sess, a, errors.New("boom"), 1000, &overflowCount, sp, NewChannelSink(events))

	assert.Equal(t, streamErrorFatal, outcome)

	got := drainEvents(events)
	var sawError bool
	for _, ev := range got {
		if _, ok := ev.(*ErrorEvent); ok {
			sawError = true
		}
	}
	assert.True(t, sawError, "generic error should emit ErrorEvent")
}
