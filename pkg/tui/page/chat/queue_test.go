package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newTestChatPage creates a minimal chatPage for testing queue behavior.
// Note: This only initializes fields needed for queue testing. The nil app
// means busy sends fall back to the local queue instead of steering.
// processMessage cannot be called without full initialization.
func newTestChatPage(t *testing.T) *chatPage {
	t.Helper()
	sessionState := &service.SessionState{}

	return &chatPage{
		sidebar:      sidebar.New(animation.NewRuntime(), t.Context(), sessionState),
		sessionState: sessionState,
		working:      true, // Start busy so messages get queued
	}
}

type immediateCommandMsg struct{ arg string }

type queueTestRuntime struct{}

func (queueTestRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (queueTestRuntime) CurrentAgentName(context.Context) string                 { return "root" }
func (queueTestRuntime) SetCurrentAgent(context.Context, string) error           { return nil }
func (queueTestRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (queueTestRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus      { return nil }
func (queueTestRuntime) RestartToolset(context.Context, string) error            { return nil }
func (queueTestRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}
func (queueTestRuntime) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (queueTestRuntime) ResetStartupInfo()                                {}
func (queueTestRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (queueTestRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (queueTestRuntime) Resume(context.Context, runtime.ResumeRequest) {}
func (queueTestRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any, ...string) error {
	return nil
}
func (queueTestRuntime) SessionStore() session.Store { return nil }
func (queueTestRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {
}
func (queueTestRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }
func (queueTestRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return nil
}

func (queueTestRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (queueTestRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (queueTestRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (queueTestRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (queueTestRuntime) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }
func (queueTestRuntime) Steer(context.Context, runtime.QueuedMessage) error     { return nil }
func (queueTestRuntime) FollowUp(context.Context, runtime.QueuedMessage) error  { return nil }
func (queueTestRuntime) SetAgentModel(context.Context, string, string) error    { return nil }
func (queueTestRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (queueTestRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}
func (queueTestRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }
func (queueTestRuntime) SupportsModelSwitching() bool                          { return false }
func (queueTestRuntime) OnToolsChanged(func(runtime.Event))                    {}
func (queueTestRuntime) OnBackgroundEvent(func(runtime.Event))                 {}
func (queueTestRuntime) OnElicitationRequest(func(runtime.Event))              {}
func (queueTestRuntime) QueueStatus() runtime.QueueStatus                      { return runtime.QueueStatus{} }
func (queueTestRuntime) TogglePause(context.Context) (bool, error)             { return false, nil }
func (queueTestRuntime) Close() error                                          { return nil }

var _ runtime.Runtime = queueTestRuntime{}

// skillDispatchRuntime records how a skill slash command is ultimately
// executed so tests can assert it reaches the right resolution path:
// fork skills via RunSkillFork, inline skills via the regular RunStream path.
type skillDispatchRuntime struct {
	queueTestRuntime

	skillset *skillstool.ToolSet

	mu            sync.Mutex
	forkCalls     []skillstool.RunSkillArgs
	runStreamRuns int
	lastRun       string
}

func (r *skillDispatchRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return r.skillset
}

func (r *skillDispatchRuntime) RunSkillFork(_ context.Context, sess *session.Session, args skillstool.RunSkillArgs, sink runtime.EventSink) (*tools.ToolCallResult, error) {
	r.mu.Lock()
	r.forkCalls = append(r.forkCalls, args)
	r.mu.Unlock()

	sink.Emit(runtime.StreamStopped(sess.ID, "", ""))
	return tools.ResultSuccess("done"), nil
}

func (r *skillDispatchRuntime) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	r.mu.Lock()
	r.runStreamRuns++
	if items := sess.GetAllMessages(); len(items) > 0 {
		r.lastRun = items[len(items)-1].Message.Content
	}
	r.mu.Unlock()

	ch := make(chan runtime.Event, 1)
	ch <- runtime.StreamStopped(sess.ID, "", "")
	close(ch)
	return ch
}

func (r *skillDispatchRuntime) forkCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.forkCalls)
}

func (r *skillDispatchRuntime) lastForkArgs() skillstool.RunSkillArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.forkCalls) == 0 {
		return skillstool.RunSkillArgs{}
	}
	return r.forkCalls[len(r.forkCalls)-1]
}

func (r *skillDispatchRuntime) runStreamCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runStreamRuns
}

func (r *skillDispatchRuntime) lastRunMessage() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastRun
}

func TestQueueFlow_BusyAgent_ImmediateSlashCommandBypassesQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	p.commandParser = commands.NewParser(commands.Category{
		Name: "Test",
		Commands: []commands.Item{
			{
				SlashCommand: "/now",
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					return func() tea.Msg { return immediateCommandMsg{arg: arg} }
				},
			},
		},
	})

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/now please"})

	require.Empty(t, p.messageQueue)
	require.NotNil(t, cmd)
	assert.Equal(t, immediateCommandMsg{arg: "please"}, cmd())
}

func TestQueueFlow_BusyAgent_BangCommandBypassesQueue(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "!"})

	require.Empty(t, p.messageQueue)
	require.NotNil(t, cmd)
	select {
	case <-ctx.Done():
		t.Fatal("bang command should not cancel the running stream")
	default:
	}
}

// TestQueueFlow_SkillCommand_DispatchesOnceWithoutLooping guards against a
// regression (introduced by reordering the checks in handleSendMsg for the
// --session-read-only feature) where fork-mode skill slash commands looped
// forever. Such commands resolve by re-dispatching themselves as
// SendMsg{BypassQueue: true}; if handleSendMsg re-parses that bypass message
// it matches the same command again, re-runs Execute, and never reaches
// processMessage (so the skill never actually runs).
//
// The test reproduces the real two-hop flow: the user-typed "/myskill" is
// parsed into an Execute that emits a BypassQueue SendMsg, which is then fed
// back through handleSendMsg. Execute must run exactly once.
func TestQueueFlow_SkillCommand_DispatchesOnceWithoutLooping(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	calls := 0
	p.commandParser = commands.NewParser(commands.Category{
		Name: "Skills",
		Commands: []commands.Item{
			{
				SlashCommand: "/myskill",
				Immediate:    true,
				Execute: func(string) tea.Cmd {
					calls++
					return core.CmdHandler(messages.SendMsg{Content: "/myskill", BypassQueue: true})
				},
			},
		},
	})

	// Hop 1: the user types "/myskill". It is parsed and Execute emits a
	// BypassQueue re-dispatch of the same command.
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/myskill"})
	require.NotNil(t, cmd)
	require.Equal(t, 1, calls)

	redispatch, ok := cmd().(messages.SendMsg)
	require.True(t, ok, "skill Execute should re-dispatch a SendMsg")
	require.True(t, redispatch.BypassQueue, "re-dispatch must bypass the queue")

	// Hop 2: the BypassQueue message must be processed directly, not re-parsed
	// back into the command (which would invoke Execute again and loop).
	_, cmd = p.handleSendMsg(redispatch)
	require.NotNil(t, cmd)
	assert.Empty(t, p.messageQueue)
	assert.Equal(t, 1, calls, "BypassQueue message must not be re-parsed into the command again")
}

func TestQueueFlow_BusyAgent_QueuesMessage(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	// newTestChatPage already sets working=true

	// Send first explicitly queued message while busy
	msg1 := messages.SendMsg{Content: "first message", Queue: true}
	_, cmd := p.handleSendMsg(msg1)

	// Should be queued
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "first message", p.messageQueue[0].content)
	// Command should be a notification (not processMessage)
	assert.NotNil(t, cmd)

	// Send second message while still busy
	msg2 := messages.SendMsg{Content: "second message", Queue: true}
	_, _ = p.handleSendMsg(msg2)

	require.Len(t, p.messageQueue, 2)
	assert.Equal(t, "first message", p.messageQueue[0].content)
	assert.Equal(t, "second message", p.messageQueue[1].content)

	// Send third message
	msg3 := messages.SendMsg{Content: "third message", Queue: true}
	_, _ = p.handleSendMsg(msg3)

	require.Len(t, p.messageQueue, 3)
}

func TestQueueFlow_QueueFull_RejectsMessage(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	// newTestChatPage sets working=true

	// Fill the queue to max
	for i := range maxQueuedMessages {
		msg := messages.SendMsg{Content: "message", Queue: true}
		_, _ = p.handleSendMsg(msg)
		assert.Len(t, p.messageQueue, i+1)
	}

	require.Len(t, p.messageQueue, maxQueuedMessages)

	// Try to add one more - should be rejected
	msg := messages.SendMsg{Content: "overflow message", Queue: true}
	_, cmd := p.handleSendMsg(msg)

	// Queue size should not change
	assert.Len(t, p.messageQueue, maxQueuedMessages)
	// Should return a warning notification command
	assert.NotNil(t, cmd)
}

func TestQueueFlow_PopFromQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)

	// Queue some messages
	p.handleSendMsg(messages.SendMsg{Content: "first", Queue: true})
	p.handleSendMsg(messages.SendMsg{Content: "second", Queue: true})
	p.handleSendMsg(messages.SendMsg{Content: "third", Queue: true})

	require.Len(t, p.messageQueue, 3)

	// Manually pop messages (simulating what processNextQueuedMessage does internally)
	// Pop first
	popped := p.messageQueue[0]
	p.messageQueue = p.messageQueue[1:]
	p.syncQueueToSidebar()

	assert.Equal(t, "first", popped.content)
	require.Len(t, p.messageQueue, 2)
	assert.Equal(t, "second", p.messageQueue[0].content)
	assert.Equal(t, "third", p.messageQueue[1].content)

	// Pop second
	popped = p.messageQueue[0]
	p.messageQueue = p.messageQueue[1:]

	assert.Equal(t, "second", popped.content)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "third", p.messageQueue[0].content)

	// Pop last
	popped = p.messageQueue[0]
	p.messageQueue = p.messageQueue[1:]

	assert.Equal(t, "third", popped.content)
	assert.Empty(t, p.messageQueue)
}

func TestQueueFlow_ClearQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	// newTestChatPage sets working=true

	// Queue some messages
	p.handleSendMsg(messages.SendMsg{Content: "first", Queue: true})
	p.handleSendMsg(messages.SendMsg{Content: "second", Queue: true})
	p.handleSendMsg(messages.SendMsg{Content: "third", Queue: true})

	require.Len(t, p.messageQueue, 3)

	// Clear the queue
	_, cmd := p.handleClearQueue()

	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd) // Success notification

	// Clearing empty queue
	_, cmd = p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd) // Info notification
}

func TestReadOnly_RejectsMessages(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := app.New(t.Context(), queueTestRuntime{}, sess, app.WithReadOnly())
	require.True(t, a.IsReadOnly())

	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "hello"})

	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd)
}

func TestReadOnly_AllowsSlashCommands(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := app.New(t.Context(), queueTestRuntime{}, sess, app.WithReadOnly())
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.commandParser = commands.NewParser(commands.Category{
		Name: "Test",
		Commands: []commands.Item{
			{
				SlashCommand: "/now",
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					return func() tea.Msg { return immediateCommandMsg{arg: arg} }
				},
			},
		},
	})

	// Slash commands should still work in read-only mode
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/now please"})

	require.NotNil(t, cmd)
	assert.Equal(t, immediateCommandMsg{arg: "please"}, cmd())
}

// skillCommandParser mirrors the real skill palette item built in
// BuildCommandCategories: an Immediate slash command whose Execute re-emits
// the same slash text as a BypassQueue SendMsg.
func skillCommandParser(name string) *commands.Parser {
	return commands.NewParser(commands.Category{
		Name: "Skills",
		Commands: []commands.Item{
			{
				SlashCommand: "/" + name,
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					input := "/" + name
					if arg = strings.TrimSpace(arg); arg != "" {
						input += " " + arg
					}
					return core.CmdHandler(messages.SendMsg{Content: input, BypassQueue: true})
				},
			},
		},
	})
}

// dispatchTypedSkill reproduces typing a skill slash command in the editor.
// Hop 1: handleSendMsg parses it and Execute re-emits a BypassQueue SendMsg.
// Hop 2: that message is fed back through handleSendMsg, which must route it
// to processMessage instead of re-parsing it into another SendMsg (the loop).
func dispatchTypedSkill(t *testing.T, p *chatPage, content string) {
	t.Helper()

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: content})
	require.NotNil(t, cmd)

	redispatch, ok := cmd().(messages.SendMsg)
	require.True(t, ok, "typed skill should resolve to a re-dispatched SendMsg")
	require.True(t, redispatch.BypassQueue, "re-dispatch must bypass the queue")

	_, cmd = p.handleSendMsg(redispatch)
	require.NotNil(t, cmd)
	if _, loops := cmd().(messages.SendMsg); loops {
		t.Fatal("skill SendMsg was re-emitted: dispatch is looping")
	}
}

// TestHandleSendMsg_ForkSkillRunsViaFork proves a fork-mode skill slash
// command reaches RunSkillFork with the parsed name/task and never loops.
func TestHandleSendMsg_ForkSkillRunsViaFork(t *testing.T) {
	t.Parallel()

	skillSet := skillstool.New([]skills.Skill{{
		Name:          "services",
		Description:   "List services",
		Context:       "fork",
		InlineContent: "# Services\nList repository services.",
	}}, t.TempDir())
	rt := &skillDispatchRuntime{skillset: skillSet}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.commandParser = skillCommandParser("services")

	dispatchTypedSkill(t, p, "/services please")

	require.Eventually(t, func() bool { return rt.forkCallCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "services", rt.lastForkArgs().Name)
	assert.Equal(t, "please", rt.lastForkArgs().Task)
	assert.Zero(t, rt.runStreamCallCount(), "fork skills must not use the inline RunStream path")
	assert.Zero(t, sess.MessageCount(), "fork skill dispatch must not append an inline user message")
}

// TestHandleSendMsg_InlineSkillRunsViaResolveInput proves an inline skill
// slash command is expanded and sent through the regular RunStream path
// (not the fork path) and never loops.
func TestHandleSendMsg_InlineSkillRunsViaResolveInput(t *testing.T) {
	t.Parallel()

	skillSet := skillstool.New([]skills.Skill{{
		Name:          "services",
		Description:   "List services",
		InlineContent: "# Services\nList repository services.",
	}}, t.TempDir())
	rt := &skillDispatchRuntime{skillset: skillSet}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.commandParser = skillCommandParser("services")

	dispatchTypedSkill(t, p, "/services please")

	require.Eventually(t, func() bool { return sess.MessageCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Zero(t, rt.forkCallCount(), "inline skills must not use fork dispatch")
	require.Equal(t, 1, rt.runStreamCallCount())
	assert.Contains(t, rt.lastRunMessage(), `<skill name="services">`)
	assert.Contains(t, rt.lastRunMessage(), "List repository services.")
	assert.Contains(t, rt.lastRunMessage(), "User's request: please")
}

// TestReadOnly_RejectsBypassQueueCommands ensures resolved skill/agent
// commands (re-dispatched as SendMsg{BypassQueue: true}) are still blocked in
// read-only mode. BypassQueue only skips re-parsing to avoid the command loop;
// it must not let model-bound work slip past the read-only guard.
func TestReadOnly_RejectsBypassQueueCommands(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := app.New(t.Context(), queueTestRuntime{}, sess, app.WithReadOnly())
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/myskill", BypassQueue: true})

	require.NotNil(t, cmd)
	assert.Empty(t, p.messageQueue)
	assert.False(t, p.working, "read-only must not start processing a BypassQueue message")
	assert.Nil(t, p.msgCancel, "read-only must not start a stream for a BypassQueue message")
}

// steerRecordingRuntime records Steer calls so tests can assert that busy
// sends are injected into the ongoing stream instead of queued locally.
type steerRecordingRuntime struct {
	queueTestRuntime

	mu        sync.Mutex
	steerErr  error
	steers    []runtime.QueuedMessage
	followUps []runtime.QueuedMessage
}

func (r *steerRecordingRuntime) Steer(_ context.Context, msg runtime.QueuedMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steerErr != nil {
		return r.steerErr
	}
	r.steers = append(r.steers, msg)
	return nil
}

func (r *steerRecordingRuntime) FollowUp(_ context.Context, msg runtime.QueuedMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.followUps = append(r.followUps, msg)
	return nil
}

func (r *steerRecordingRuntime) CancelSteer(_ context.Context, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, msg := range r.steers {
		if msg.ID != id {
			continue
		}
		r.steers = append(r.steers[:i], r.steers[i+1:]...)
		return true
	}
	return false
}

func (r *steerRecordingRuntime) CancelFollowUp(_ context.Context, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, msg := range r.followUps {
		if msg.ID != id {
			continue
		}
		r.followUps = append(r.followUps[:i], r.followUps[i+1:]...)
		return true
	}
	return false
}

func (r *steerRecordingRuntime) followedUp() []runtime.QueuedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.QueuedMessage(nil), r.followUps...)
}

func (r *steerRecordingRuntime) steered() []runtime.QueuedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.QueuedMessage(nil), r.steers...)
}

func TestFollowUpFlow_BusyAgent_UsesRuntimeFollowUpQueue(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "do this next", FollowUp: true})

	require.NotNil(t, cmd)
	assert.IsType(t, followUpSentMsg{}, cmd())
	assert.Empty(t, p.messageQueue)
	assert.Empty(t, rt.steered())
	if assert.Len(t, rt.followedUp(), 1) {
		assert.Equal(t, "do this next", rt.followedUp()[0].Content)
	}
}

// TestSteerFlow_BusyAgent_SteersMessage pins the issue #3547 contract: a
// plain send while the agent is working attaches to the ongoing stream via
// the runtime steer queue instead of waiting in the local TUI queue.
func TestSteerFlow_BusyAgent_SteersMessage(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "extra context"})

	require.NotNil(t, cmd)
	assert.Empty(t, p.messageQueue, "steered messages must not enter the local queue")

	// Run the async steer command and verify the runtime received the message.
	result := cmd()
	assert.IsType(t, steerSentMsg{}, result)
	steered := rt.steered()
	require.Len(t, steered, 1)
	assert.Equal(t, "extra context", steered[0].Content)
}

func TestOptionUpRestoresAllPendingMessageTypes(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, steerCmd := p.handleSendMsg(messages.SendMsg{Content: "first steer"})
	steerResult := steerCmd()
	_, _ = p.Update(steerResult)
	_, followCmd := p.handleSendMsg(messages.SendMsg{Content: "then follow up", FollowUp: true})
	followResult := followCmd()
	_, _ = p.Update(followResult)
	p.enqueueMessage(messages.SendMsg{Content: "locally queued", Queue: true})

	_, cmd := p.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})

	assert.Empty(t, rt.steered())
	assert.Empty(t, rt.followedUp())
	assert.Empty(t, p.pendingMessages)
	assert.Empty(t, p.messageQueue)
	msgs := runTimerCmd(t, cmd)
	assert.Contains(t, msgs, messages.RestorePendingMessagesMsg{Content: "first steer\nthen follow up\nlocally queued"})
	assert.Contains(t, msgs, messages.RequestFocusMsg{Target: messages.PanelEditor})
}

func TestConsumedSteerIsNotRestored(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, steerCmd := p.handleSendMsg(messages.SendMsg{Content: "already consumed"})
	_, _ = p.Update(steerCmd())
	require.Len(t, p.pendingMessages, 1)
	p.consumePendingMessage("already consumed")

	_, cmd := p.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})

	assert.Empty(t, p.pendingMessages)
	assert.Contains(t, runTimerCmd(t, cmd), notification.ShowMsg{Text: "No pending messages", Type: notification.TypeInfo})
}

// TestSteerFlow_ExplicitQueue_SkipsSteering verifies the internal Queue
// flag (used by fallbacks such as a rejected steer): a Queue-flagged send
// while busy goes to the local queue and never touches the runtime steer
// queue.
func TestSteerFlow_ExplicitQueue_SkipsSteering(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "for later", Queue: true})

	require.NotNil(t, cmd)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "for later", p.messageQueue[0].content)
	assert.Empty(t, rt.steered())
}

// TestSteerFlow_QueueSendMode_QueuesPlainSend verifies the /settings switch:
// with the send mode set to queue, a plain send while busy goes to the local
// queue instead of the runtime steer queue.
func TestSteerFlow_QueueSendMode_QueuesPlainSend(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess), WithSendMode(messages.SendModeQueue)).(*chatPage)
	p.working = true

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "for later"})

	require.NotNil(t, cmd)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "for later", p.messageQueue[0].content)
	assert.Empty(t, rt.steered())

	// Switching back to steer mid-session restores steering.
	p.SetSendMode(messages.SendModeSteer)
	_, cmd = p.handleSendMsg(messages.SendMsg{Content: "right away"})
	require.NotNil(t, cmd)
	assert.IsType(t, steerSentMsg{}, cmd())
	require.Len(t, rt.steered(), 1)
	assert.Equal(t, "right away", rt.steered()[0].Content)
}

// TestSteerFlow_SteerRejected_FallsBackToQueue verifies that a steer
// rejection (e.g. full runtime queue) re-routes the message to the local
// queue while the agent is still working, so it is never dropped.
func TestSteerFlow_SteerRejected_FallsBackToQueue(t *testing.T) {
	t.Parallel()

	rt := &steerRecordingRuntime{steerErr: errors.New("steer queue full")}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "extra context"})
	require.NotNil(t, cmd)

	failed, ok := cmd().(steerFailedMsg)
	require.True(t, ok, "a rejected steer must come back as steerFailedMsg")
	assert.Equal(t, "extra context", failed.original.Content)

	_, cmd = p.Update(failed)
	require.NotNil(t, cmd)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "extra context", p.messageQueue[0].content)
}

// TestSteerFlow_ForkSkillWhileBusy_Queues verifies fork-mode skill commands
// are never steered: they spawn their own sub-session stream, which cannot
// attach to the running one, so they wait in the local queue instead.
func TestSteerFlow_ForkSkillWhileBusy_Queues(t *testing.T) {
	t.Parallel()

	skillSet := skillstool.New([]skills.Skill{{
		Name:          "services",
		Description:   "List services",
		Context:       "fork",
		InlineContent: "# Services\nList repository services.",
	}}, t.TempDir())
	rt := &skillDispatchRuntime{skillset: skillSet}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	p.working = true

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/services please"})

	require.NotNil(t, cmd)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "/services please", p.messageQueue[0].content)
	assert.Zero(t, rt.forkCallCount(), "fork skill must not run while another stream is active")
}

// TestSteerFlow_NoApp_FallsBackToQueue documents the defensive fallback:
// without an app there is no runtime to steer, so busy sends queue locally.
func TestSteerFlow_NoApp_FallsBackToQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "hello"})

	require.NotNil(t, cmd)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "hello", p.messageQueue[0].content)
}
