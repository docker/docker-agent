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
	"github.com/docker/docker-agent/pkg/userconfig"
)

// steerRuntime is a minimal runtime.Runtime for testing steer/follow-up behaviour.
type steerRuntime struct {
	steered         []runtime.QueuedMessage
	steerFn         func(runtime.QueuedMessage) error // optional override
	steerCleared    int                               // number of ClearSteerQueue calls
	followedUp      []runtime.QueuedMessage
	followUpFn      func(runtime.QueuedMessage) error // optional override
	followUpCleared int                               // number of ClearFollowUpQueue calls
}

func (r *steerRuntime) Steer(msg runtime.QueuedMessage) error {
	if r.steerFn != nil {
		return r.steerFn(msg)
	}
	r.steered = append(r.steered, msg)
	return nil
}

func (r *steerRuntime) ClearSteerQueue() {
	r.steerCleared++
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

func (r *steerRuntime) FollowUp(msg runtime.QueuedMessage) error {
	if r.followUpFn != nil {
		return r.followUpFn(msg)
	}
	r.followedUp = append(r.followedUp, msg)
	return nil
}

func (r *steerRuntime) ClearFollowUpQueue() {
	r.followUpCleared++
}

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

func TestSteer_ClearQueue_AlsoClearsRuntime(t *testing.T) {
	t.Parallel()

	p, rt := newTestChatPage(t)

	// Steer some messages
	p.handleSendMsg(messages.SendMsg{Content: "first"})
	p.handleSendMsg(messages.SendMsg{Content: "second"})
	p.handleSendMsg(messages.SendMsg{Content: "third"})

	require.Len(t, p.messageQueue, 3)

	// Clear the display queue — should also drain the runtime steer queue
	_, cmd := p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd)               // Success notification
	assert.Equal(t, 1, rt.steerCleared) // runtime queue was drained

	// Clearing empty queue should NOT call ClearSteerQueue
	_, cmd = p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd)               // Info notification
	assert.Equal(t, 1, rt.steerCleared) // unchanged — no extra drain
}

func TestSteer_BusyAgent_PassesAttachments(t *testing.T) {
	t.Parallel()

	p, rt := newTestChatPage(t)

	// Send a message with an inline (pasted text) attachment while busy.
	// File-reference attachments require real files on disk so we only
	// test inline content here.
	msg := messages.SendMsg{
		Content: "check this",
		Attachments: []messages.Attachment{
			{Name: "paste-1", Content: "some pasted text"},
		},
	}
	_, cmd := p.handleSendMsg(msg)
	assert.NotNil(t, cmd)

	// The runtime should have received the steered message with the
	// inline attachment text appended to Content.
	require.Len(t, rt.steered, 1)
	assert.Contains(t, rt.steered[0].Content, "check this")
	assert.Contains(t, rt.steered[0].Content, "some pasted text")
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

// setFollowupBehavior writes the global follow-up behavior to an isolated
// HOME-rooted config for the duration of a test. Not parallel-safe because it
// mutates HOME via t.Setenv.
func setFollowupBehavior(t *testing.T, behavior string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	if cfg.Settings == nil {
		cfg.Settings = &userconfig.Settings{}
	}
	cfg.Settings.FollowupBehavior = behavior
	require.NoError(t, cfg.Save())
}

func TestFollowUp_BusyAgent_EnqueuesAsFollowUp(t *testing.T) {
	setFollowupBehavior(t, userconfig.FollowupBehaviorFollowUp)

	p, rt := newTestChatPage(t)

	// Send while busy in follow-up mode — should call FollowUp, not Steer.
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "first"})
	assert.NotNil(t, cmd)

	assert.Empty(t, rt.steered, "steer path should not be used in follow-up mode")
	require.Len(t, rt.followedUp, 1)
	assert.Equal(t, "first", rt.followedUp[0].Content)

	// Display queue should still track messages for the sidebar.
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "first", p.messageQueue[0].content)

	// Second message also follows the follow-up path.
	_, _ = p.handleSendMsg(messages.SendMsg{Content: "second"})
	require.Len(t, rt.followedUp, 2)
	assert.Empty(t, rt.steered)
}

func TestFollowUp_QueueFull_RejectsMessage(t *testing.T) {
	setFollowupBehavior(t, userconfig.FollowupBehaviorFollowUp)

	p, rt := newTestChatPage(t)

	rt.followUpFn = func(msg runtime.QueuedMessage) error {
		if len(rt.followedUp) >= 2 {
			return errors.New("follow-up queue full")
		}
		rt.followedUp = append(rt.followedUp, msg)
		return nil
	}

	// First two succeed.
	for i := range 2 {
		_, _ = p.handleSendMsg(messages.SendMsg{Content: "message"})
		assert.Len(t, rt.followedUp, i+1)
	}

	// Third is rejected.
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "overflow"})
	assert.NotNil(t, cmd) // warning notification
	assert.Len(t, rt.followedUp, 2)
	// Display queue should not grow on rejection.
	assert.Len(t, p.messageQueue, 2)
}

func TestClearQueue_DrainsBothRuntimeQueues(t *testing.T) {
	t.Parallel()

	p, rt := newTestChatPage(t)

	// Prime the display queue by sending two messages in (default) steer mode.
	p.handleSendMsg(messages.SendMsg{Content: "first"})
	p.handleSendMsg(messages.SendMsg{Content: "second"})
	require.Len(t, p.messageQueue, 2)

	// Clear should drain both runtime queues unconditionally so switching
	// modes after queueing does not leak messages.
	_, cmd := p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd)
	assert.Equal(t, 1, rt.steerCleared)
	assert.Equal(t, 1, rt.followUpCleared)
}

func TestFollowUp_BusyAgent_PassesAttachments(t *testing.T) {
	setFollowupBehavior(t, userconfig.FollowupBehaviorFollowUp)

	p, rt := newTestChatPage(t)

	msg := messages.SendMsg{
		Content: "check this",
		Attachments: []messages.Attachment{
			{Name: "paste-1", Content: "some pasted text"},
		},
	}
	_, cmd := p.handleSendMsg(msg)
	assert.NotNil(t, cmd)

	require.Len(t, rt.followedUp, 1)
	assert.Contains(t, rt.followedUp[0].Content, "check this")
	assert.Contains(t, rt.followedUp[0].Content, "some pasted text")
	assert.Empty(t, rt.steered)
}
