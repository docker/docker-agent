package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// stubRuntime is a no-op runtime.Runtime for wiring an *app.App into
// appModel handler tests. It deliberately does NOT implement the optional
// live-session capabilities.
type stubRuntime struct{}

func (stubRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (stubRuntime) CurrentAgentName(context.Context) string                 { return "root" }
func (stubRuntime) SetCurrentAgent(context.Context, string) error           { return nil }
func (stubRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (stubRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus      { return nil }
func (stubRuntime) RestartToolset(context.Context, string) error            { return nil }
func (stubRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}
func (stubRuntime) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (stubRuntime) ResetStartupInfo()                                {}
func (stubRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (stubRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (stubRuntime) Resume(context.Context, runtime.ResumeRequest) {}
func (stubRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any, ...string) error {
	return nil
}
func (stubRuntime) SessionStore() session.Store { return nil }
func (stubRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {
}
func (stubRuntime) PermissionsInfo() *runtime.PermissionsInfo      { return nil }
func (stubRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet { return nil }

func (stubRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (stubRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo { return nil }

func (stubRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (stubRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (stubRuntime) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }
func (stubRuntime) Steer(context.Context, runtime.QueuedMessage) error     { return nil }
func (stubRuntime) FollowUp(context.Context, runtime.QueuedMessage) error  { return nil }
func (stubRuntime) SetAgentModel(context.Context, string, string) error    { return nil }
func (stubRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (stubRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}
func (stubRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }
func (stubRuntime) SupportsModelSwitching() bool                          { return false }
func (stubRuntime) OnToolsChanged(func(runtime.Event))                    {}
func (stubRuntime) OnBackgroundEvent(func(runtime.Event))                 {}
func (stubRuntime) OnElicitationRequest(func(runtime.Event))              {}
func (stubRuntime) QueueStatus() runtime.QueueStatus                      { return runtime.QueueStatus{} }
func (stubRuntime) TogglePause(context.Context) (bool, error)             { return false, nil }
func (stubRuntime) Close() error                                          { return nil }

var _ runtime.Runtime = stubRuntime{}

// liveCompactRuntime adds the optional targeted-compaction capability.
type liveCompactRuntime struct {
	stubRuntime

	compacted  []string
	compactErr error
}

func (r *liveCompactRuntime) CompactLiveSession(_ context.Context, sessionID, _ string, _ runtime.EventSink) error {
	if r.compactErr != nil {
		return r.compactErr
	}
	r.compacted = append(r.compacted, sessionID)
	return nil
}

// newCompactTestModel wires a minimal appModel around rt and sess.
func newCompactTestModel(t *testing.T, rt runtime.Runtime, sess *session.Session) (*appModel, *mockChatPage) {
	t.Helper()
	m, _ := newTestModel(t)
	m.application = app.New(t.Context(), rt, sess)
	page := m.activeTab.chatPage.(*mockChatPage)
	return m, page
}

func TestHandleCompactSession_EmptyTargetUsesRootPath(t *testing.T) {
	t.Parallel()

	rt := &liveCompactRuntime{}
	m, page := newCompactTestModel(t, rt, session.New())

	_, _ = m.Update(messages.CompactSessionMsg{AdditionalPrompt: "focus on code"})

	assert.Equal(t, []string{"focus on code"}, page.compactCalls,
		"/compact routes through the chat page's root compaction")
	assert.Empty(t, rt.compacted, "the targeted API must not be used for the root session")
}

func TestHandleCompactSession_RootSessionIDUsesRootPath(t *testing.T) {
	t.Parallel()

	sess := session.New()
	rt := &liveCompactRuntime{}
	m, page := newCompactTestModel(t, rt, sess)

	_, _ = m.Update(messages.CompactSessionMsg{SessionID: sess.ID, AgentName: "root"})

	assert.Len(t, page.compactCalls, 1, "the main /context row routes through the root compaction path")
	assert.Empty(t, rt.compacted)
}

func TestHandleCompactSession_SubSessionUsesTargetedPath(t *testing.T) {
	t.Parallel()

	rt := &liveCompactRuntime{}
	m, page := newCompactTestModel(t, rt, session.New())

	_, cmd := m.Update(messages.CompactSessionMsg{SessionID: "child-1", AgentName: "worker"})

	assert.Empty(t, page.compactCalls, "a targeted request must not cancel or restart the root stream")
	assert.Equal(t, []string{"child-1"}, rt.compacted)

	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	note, ok := msgs[0].(notification.ShowMsg)
	require.True(t, ok, "expected a notification, got %T", msgs[0])
	assert.Equal(t, notification.TypeInfo, note.Type)
	assert.Contains(t, note.Text, "worker")
	assert.Contains(t, note.Text, "child-1")
}

func TestHandleCompactSession_TargetedErrorNotifies(t *testing.T) {
	t.Parallel()

	rt := &liveCompactRuntime{compactErr: errors.New("session child-1 is not live")}
	m, page := newCompactTestModel(t, rt, session.New())

	_, cmd := m.Update(messages.CompactSessionMsg{SessionID: "child-1"})

	assert.Empty(t, page.compactCalls)
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	note, ok := msgs[0].(notification.ShowMsg)
	require.True(t, ok, "expected a notification, got %T", msgs[0])
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "not live")
}

func TestHandleCompactSession_UnsupportedRuntimeNotifies(t *testing.T) {
	t.Parallel()

	m, page := newCompactTestModel(t, stubRuntime{}, session.New())

	_, cmd := m.Update(messages.CompactSessionMsg{SessionID: "child-1"})

	assert.Empty(t, page.compactCalls)
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	note, ok := msgs[0].(notification.ShowMsg)
	require.True(t, ok, "expected a notification, got %T", msgs[0])
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "not supported")
}
