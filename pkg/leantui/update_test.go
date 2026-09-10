package leantui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type cycleThinkingRuntime struct {
	supports   bool
	level      effort.Level
	err        error
	cycleCalls int
	setCalls   int
	setLevel   effort.Level
	runCalls   int
	followUps  []runtime.QueuedMessage
	steered    []runtime.QueuedMessage
	steerErr   error
	models     []runtime.ModelChoice
	modelRef   string
	modelErr   error
	store      session.Store
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
	r.runCalls++
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
func (r *cycleThinkingRuntime) SessionStore() session.Store { return r.store }
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

func TestFirstMessageBangCommandRunsLocally(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "bang-output")
	rt := &cycleThinkingRuntime{}
	m := bareModel(80)
	m.app = app.New(t.Context(), rt, session.New())

	m.sendFirstMessage(t.Context(), `!printf bang > "`+outputPath+`"`, "")

	output, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "bang", string(output))
	assert.Zero(t, rt.runCalls)
}

func TestSubmitBangCommandRunsImmediatelyWhileBusy(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "bang-output")
	rt := &cycleThinkingRuntime{}
	m := bareModel(80)
	m.app = app.New(t.Context(), rt, session.New())
	m.busy = true

	m.submitEditor(t.Context(), `!printf bang > "`+outputPath+`"`)

	output, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "bang", string(output))
	assert.Zero(t, rt.runCalls)
	assert.Empty(t, rt.steered)
	assert.Empty(t, m.queue)
	assert.True(t, m.busy)
}

func TestSubmitBangCommandHonorsReadOnlySession(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "bang-output")
	rt := &cycleThinkingRuntime{}
	m := bareModel(80)
	m.app = app.New(t.Context(), rt, session.New(), app.WithReadOnly())

	m.submitEditor(t.Context(), `!printf bang > "`+outputPath+`"`)

	_, err := os.Stat(outputPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Zero(t, rt.runCalls)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "This session is read-only.")
}

func TestSessionsCommandListsCurrentDirectoryAndResumesSelection(t *testing.T) {
	t.Parallel()
	store := session.NewInMemorySessionStore()
	workingDir := t.TempDir()
	current := session.New(session.WithWorkingDir(workingDir))
	resumable := session.New(session.WithWorkingDir(workingDir))
	resumable.Title = "Previous work"
	resumable.CreatedAt = time.Date(2026, time.September, 2, 14, 30, 0, 0, time.Local)
	resumable.AddMessage(session.UserMessage("continue this task"))
	resumable.AddMessage(session.NewAgentMessage("coder", &chat.Message{Role: chat.MessageRoleAssistant, Content: "previous answer"}))
	other := session.New(session.WithWorkingDir(t.TempDir()))
	other.Title = "Other directory"
	require.NoError(t, store.AddSession(t.Context(), resumable))
	require.NoError(t, store.AddSession(t.Context(), other))

	rt := &cycleThinkingRuntime{store: store}
	m := bareModel(80)
	m.app = app.New(t.Context(), rt, current)
	m.sessionState = service.NewSessionState(current)

	assert.True(t, m.handleSlash(t.Context(), "/sessions", busySubmitSteer))
	assert.Equal(t, "/sessions ", m.screen.Editor.Text())
	assert.True(t, m.screen.Autocomplete.Active)
	cmd, ok := m.screen.Autocomplete.Current()
	require.True(t, ok)
	assert.Equal(t, "Previous work", cmd.Name)
	assert.Equal(t, "/sessions "+resumable.ID, m.screen.Autocomplete.Completion(cmd))

	assert.True(t, m.handleSlash(t.Context(), m.screen.Autocomplete.Completion(cmd), busySubmitSteer))
	assert.Equal(t, resumable.ID, m.app.Session().ID)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "continue this task")
	assert.Contains(t, transcript, "previous answer")
	assert.NotContains(t, transcript, "Other directory")
}

func TestLoadInitialSessionTranscriptMarksRestoredUserMessages(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(session.UserMessage("restored question"))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "restored answer",
	}))

	m := bareModel(80)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, sess)
	m.sessionState = service.NewSessionState(sess)
	m.loadInitialSessionTranscript()

	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "\x1b]133;A;redraw=0\x07")
	assert.Contains(t, transcript, "restored question")
	assert.Contains(t, transcript, "restored answer")
}

func TestLoadSessionTranscriptRestoresToolCalls(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.AddMessage(session.UserMessage("inspect the repository"))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		ReasoningContent: "I should inspect the files.",
		Content:          "I found the issue.",
		ToolCalls: []tools.ToolCall{{
			ID: "call-1",
			Function: tools.FunctionCall{
				Name:      "inspect_files",
				Arguments: `{"path":"pkg/leantui"}`,
			},
		}},
		ToolDefinitions: []tools.Tool{{Name: "inspect_files"}},
	}))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "call-1",
		Content:    "update.go\nview.go",
	}))

	m := bareModel(80)
	m.sessionState = service.NewSessionState(sess)
	m.loadSessionTranscript(sess)

	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "inspect the repository")
	assert.Contains(t, transcript, "I should inspect the files.")
	assert.Contains(t, transcript, "I found the issue.")
	assert.Contains(t, transcript, "inspect_files")
	assert.Contains(t, transcript, "pkg/leantui")
	assert.Contains(t, transcript, "update.go")
	assert.Zero(t, m.screen.Transcript.ToolCount())
}

func TestSessionsCommandReportsNoSessionsInCurrentDirectory(t *testing.T) {
	t.Parallel()
	store := session.NewInMemorySessionStore()
	other := session.New(session.WithWorkingDir(t.TempDir()))
	require.NoError(t, store.AddSession(t.Context(), other))

	current := session.New(session.WithWorkingDir(t.TempDir()))
	m := bareModel(80)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{store: store}, current)

	assert.True(t, m.handleSlash(t.Context(), "/sessions", busySubmitSteer))
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "No previous sessions found in this directory")
	assert.False(t, m.screen.Autocomplete.Active)
}

func TestSessionsCommandDoesNotSwitchWhileBusy(t *testing.T) {
	t.Parallel()
	store := session.NewInMemorySessionStore()
	current := session.New(session.WithWorkingDir(t.TempDir()))
	m := bareModel(80)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{store: store}, current)
	m.busy = true

	assert.True(t, m.handleSlash(t.Context(), "/sessions", busySubmitSteer))
	assert.Equal(t, current.ID, m.app.Session().ID)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, true, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "Wait for the current response to finish")
}

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

func TestEscapeInterruptsActiveRun(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	runCtx, cancel := context.WithCancel(t.Context())
	m.busy = true
	m.runCancel = cancel
	m.queue = []ui.PendingUserMessage{{Content: "queued"}}
	m.pendingUsers = []ui.PendingUserMessage{{Content: "pending"}}
	m.ignoredUsers = []string{"ignored"}
	m.screen.Confirm = &ui.ConfirmModel{}
	m.screen.Autocomplete.SetCommands([]ui.Command{{Name: "help"}})
	m.screen.Autocomplete.Sync("/h")

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})

	require.ErrorIs(t, runCtx.Err(), context.Canceled)
	assert.Empty(t, m.queue)
	assert.Empty(t, m.pendingUsers)
	assert.Empty(t, m.ignoredUsers)
	assert.Nil(t, m.screen.Confirm)
	assert.NotContains(t, strings.Join(m.screen.Transcript.Lines(80, 0, true, m.sessionState, nil), "\n"), "Cancelled")

	m.screen.Transcript.AppendAssistant("partial response")
	m.handleEvent(t.Context(), runtime.StreamStopped("session", "coder", "canceled"))

	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	responseAt := strings.Index(transcript, "partial response")
	cancelledAt := strings.Index(transcript, "Cancelled")
	assert.NotEqual(t, -1, responseAt)
	assert.NotEqual(t, -1, cancelledAt)
	assert.Less(t, responseAt, cancelledAt)
}

func TestCtrlCCancelMarkerFollowsBufferedResponse(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.busy = true
	m.runCancel = func() {}

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyCtrlC})
	m.screen.Transcript.AppendAssistant("partial response")
	m.handleEvent(t.Context(), runtime.StreamStopped("session", "coder", "canceled"))

	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	responseAt := strings.Index(transcript, "partial response")
	cancelledAt := strings.Index(transcript, "Cancelled")
	assert.NotEqual(t, -1, responseAt)
	assert.NotEqual(t, -1, cancelledAt)
	assert.Less(t, responseAt, cancelledAt)
}

func TestEscapeWhileIdleDismissesAutocomplete(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Autocomplete.SetCommands([]ui.Command{{Name: "help"}})
	require.True(t, m.screen.Autocomplete.Sync("/h"))

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})

	assert.False(t, m.screen.Autocomplete.Active)
	assert.False(t, m.quitting)
}

func TestShiftEnterInsertsNewline(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Editor.SetText("first line")

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyShiftEnter})

	assert.Equal(t, "first line\n", m.screen.Editor.Text())
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
	joined := strings.Join(m.screen.Transcript.Lines(80, 0, true, m.sessionState, m.pendingUsers), "\n")
	assert.Contains(t, joined, "Follow-up: do this next")
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
	assert.Contains(t, joined, "Steering: turn left")
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

func TestCopyCommandCopiesLastAssistantResponse(t *testing.T) {
	sess := session.New()
	sess.AddMessage(session.UserMessage("question"))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "the last response",
	}))
	m := bareModel(80)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, sess)

	originalWriteClipboard := writeClipboard
	t.Cleanup(func() { writeClipboard = originalWriteClipboard })
	var copied string
	writeClipboard = func(text string) error {
		copied = text
		return nil
	}

	assert.True(t, m.handleSlash(t.Context(), "/copy", busySubmitSteer))
	assert.Equal(t, "the last response", copied)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "Last response copied to clipboard.")
}

func TestCopyCommandReportsMissingAssistantResponse(t *testing.T) {
	m := bareModel(80)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, session.New())

	originalWriteClipboard := writeClipboard
	t.Cleanup(func() { writeClipboard = originalWriteClipboard })
	called := false
	writeClipboard = func(string) error {
		called = true
		return nil
	}

	assert.True(t, m.handleSlash(t.Context(), "/copy", busySubmitSteer))
	assert.False(t, called)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "No assistant response to copy.")
}
