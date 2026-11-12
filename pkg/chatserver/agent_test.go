package chatserver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type stubRunner struct {
	events           []runtime.Event
	onRun            func(context.Context, *session.Session)
	resumes          []runtime.ResumeRequest
	elicitationIDs   []string
	elicitationReply tools.ElicitationAction
	elicitationData  map[string]any
	elicitationErr   error
}

func (r *stubRunner) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	if r.onRun != nil {
		r.onRun(ctx, sess)
	}
	events := make(chan runtime.Event, len(r.events))
	for _, event := range r.events {
		events <- event
	}
	close(events)
	return events
}

func (r *stubRunner) Resume(_ context.Context, req runtime.ResumeRequest) {
	r.resumes = append(r.resumes, req)
}

func (r *stubRunner) ResumeElicitation(_ context.Context, action tools.ElicitationAction, content map[string]any, ids ...string) error {
	r.elicitationReply = action
	r.elicitationData = content
	r.elicitationIDs = append(r.elicitationIDs, ids...)
	return r.elicitationErr
}

func TestRunAgentLoopForwardsEvents(t *testing.T) {
	t.Parallel()
	first := tools.ToolCall{ID: "first", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"test.txt"}`}}
	second := tools.ToolCall{ID: "second", Type: "function", Function: tools.FunctionCall{Name: "think", Arguments: `{}`}}
	runner := &stubRunner{events: []runtime.Event{
		runtime.AgentChoice("root", "session", "hello"),
		runtime.ToolCall(first, tools.Tool{}, "root"),
		runtime.AgentChoice("root", "session", " world"),
		runtime.ToolCall(second, tools.Tool{}, "root"),
	}}
	sess := session.New()
	runner.onRun = func(ctx context.Context, got *session.Session) {
		assert.Equal(t, t.Context(), ctx)
		assert.Same(t, sess, got)
	}
	var content []string
	var calls []ToolCallReference
	err := runAgentLoop(t.Context(), runner, sess, agentEmit{
		onContent:  func(delta string) { content = append(content, delta) },
		onToolCall: func(call ToolCallReference) { calls = append(calls, call) },
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"hello", " world"}, content)
	assert.Equal(t, []ToolCallReference{
		{Index: 0, ID: "first", Type: "function", Function: ToolCallFunction{Name: "read_file", Arguments: first.Function.Arguments}},
		{Index: 1, ID: "second", Type: "function", Function: ToolCallFunction{Name: "think", Arguments: second.Function.Arguments}},
	}, calls)
}

func TestRunAgentLoopRespondsToInteraction(t *testing.T) {
	t.Parallel()
	runner := &stubRunner{events: []runtime.Event{
		&runtime.ToolCallConfirmationEvent{},
		&runtime.ElicitationRequestEvent{ElicitationID: "request-id", ServerElicitationID: "server-id"},
		&runtime.MaxIterationsReachedEvent{},
	}, elicitationErr: errors.New("response failed")}

	require.NoError(t, runAgentLoop(t.Context(), runner, session.New(), agentEmit{}))
	assert.Equal(t, []runtime.ResumeRequest{runtime.ResumeApprove(), runtime.ResumeReject("")}, runner.resumes)
	assert.Equal(t, tools.ElicitationActionDecline, runner.elicitationReply)
	assert.Nil(t, runner.elicitationData)
	assert.Equal(t, []string{"request-id"}, runner.elicitationIDs)
}

func TestRunAgentLoopDrainsAfterErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := &stubRunner{events: []runtime.Event{
		&runtime.ErrorEvent{Error: "first failure"},
		runtime.AgentChoice("root", "session", "cancel"),
		&runtime.ErrorEvent{Error: "second failure"},
		runtime.AgentChoice("root", "session", "drained"),
	}}
	var content []string
	err := runAgentLoop(ctx, runner, session.New(), agentEmit{onContent: func(delta string) {
		content = append(content, delta)
		cancel()
	}})
	require.EqualError(t, err, "first failure\nsecond failure\ncontext canceled")
	assert.Equal(t, []string{"cancel", "drained"}, content)
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}
