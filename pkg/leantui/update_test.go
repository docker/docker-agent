package leantui

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

type cycleThinkingRuntime struct {
	supports   bool
	level      effort.Level
	err        error
	cycleCalls int
	setCalls   int
	setLevel   effort.Level
	followUps  []runtime.QueuedMessage
	steered    []runtime.QueuedMessage
	steerErr   error
	models     []runtime.ModelChoice
	modelRef   string
	modelErr   error
}

func (r *cycleThinkingRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (r *cycleThinkingRuntime) CurrentAgentName(context.Context) string       { return "coder" }
func (r *cycleThinkingRuntime) SetCurrentAgent(context.Context, string) error { return nil }
func (r *cycleThinkingRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}
func (r *cycleThinkingRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }
func (r *cycleThinkingRuntime) RestartToolset(context.Context, string) error       { return nil }
func (r *cycleThinkingRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}

func (r *cycleThinkingRuntime) EmitAgentInfo(_ context.Context, sink runtime.EventSink) {
	sink.Emit(runtime.TeamInfo([]runtime.AgentDetails{{Name: "coder", Thinking: r.level.String()}}, "coder"))
}
func (r *cycleThinkingRuntime) ResetStartupInfo() {}
func (r *cycleThinkingRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (r *cycleThinkingRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (r *cycleThinkingRuntime) Resume(context.Context, runtime.ResumeRequest) {}
func (r *cycleThinkingRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any, ...string) error {
	return nil
}
func (r *cycleThinkingRuntime) SessionStore() session.Store { return nil }
func (r *cycleThinkingRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {
}
func (r *cycleThinkingRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }
func (r *cycleThinkingRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return nil
}

func (r *cycleThinkingRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (r *cycleThinkingRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (r *cycleThinkingRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (r *cycleThinkingRuntime) UpdateSessionTitle(_ context.Context, sess *session.Session, title string) error {
	sess.Title = title
	return nil
}

func (r *cycleThinkingRuntime) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }

func (r *cycleThinkingRuntime) Close() error { return nil }
func (r *cycleThinkingRuntime) Stop()        {}
func (r *cycleThinkingRuntime) Steer(_ context.Context, msg runtime.QueuedMessage) error {
	if r.steerErr != nil {
		return r.steerErr
	}
	r.steered = append(r.steered, msg)
	return nil
}

func (r *cycleThinkingRuntime) FollowUp(_ context.Context, msg runtime.QueuedMessage) error {
	r.followUps = append(r.followUps, msg)
	return nil
}
func (r *cycleThinkingRuntime) QueueStatus() runtime.QueueStatus { return runtime.QueueStatus{} }

func (r *cycleThinkingRuntime) TogglePause(context.Context) (bool, error) {
	return false, nil
}

func (r *cycleThinkingRuntime) SetAgentModel(_ context.Context, _, modelRef string) error {
	r.modelRef = modelRef
	return r.modelErr
}

func (r *cycleThinkingRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	r.cycleCalls++
	if r.err != nil {
		return "", r.err
	}
	return r.level, nil
}

func (r *cycleThinkingRuntime) SetAgentThinkingLevel(_ context.Context, _ string, level effort.Level) (effort.Level, error) {
	r.setCalls++
	if r.err != nil {
		return "", r.err
	}
	r.setLevel = level
	return level, nil
}

func (r *cycleThinkingRuntime) AvailableModels(context.Context) []runtime.ModelChoice {
	return r.models
}
func (r *cycleThinkingRuntime) SupportsModelSwitching() bool             { return r.supports }
func (r *cycleThinkingRuntime) OnToolsChanged(func(runtime.Event))       {}
func (r *cycleThinkingRuntime) OnBackgroundEvent(func(runtime.Event))    {}
func (r *cycleThinkingRuntime) OnElicitationRequest(func(runtime.Event)) {}

var _ runtime.Runtime = (*cycleThinkingRuntime)(nil)

func TestShiftTabCyclesThinkingLevel(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{supports: true, level: effort.High}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyShiftTab})

	assert.Equal(t, 1, rt.cycleCalls)
	assert.Equal(t, "high", m.status.Thinking)
	assert.Zero(t, m.screen.Transcript.BlockCount())
}

func TestShiftTabReportsUnsupportedThinkingLevel(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{supports: true, err: runtime.ErrUnsupported}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyShiftTab})

	assert.Equal(t, 1, rt.cycleCalls)
	assert.Empty(t, m.status.Thinking)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestEffortCommandSetsThinkingLevel(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{supports: true}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())

	m.handleSetThinkingLevel(t.Context(), "high")

	assert.Equal(t, 1, rt.setCalls)
	assert.Equal(t, effort.High, rt.setLevel)
	assert.Equal(t, "high", m.status.Thinking)
}

func TestEffortCommandRejectsUnknownLevel(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{supports: true}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())

	m.handleSetThinkingLevel(t.Context(), "turbo")

	assert.Zero(t, rt.setCalls)
	assert.Empty(t, m.status.Thinking)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestModelCommandSwitchesModel(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{supports: true}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())

	assert.True(t, m.handleSlash(t.Context(), "/model openai/gpt-5", busySubmitSteer))

	assert.Equal(t, "openai/gpt-5", rt.modelRef)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestModelCommandOpensModelAutocomplete(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{
		supports: true,
		models: []runtime.ModelChoice{
			{Name: "GPT 5", Ref: "openai/gpt-5", Provider: "openai", Model: "gpt-5", IsCurrent: true, IsDefault: true},
			{Name: "Sonnet", Ref: "anthropic/claude-sonnet-4-6", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
	}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())

	assert.True(t, m.handleSlash(t.Context(), "/model", busySubmitSteer))

	assert.Equal(t, "/model ", m.screen.Editor.Text())
	assert.True(t, m.screen.Autocomplete.Active)
	cmd, ok := m.screen.Autocomplete.Current()
	if assert.True(t, ok) {
		assert.Equal(t, "openai/gpt-5", cmd.Name)
		assert.Equal(t, "/model default", m.screen.Autocomplete.Completion(cmd))
	}

	assert.True(t, m.screen.Autocomplete.Sync("/model gpt 5"))
	cmd, ok = m.screen.Autocomplete.Current()
	if assert.True(t, ok) {
		assert.Equal(t, "openai/gpt-5", cmd.Name)
		assert.Equal(t, "/model default", m.screen.Autocomplete.Completion(cmd))
	}
}

func TestAltEnterWhileBusyQueuesRuntimeFollowUp(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())
	m.busy = true
	m.screen.Editor.SetText("do this next")

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyAltEnter})

	if assert.Len(t, rt.followUps, 1) {
		assert.Equal(t, "do this next", rt.followUps[0].Content)
	}
	assert.Empty(t, rt.steered)
	assert.Empty(t, m.queue)
	assert.Len(t, m.pendingUsers, 1)
	assert.True(t, m.screen.Editor.IsEmpty())
}

func TestEditorSubmitWhileBusySteersAndRendersAtStreamEnd(t *testing.T) {
	t.Parallel()
	rt := &cycleThinkingRuntime{}
	m := bareModel(24)
	m.app = app.New(t.Context(), rt, session.New())
	m.busy = true
	m.screen.Transcript.AppendAssistant("assistant is still streaming")
	m.screen.Editor.SetText("turn left")

	m.handleEnter(t.Context())

	if assert.Len(t, rt.steered, 1) {
		assert.Equal(t, "turn left", rt.steered[0].Content)
	}
	assert.Empty(t, m.queue)
	assert.Len(t, m.pendingUsers, 1)

	joined := strings.Join(m.screen.Transcript.Lines(80, 0, true, m.sessionState, m.pendingUsers), "\n")
	assistantAt := strings.Index(joined, "assistant is still streaming")
	steerAt := strings.Index(joined, "turn left")
	assert.NotEqual(t, -1, assistantAt)
	assert.NotEqual(t, -1, steerAt)
	assert.Less(t, assistantAt, steerAt)
}

func TestSteeredUserEventConfirmsPendingAfterAssistant(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.busy = true
	m.screen.Transcript.AppendAssistant("assistant response")
	m.addPendingUser("/change", "resolved steering prompt", ui.PendingUserSteer)

	m.handleEvent(t.Context(), runtime.UserMessage("resolved steering prompt\n", "session", nil, 1))

	assert.Empty(t, m.pendingUsers)
	assert.Equal(t, 2, m.screen.Transcript.BlockCount())
	joined := strings.Join(m.screen.Transcript.Lines(80, 0, true, m.sessionState, nil), "\n")
	assistantAt := strings.Index(joined, "assistant response")
	steerAt := strings.Index(joined, "/change")
	assert.NotEqual(t, -1, assistantAt)
	assert.NotEqual(t, -1, steerAt)
	assert.Less(t, assistantAt, steerAt)
}
