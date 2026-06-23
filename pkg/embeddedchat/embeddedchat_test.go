package embeddedchat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dagentcfg "github.com/docker/docker-agent/pkg/config"
	dagentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewLoadsAgentAndWelcomeMessage(t *testing.T) {
	cfg := []byte(`agents:
  root:
    description: Test agent
    instruction: Be helpful.
    welcome_message: Hello from embedded chat.
    harness:
      type: claude-code
`)

	s, err := New(t.Context(), Config{AgentSource: dagentcfg.NewBytesSource("agent.yaml", cfg)})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "Hello from embedded chat.", s.WelcomeMessage())
	require.NotNil(t, s.Runtime())
	require.NotNil(t, s.Conversation())
}

func TestNewRequiresAgentSource(t *testing.T) {
	s, err := New(t.Context(), Config{})
	require.Nil(t, s)
	require.ErrorIs(t, err, ErrAgentSourceRequired)
}

type fakeRuntime struct {
	events chan dagentruntime.Event

	runCtxs      []context.Context
	resumes      []dagentruntime.ResumeRequest
	elicitations []tools.ElicitationAction
	closed       bool
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{events: make(chan dagentruntime.Event, 8)}
}

func (f *fakeRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan dagentruntime.Event {
	f.runCtxs = append(f.runCtxs, ctx)
	return f.events
}

func (f *fakeRuntime) Resume(_ context.Context, req dagentruntime.ResumeRequest) {
	f.resumes = append(f.resumes, req)
}

func (f *fakeRuntime) ResumeElicitation(_ context.Context, action tools.ElicitationAction, _ map[string]any) error {
	f.elicitations = append(f.elicitations, action)
	return nil
}

func (f *fakeRuntime) Close() error {
	f.closed = true
	return nil
}

func newTestSession(rt *fakeRuntime) *Session {
	return &Session{rt: rt, session: session.New()}
}

func TestTranslateRuntimeEvent(t *testing.T) {
	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "tool"}}
	def := tools.Tool{Name: "tool"}

	event, ok := TranslateRuntimeEvent(dagentruntime.AgentChoice("agent", "session", "hello"))
	require.True(t, ok)
	require.Equal(t, "hello", event.Text)

	event, ok = TranslateRuntimeEvent(dagentruntime.ToolCall(call, def, "agent"))
	require.True(t, ok)
	require.Equal(t, call, event.Tool.Call)
	require.Equal(t, def, event.Tool.Def)
	require.False(t, event.Tool.Finished)

	event, ok = TranslateRuntimeEvent(dagentruntime.ToolCallResponse("call-1", def, tools.ResultError("boom"), "boom", "agent"))
	require.True(t, ok)
	require.Equal(t, "call-1", event.Tool.Call.ID)
	require.True(t, event.Tool.Finished)
	require.True(t, event.Tool.IsError)

	_, ok = TranslateRuntimeEvent(dagentruntime.AgentChoice("agent", "session", ""))
	require.False(t, ok)
}

func TestSessionSendStreamsEventsAndDone(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	require.Len(t, s.session.Messages, 1)

	rt.events <- dagentruntime.AgentChoice("agent", s.session.ID, "hello")
	require.Equal(t, "hello", receiveEvent(t, out).Text)

	close(rt.events)
	event := receiveEvent(t, out)
	require.True(t, event.Done)
	assertClosed(t, out)
}

func TestSessionSendSurfacesConfirmationAndConfirmResumesRuntime(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "use tool")
	require.NoError(t, err)

	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "write_file"}}
	def := tools.Tool{Name: "write_file"}
	rt.events <- dagentruntime.ToolCallConfirmation(call, def, "agent")

	event := receiveEvent(t, out)
	require.NotNil(t, event.Tool)
	require.True(t, event.Tool.NeedsConfirmation)
	require.Equal(t, call, event.Tool.Call)

	require.NoError(t, s.Confirm(t.Context(), dagentruntime.ResumeApproveTool("write_file(*)")))
	require.Len(t, rt.resumes, 1)
	require.Equal(t, dagentruntime.ResumeTypeApproveTool, rt.resumes[0].Type)
	require.Equal(t, "write_file(*)", rt.resumes[0].ToolName)
}

func TestSessionSendHandlesRuntimeErrorWithoutDone(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	rt.events <- dagentruntime.Error("boom")

	event := receiveEvent(t, out)
	require.EqualError(t, event.Err, "boom")

	rt.events <- dagentruntime.AgentChoice("agent", s.session.ID, "ignored")
	close(rt.events)
	assertClosed(t, out)
}

func TestSessionSendDeclinesElicitationAndRejectsMaxIterations(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	rt.events <- dagentruntime.ElicitationRequest("authorize", "url", nil, "https://example.com", "id", nil, "agent")
	rt.events <- dagentruntime.MaxIterationsReached(3)
	close(rt.events)

	require.True(t, receiveEvent(t, out).Done)
	require.Equal(t, []tools.ElicitationAction{"decline"}, rt.elicitations)
	require.Len(t, rt.resumes, 1)
	require.Equal(t, dagentruntime.ResumeTypeReject, rt.resumes[0].Type)
}

func TestSessionSendRejectsConcurrentRun(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	ctx, cancel := context.WithCancel(t.Context())
	_, err := s.Send(ctx, "first")
	require.NoError(t, err)

	out, err := s.Send(t.Context(), "second")
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrRunActive)

	cancel()
	close(rt.events)
}

func TestSessionRejectsOperationsAfterClose(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	require.NoError(t, s.Close())
	out, err := s.Send(t.Context(), "hi")
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrClosed)
	require.ErrorIs(t, s.Restart(), ErrClosed)
	require.ErrorIs(t, s.Confirm(t.Context(), dagentruntime.ResumeApprove()), ErrClosed)

	close(rt.events)
}

func TestSessionCloseCancelsActiveRunAndClosesRuntime(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	_, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	require.Len(t, rt.runCtxs, 1)

	require.NoError(t, s.Close())
	require.True(t, rt.closed)
	require.Eventually(t, func() bool {
		return errors.Is(rt.runCtxs[0].Err(), context.Canceled)
	}, time.Second, time.Millisecond)

	close(rt.events)
}

func TestSessionRestartKeepsRunActiveUntilRuntimeStops(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "first")
	require.NoError(t, err)
	require.NoError(t, s.Restart())

	next, err := s.Send(t.Context(), "second")
	require.Nil(t, next)
	require.ErrorIs(t, err, ErrRunActive)

	close(rt.events)
	assertClosed(t, out)

	next, err = s.Send(t.Context(), "second")
	require.NoError(t, err)
	require.True(t, receiveEvent(t, next).Done)
}

func TestSessionRestartCancelsRunAndReplacesConversation(t *testing.T) {
	rt := newFakeRuntime()
	s := newTestSession(rt)

	_, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	oldSession := s.session

	require.NoError(t, s.Restart())
	require.NotSame(t, oldSession, s.session)
	require.Empty(t, s.session.Messages)
	require.Eventually(t, func() bool {
		return errors.Is(rt.runCtxs[0].Err(), context.Canceled)
	}, time.Second, time.Millisecond)

	close(rt.events)
}

func receiveEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		require.True(t, ok)
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for embedded chat event")
		return Event{}
	}
}

func assertClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-ch:
		require.False(t, ok, "unexpected event: %#v", event)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for embedded chat stream to close")
	}
}
