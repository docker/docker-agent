package cli

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// mockRuntime implements runtime.Runtime for testing the CLI runner.
// It emits pre-configured events from RunStream and records Resume calls.
type mockRuntime struct {
	events []runtime.Event

	mu                    sync.Mutex
	resumes               []runtime.ResumeRequest
	elicitationDeclines   int
	elicitationLastAction tools.ElicitationAction
}

func (m *mockRuntime) CurrentAgentName() string { return "test" }
func (m *mockRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{Name: "test"}
}
func (m *mockRuntime) SetCurrentAgent(string) error                                         { return nil }
func (m *mockRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error)              { return nil, nil }
func (m *mockRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus                   { return nil }
func (m *mockRuntime) RestartToolset(context.Context, string) error                         { return nil }
func (m *mockRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}
func (m *mockRuntime) ResetStartupInfo()                                                    {}
func (m *mockRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}

func (m *mockRuntime) ResumeElicitation(_ context.Context, action tools.ElicitationAction, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.elicitationDeclines++
	m.elicitationLastAction = action
	return nil
}
func (m *mockRuntime) SessionStore() session.Store                                            { return nil }
func (m *mockRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {}
func (m *mockRuntime) PermissionsInfo() *runtime.PermissionsInfo                              { return nil }
func (m *mockRuntime) CurrentAgentSkillsToolset() *skillstool.Toolset                         { return nil }
func (m *mockRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (m *mockRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}
func (m *mockRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error    { return nil }
func (m *mockRuntime) TitleGenerator() *sessiontitle.Generator                               { return nil }
func (m *mockRuntime) Close() error                                                          { return nil }
func (m *mockRuntime) Steer(runtime.QueuedMessage) error                                     { return nil }
func (m *mockRuntime) FollowUp(runtime.QueuedMessage) error                                  { return nil }
func (m *mockRuntime) QueueStatus() runtime.QueueStatus                                      { return runtime.QueueStatus{} }
func (m *mockRuntime) TogglePause(context.Context) (bool, error)                             { return false, nil }
func (m *mockRuntime) SetAgentModel(context.Context, string, string) error                   { return nil }
func (m *mockRuntime) AvailableModels(context.Context) []runtime.ModelChoice                 { return nil }
func (m *mockRuntime) SupportsModelSwitching() bool                                          { return false }
func (m *mockRuntime) OnToolsChanged(func(runtime.Event))                                    {}
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

func TestElicitationAutoDeclineInJSONMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{
			&runtime.ElicitationRequestEvent{
				Type:    "elicitation_request",
				Message: "Please authorize",
				Meta:    map[string]any{"cagent/server_url": "https://example.com"},
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
