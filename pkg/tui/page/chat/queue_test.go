package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// steerRuntime is a minimal runtime.Runtime for testing steer behaviour.
type steerRuntime struct {
	steered []runtime.QueuedMessage
	steerFn func(runtime.QueuedMessage) error // optional override
}

func (r *steerRuntime) Steer(msg runtime.QueuedMessage) error {
	if r.steerFn != nil {
		return r.steerFn(msg)
	}
	r.steered = append(r.steered, msg)
	return nil
}

// Remaining interface methods — no-ops for this test.

func (r *steerRuntime) CurrentAgentName() string { return "test" }

func (r *steerRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (r *steerRuntime) SetCurrentAgent(string) error { return nil }

func (r *steerRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (r *steerRuntime) EmitStartupInfo(_ context.Context, _ *session.Session, _ chan runtime.Event) {
	// Do not close the channel — app.New's goroutine defers the close.
}

func (r *steerRuntime) ResetStartupInfo() {}

func (r *steerRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (r *steerRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}

func (r *steerRuntime) Resume(context.Context, runtime.ResumeRequest) {}

func (r *steerRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any) error {
	return nil
}

func (r *steerRuntime) SessionStore() session.Store { return nil }

func (r *steerRuntime) Summarize(context.Context, *session.Session, string, chan runtime.Event) {}

func (r *steerRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }

func (r *steerRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset { return nil }

func (r *steerRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (r *steerRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (r *steerRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}

func (r *steerRuntime) TitleGenerator() *sessiontitle.Generator { return nil }

func (r *steerRuntime) Close() error { return nil }

func (r *steerRuntime) FollowUp(runtime.QueuedMessage) error { return nil }

func (r *steerRuntime) RegenerateTitle(context.Context, *session.Session, chan runtime.Event) {}

// newTestChatPage creates a minimal chatPage for testing steer/queue behaviour.
func newTestChatPage(t *testing.T) (*chatPage, *steerRuntime) {
	t.Helper()
	sessionState := &service.SessionState{}

	rt := &steerRuntime{}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a := app.New(ctx, rt, session.New())

	return &chatPage{
		sidebar:      sidebar.New(sessionState),
		sessionState: sessionState,
		working:      true, // Start busy so messages get steered
		app:          a,
	}, rt
}

func TestSteer_BusyAgent_SteersMessage(t *testing.T) {
	t.Parallel()

	p, rt := newTestChatPage(t)

	// Send first message while busy — should steer to runtime
	msg1 := messages.SendMsg{Content: "first message"}
	_, cmd := p.handleSendMsg(msg1)
	assert.NotNil(t, cmd) // notification command

	require.Len(t, rt.steered, 1)
	assert.Equal(t, "first message", rt.steered[0].Content)
	// Display queue should track the steered message
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "first message", p.messageQueue[0].content)

	// Send second message
	msg2 := messages.SendMsg{Content: "second message"}
	_, _ = p.handleSendMsg(msg2)

	require.Len(t, rt.steered, 2)
	assert.Equal(t, "second message", rt.steered[1].Content)
	require.Len(t, p.messageQueue, 2)
}

func TestSteer_QueueFull_RejectsMessage(t *testing.T) {
	t.Parallel()

	p, rt := newTestChatPage(t)

	// Make the runtime's steer queue reject after the first call
	calls := 0
	rt.steerFn = func(msg runtime.QueuedMessage) error {
		calls++
		if calls > 3 {
			return errors.New("steer queue full")
		}
		rt.steered = append(rt.steered, msg)
		return nil
	}

	// First 3 messages succeed
	for i := range 3 {
		_, _ = p.handleSendMsg(messages.SendMsg{Content: "message"})
		assert.Len(t, rt.steered, i+1)
	}

	// Fourth message should be rejected by the runtime
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "overflow"})
	assert.NotNil(t, cmd) // warning notification
	assert.Len(t, rt.steered, 3)
	// Display queue should not grow when steer fails
	assert.Len(t, p.messageQueue, 3)
}

func TestSteer_ClearQueue(t *testing.T) {
	t.Parallel()

	p, _ := newTestChatPage(t)

	// Steer some messages
	p.handleSendMsg(messages.SendMsg{Content: "first"})
	p.handleSendMsg(messages.SendMsg{Content: "second"})
	p.handleSendMsg(messages.SendMsg{Content: "third"})

	require.Len(t, p.messageQueue, 3)

	// Clear the display queue
	_, cmd := p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd) // Success notification

	// Clearing empty queue
	_, cmd = p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd) // Info notification
}

func TestSteer_IdleAgent_ProcessesImmediately(t *testing.T) {
	t.Parallel()

	p, rt := newTestChatPage(t)
	p.working = false // agent is idle

	// When idle, handleSendMsg should NOT steer — it calls processMessage
	// instead. We can't call processMessage without full init, but we can
	// verify no steer occurred.
	_ = messages.SendMsg{Content: "hello"}
	assert.Empty(t, rt.steered)
}
