package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/jscommands"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

func TestMain(m *testing.M) {
	// PrepareUserMessage tests exercise ${...} expansion without going
	// through Run, which normally does this registration.
	jscommands.Register()
	os.Exit(m.Run())
}

// mockRuntime implements runtime.Runtime for testing the CLI runner.
// It emits pre-configured events from RunStream and records Resume calls.
type mockRuntime struct {
	events []runtime.Event

	mu                    sync.Mutex
	resumes               []runtime.ResumeRequest
	elicitationDeclines   int
	elicitationLastAction tools.ElicitationAction
}

// mockRuntimeWithOverrides extends mockRuntime to allow method overriding for testing
type mockRuntimeWithOverrides struct {
	*mockRuntime

	setCurrentAgentFn  func(string) error
	currentAgentInfoFn func(context.Context) runtime.CurrentAgentInfo
}

func (m *mockRuntimeWithOverrides) SetCurrentAgent(ctx context.Context, name string) error {
	if m.setCurrentAgentFn != nil {
		return m.setCurrentAgentFn(name)
	}
	return m.mockRuntime.SetCurrentAgent(ctx, name)
}

func (m *mockRuntimeWithOverrides) CurrentAgentInfo(ctx context.Context) runtime.CurrentAgentInfo {
	if m.currentAgentInfoFn != nil {
		return m.currentAgentInfoFn(ctx)
	}
	return m.mockRuntime.CurrentAgentInfo(ctx)
}

func (m *mockRuntime) CurrentAgentName(context.Context) string { return "test" }
func (m *mockRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{Name: "test"}
}

func (m *mockRuntime) SetCurrentAgent(context.Context, string) error { return nil }

func (m *mockRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (m *mockRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }

func (m *mockRuntime) RestartToolset(context.Context, string) error                         { return nil }
func (m *mockRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}
func (m *mockRuntime) EmitAgentInfo(context.Context, runtime.EventSink)                     {}
func (m *mockRuntime) ResetStartupInfo()                                                    {}
func (m *mockRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}

func (m *mockRuntime) ResumeElicitation(_ context.Context, action tools.ElicitationAction, _ map[string]any, _ ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.elicitationDeclines++
	m.elicitationLastAction = action
	return nil
}

func (m *mockRuntime) SessionStore() session.Store                                            { return nil }
func (m *mockRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {}
func (m *mockRuntime) PermissionsInfo() *runtime.PermissionsInfo                              { return nil }

func (m *mockRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet { return nil }

func (m *mockRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (m *mockRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (m *mockRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (m *mockRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error { return nil }

func (m *mockRuntime) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }

func (m *mockRuntime) Close() error { return nil }

func (m *mockRuntime) Steer(context.Context, runtime.QueuedMessage) error { return nil }

func (m *mockRuntime) FollowUp(context.Context, runtime.QueuedMessage) error { return nil }

func (m *mockRuntime) QueueStatus() runtime.QueueStatus { return runtime.QueueStatus{} }

func (m *mockRuntime) TogglePause(context.Context) (bool, error) { return false, nil }

func (m *mockRuntime) SetAgentModel(context.Context, string, string) error { return nil }

func (m *mockRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (m *mockRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (m *mockRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }

func (m *mockRuntime) SupportsModelSwitching() bool                                          { return false }
func (m *mockRuntime) OnToolsChanged(func(runtime.Event))                                    {}
func (m *mockRuntime) OnBackgroundEvent(func(runtime.Event))                                 {}
func (m *mockRuntime) OnElicitationRequest(func(runtime.Event))                              {}
func (m *mockRuntime) RegenerateTitle(context.Context, *session.Session, chan runtime.Event) {}

func (m *mockRuntime) Resume(_ context.Context, req runtime.ResumeRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumes = append(m.resumes, req)
}

func (m *mockRuntime) RunStream(_ context.Context, _ *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event, len(m.events))
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch
}

func (m *mockRuntime) getResumes() []runtime.ResumeRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]runtime.ResumeRequest, len(m.resumes))
	copy(result, m.resumes)
	return result
}

func maxIterEvent(maxIter int) *runtime.MaxIterationsReachedEvent {
	return &runtime.MaxIterationsReachedEvent{
		Type:          "max_iterations_reached",
		MaxIterations: maxIter,
	}
}

func TestMaxIterationsAutoApproveInYoloMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{maxIterEvent(60)},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1)
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeApprove)
}

func TestMaxIterationsAutoApproveSafetyCap(t *testing.T) {
	t.Parallel()

	// Emit maxAutoExtensions+1 events to trigger the safety cap
	events := make([]runtime.Event, maxAutoExtensions+1)
	for i := range events {
		events[i] = maxIterEvent(60 + i*10)
	}

	rt := &mockRuntime{events: events}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), maxAutoExtensions+1)

	// First maxAutoExtensions should be approved
	for i := range maxAutoExtensions {
		assert.Equal(t, resumes[i].Type, runtime.ResumeTypeApprove,
			"extension %d should be approved", i+1)
	}
	// Last one should be rejected (safety cap)
	assert.Equal(t, resumes[maxAutoExtensions].Type, runtime.ResumeTypeReject,
		"extension beyond cap should be rejected")
}

func TestMaxIterationsAutoApproveJSONMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{maxIterEvent(60)},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1)
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeApprove)
}

func TestMaxIterationsRejectInJSONModeWithoutYolo(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{maxIterEvent(60)},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: false, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1)
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeReject)
}

// JSON mode has no stdin user — a ToolCallConfirmationEvent must be
// rejected even under --yolo (AutoApprove=true), because the event
// only fires when a preempt-yolo hook overrode yolo. Without this,
// eval containers hang on the Resume channel.
func TestToolCallConfirmationRejectedInJSONModeUnderYolo(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{
			&runtime.ToolCallConfirmationEvent{
				Type: "tool_call_confirmation",
				ToolCall: tools.ToolCall{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rm -rf /tmp/x"}`},
				},
			},
		},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1, "one Resume expected")
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeReject, "JSON+yolo must reject confirmation events")
}

func TestElicitationAutoDeclineInJSONMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{
			&runtime.ElicitationRequestEvent{
				Type:    "elicitation_request",
				Message: "Please authorize",
				Meta:    map[string]any{"docker-agent/server_url": "https://example.com"},
			},
		},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, rt.elicitationDeclines, 1)
	assert.Equal(t, rt.elicitationLastAction, tools.ElicitationAction("decline"))
}

func TestMaxIterationsSafetyCapJSONMode(t *testing.T) {
	t.Parallel()

	events := make([]runtime.Event, maxAutoExtensions+1)
	for i := range events {
		events[i] = maxIterEvent(60 + i*10)
	}

	rt := &mockRuntime{events: events}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), maxAutoExtensions+1)

	for i := range maxAutoExtensions {
		assert.Equal(t, resumes[i].Type, runtime.ResumeTypeApprove)
	}
	assert.Equal(t, resumes[maxAutoExtensions].Type, runtime.ResumeTypeReject)
}

// TestPrepareUserMessage_AgentSwitching tests that PrepareUserMessage correctly
// handles agent-switching commands and returns empty messages on switch failures.
func TestPrepareUserMessage_AgentSwitching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		userInput         string
		commandAgent      string
		setAgentErr       error
		expectedContent   string
		expectedAttach    string
		expectAgentSwitch bool
		expectNilMessage  bool
		expectError       bool
	}{
		{
			name:              "agent switch succeeds with trailing args",
			userInput:         "/plan design a login flow",
			commandAgent:      "planner",
			setAgentErr:       nil,
			expectedContent:   "design a login flow",
			expectedAttach:    "",
			expectAgentSwitch: true,
		},
		{
			name:              "agent switch succeeds without trailing args",
			userInput:         "/plan",
			commandAgent:      "planner",
			setAgentErr:       nil,
			expectedContent:   "",
			expectedAttach:    "",
			expectAgentSwitch: true,
			expectNilMessage:  true,
		},
		{
			name:              "agent switch fails - returns error",
			userInput:         "/plan design a login flow",
			commandAgent:      "planner",
			setAgentErr:       errors.New("agent not found"),
			expectedContent:   "",
			expectedAttach:    "",
			expectAgentSwitch: true,
			expectError:       true,
		},
		{
			name:              "non-agent command - no switch",
			userInput:         "/test regular command",
			commandAgent:      "",
			setAgentErr:       nil,
			expectedContent:   "This is the test instruction regular command",
			expectedAttach:    "",
			expectAgentSwitch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create a mock runtime that tracks SetCurrentAgent calls
			var setAgentCalled bool
			var setAgentName string
			rt := &mockRuntimeWithOverrides{
				mockRuntime: &mockRuntime{events: []runtime.Event{}},
			}

			// Override SetCurrentAgent to track calls and return the test error
			rt.setCurrentAgentFn = func(name string) error {
				setAgentCalled = true
				setAgentName = name
				return tt.setAgentErr
			}

			// Override CurrentAgentInfo to return test commands
			rt.currentAgentInfoFn = func(context.Context) runtime.CurrentAgentInfo {
				commands := make(map[string]types.Command)
				if tt.commandAgent != "" {
					commands["plan"] = types.Command{
						Description: "Hand off to the planner",
						Agent:       tt.commandAgent,
					}
				} else {
					commands["test"] = types.Command{
						Instruction: "This is the test instruction",
					}
				}
				return runtime.CurrentAgentInfo{
					Name:     "test",
					Commands: commands,
				}
			}

			msg, attachPath, err := PrepareUserMessage(t.Context(), rt, tt.userInput, "")
			if tt.expectError {
				assert.Assert(t, err != nil, "Expected error but got nil")
				return
			}
			assert.NilError(t, err)

			// Verify agent switch was called (or not)
			assert.Equal(t, tt.expectAgentSwitch, setAgentCalled, "SetCurrentAgent call mismatch")
			if tt.expectAgentSwitch && setAgentCalled {
				assert.Equal(t, tt.commandAgent, setAgentName, "Wrong agent name passed to SetCurrentAgent")
			}

			// Verify message content
			if tt.expectNilMessage {
				assert.Assert(t, msg == nil, "Expected nil message")
			} else {
				assert.Equal(t, tt.expectedContent, msg.Message.Content, "Message content mismatch")
			}
			assert.Equal(t, tt.expectedAttach, attachPath, "Attachment path mismatch")
		})
	}
}

func TestPrepareUserMessage_EmptyMessageForAgentOnlyCommand(t *testing.T) {
	t.Parallel()

	rt := &mockRuntimeWithOverrides{
		mockRuntime: &mockRuntime{},
	}
	rt.currentAgentInfoFn = func(context.Context) runtime.CurrentAgentInfo {
		return runtime.CurrentAgentInfo{
			Name: "test",
			Commands: map[string]types.Command{
				"plan": {Agent: "planner"}, // agent-only, no instruction
			},
		}
	}

	msg, attachPath, err := PrepareUserMessage(t.Context(), rt, "/plan", "")
	assert.NilError(t, err)

	// Agent-only command with no args should produce nil message
	assert.Assert(t, msg == nil, "Expected nil message for agent-only command with no args")
	assert.Equal(t, "", attachPath, "Expected no attachment")
}

// TestPrepareUserMessage_CommandResolution tests that commands are resolved
// correctly before agent switching.
func TestPrepareUserMessage_CommandResolution(t *testing.T) {
	t.Parallel()

	rt := &mockRuntimeWithOverrides{
		mockRuntime: &mockRuntime{},
	}
	rt.currentAgentInfoFn = func(context.Context) runtime.CurrentAgentInfo {
		return runtime.CurrentAgentInfo{
			Name: "test",
			Commands: map[string]types.Command{
				"fix": {
					Instruction: "Fix the file ${args[0]}",
				},
			},
		}
	}

	msg, _, err := PrepareUserMessage(t.Context(), rt, "/fix main.go", "")
	assert.NilError(t, err)

	assert.Equal(t, "Fix the file main.go", msg.Message.Content, "Command should be resolved with args")
}

// swapStdin replaces os.Stdin with a pipe carrying the given content for the
// duration of the test. A pipe is never a terminal, so it also exercises the
// non-TTY stdin paths. Tests using it must not run in parallel: os.Stdin is
// process-global state.
func swapStdin(t *testing.T, content string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdin pipe: %v", err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatalf("writing stdin pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing stdin pipe: %v", err)
	}

	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
}

// A run that would silently do nothing (no message, empty non-TTY stdin, e.g.
// bare `docker agent` in CI) must fail with an actionable error instead of
// exiting 0 with no output (issue #3442).
func TestRunEmptyStdinNonTTYFails(t *testing.T) {
	swapStdin(t, "")

	rt := &mockRuntime{}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, sess, nil)
	assert.ErrorContains(t, err, "no message provided and stdin is not a terminal")
	assert.ErrorContains(t, err, "interactive terminal")
}

func TestRunEmptyStdinWithDashFails(t *testing.T) {
	swapStdin(t, "")

	rt := &mockRuntime{}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, sess, []string{"-"})
	assert.ErrorContains(t, err, "no message received on stdin")
}

func TestRunPipedStdinStillWorks(t *testing.T) {
	swapStdin(t, "hello agent\n")

	rt := &mockRuntime{
		events: []runtime.Event{&runtime.AgentChoiceEvent{Content: "4"}},
	}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, sess, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(sess.GetAllMessages()) > 0, true)
}

// An ErrorEvent must surface exactly once: returned to the command layer
// (which prints it), never also printed by the runner (issue #3442).
func TestErrorEventReturnedNotPrinted(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{runtime.Error("model failed: HTTP 404")},
	}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, sess, []string{"hello"})
	assert.ErrorContains(t, err, "model failed: HTTP 404")

	var runtimeErr RuntimeError
	assert.Equal(t, errors.As(err, &runtimeErr), true)
	assert.Equal(t, strings.Contains(buf.String(), "model failed"), false)
}
