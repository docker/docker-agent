package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

const testSessionID = "acp-test-session"

// fakeRuntime is a minimal runtime.Runtime for driving runAgent, adapted from
// the mock in pkg/app/app_test.go: RunStream replays the configured events and
// Resume records the requests it receives.
type fakeRuntime struct {
	events []runtime.Event
	// onRunStream runs synchronously when RunStream is called, before any
	// event is delivered (e.g. to cancel the turn context).
	onRunStream func()

	mu          sync.Mutex
	resumeCalls []runtime.ResumeRequest
}

var _ runtime.Runtime = (*fakeRuntime)(nil)

// RunStream returns a pre-filled closed channel so no producer goroutine can
// leak when runAgent returns before draining all events.
func (f *fakeRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	if f.onRunStream != nil {
		f.onRunStream()
	}
	ch := make(chan runtime.Event, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch
}

func (f *fakeRuntime) Resume(_ context.Context, req runtime.ResumeRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls = append(f.resumeCalls, req)
}

func (f *fakeRuntime) resumeRequests() []runtime.ResumeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.resumeCalls)
}

func (f *fakeRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (f *fakeRuntime) CurrentAgentName(context.Context) string                 { return "fake" }
func (f *fakeRuntime) SetCurrentAgent(context.Context, string) error           { return nil }
func (f *fakeRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (f *fakeRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus      { return nil }
func (f *fakeRuntime) RestartToolset(context.Context, string) error            { return nil }
func (f *fakeRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}
func (f *fakeRuntime) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (f *fakeRuntime) ResetStartupInfo()                                {}
func (f *fakeRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}

func (f *fakeRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any, ...string) error {
	return nil
}
func (f *fakeRuntime) SessionStore() session.Store { return nil }
func (f *fakeRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {
}
func (f *fakeRuntime) PermissionsInfo() *runtime.PermissionsInfo      { return nil }
func (f *fakeRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet { return nil }
func (f *fakeRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (f *fakeRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (f *fakeRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (f *fakeRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (f *fakeRuntime) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }
func (f *fakeRuntime) Steer(context.Context, runtime.QueuedMessage) error     { return nil }
func (f *fakeRuntime) FollowUp(context.Context, runtime.QueuedMessage) error  { return nil }
func (f *fakeRuntime) QueueStatus() runtime.QueueStatus                       { return runtime.QueueStatus{} }
func (f *fakeRuntime) TogglePause(context.Context) (bool, error)              { return false, nil }
func (f *fakeRuntime) SetAgentModel(context.Context, string, string) error    { return nil }
func (f *fakeRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (f *fakeRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}
func (f *fakeRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }
func (f *fakeRuntime) SupportsModelSwitching() bool                          { return false }
func (f *fakeRuntime) OnToolsChanged(func(runtime.Event))                    {}
func (f *fakeRuntime) OnBackgroundEvent(func(runtime.Event))                 {}
func (f *fakeRuntime) OnElicitationRequest(func(runtime.Event))              {}
func (f *fakeRuntime) Close() error                                          { return nil }

// captureWriter is a goroutine-safe sink for the connection's outbound
// line-delimited JSON-RPC messages. failOn, when set, is called with the
// 1-based write index and can inject write failures.
type captureWriter struct {
	failOn func(n int) error

	mu     sync.Mutex
	writes int
	buf    bytes.Buffer
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.failOn != nil {
		if err := w.failOn(w.writes); err != nil {
			return 0, err
		}
	}
	return w.buf.Write(p)
}

func (w *captureWriter) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var lines []string
	for line := range strings.SplitSeq(w.buf.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// peerResponder plays the ACP client side of the connection's outbound
// stream. Notifications pass through to the capture writer unchanged, while
// session/request_permission requests are decoded, recorded, and answered on
// the peer pipe with a JSON-RPC response echoing the request ID. respond
// picks the result the client answers with, letting tests choose the
// permission outcome; any other request fails the test immediately instead
// of deadlocking the sender.
type peerResponder struct {
	t       *testing.T
	out     io.Writer // outbound notifications, usually a captureWriter
	peer    io.Writer // write half of the connection's inbound peer pipe
	respond func(req acpsdk.RequestPermissionRequest) any

	mu       sync.Mutex
	requests []acpsdk.RequestPermissionRequest
}

// Write receives exactly one line-delimited JSON-RPC message per call: the
// SDK marshals each message and writes it in a single call under its write
// mutex. A batched write would fail to parse and fail the test loudly.
func (p *peerResponder) Write(b []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		p.t.Errorf("peer received malformed JSON-RPC message %q: %v", b, err)
		return 0, err
	}
	if len(msg.ID) == 0 {
		return p.out.Write(b)
	}
	if msg.Method != "session/request_permission" || p.respond == nil {
		err := fmt.Errorf("peer cannot answer JSON-RPC request %q (id %s)", msg.Method, msg.ID)
		p.t.Error(err)
		return 0, err
	}

	var req acpsdk.RequestPermissionRequest
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		p.t.Errorf("peer failed to decode %s params: %v", msg.Method, err)
		return 0, err
	}
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	if err := p.reply(msg.ID, p.respond(req)); err != nil {
		p.t.Errorf("peer failed to answer %s: %v", msg.Method, err)
		return 0, err
	}
	return len(b), nil
}

// reply writes a JSON-RPC response with the given request ID and result into
// the peer pipe. The pipe write only blocks until the connection's receive
// loop consumes the line, which it does continuously until cleanup.
func (p *peerResponder) reply(id json.RawMessage, result any) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	response, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: resultJSON})
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	_, err = p.peer.Write(append(response, '\n'))
	return err
}

func (p *peerResponder) recordedRequests() []acpsdk.RequestPermissionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// permissionSelected is the JSON-RPC result for a user picking optionID.
func permissionSelected(optionID string) acpsdk.RequestPermissionResponse {
	return acpsdk.RequestPermissionResponse{Outcome: acpsdk.RequestPermissionOutcome{
		Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: acpsdk.PermissionOptionId(optionID)},
	}}
}

// permissionCancelled is the JSON-RPC result for a cancelled prompt turn.
func permissionCancelled() acpsdk.RequestPermissionResponse {
	return acpsdk.RequestPermissionResponse{Outcome: acpsdk.RequestPermissionOutcome{
		Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{},
	}}
}

// runAgentFixture wires an Agent to a real SDK AgentSideConnection whose
// outbound messages flow through a peerResponder: notifications land in the
// captureWriter, and session/request_permission requests are answered over
// the peer pipe. Without a respond function the peer stays idle and only
// keeps the connection's receive loop alive until cleanup.
type runAgentFixture struct {
	agent *Agent
	sess  *Session
	rt    *fakeRuntime
	out   *captureWriter
	peer  *peerResponder
}

func newRunAgentFixture(t *testing.T, rt *fakeRuntime, out *captureWriter) *runAgentFixture {
	t.Helper()
	return newRunAgentFixtureWithPermissions(t, rt, out, nil)
}

// newRunAgentFixtureWithPermissions additionally installs respond as the
// peer-side handler for session/request_permission requests; its return
// value is marshaled as the JSON-RPC result the client answers with.
func newRunAgentFixtureWithPermissions(t *testing.T, rt *fakeRuntime, out *captureWriter, respond func(acpsdk.RequestPermissionRequest) any) *runAgentFixture {
	t.Helper()

	acpAgent := &Agent{sessions: make(map[string]*Session)}
	peerReader, peerWriter := io.Pipe()
	peer := &peerResponder{t: t, out: out, peer: peerWriter, respond: respond}
	conn := acpsdk.NewAgentSideConnection(acpAgent, peer, peerReader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	acpAgent.SetAgentConnection(conn)

	t.Cleanup(func() {
		_ = peerWriter.Close()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for ACP connection shutdown")
		}
	})

	return &runAgentFixture{
		agent: acpAgent,
		sess:  &Session{id: testSessionID, sess: session.New(), rt: rt},
		rt:    rt,
		out:   out,
		peer:  peer,
	}
}

// sessionUpdates parses the captured JSON-RPC notifications and returns the
// decoded session updates in emission order.
func (f *runAgentFixture) sessionUpdates(t *testing.T) []acpsdk.SessionUpdate {
	t.Helper()

	var updates []acpsdk.SessionUpdate
	for _, line := range f.out.lines() {
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &msg))
		require.Equal(t, "2.0", msg.JSONRPC)
		require.Equal(t, "session/update", msg.Method)

		var notification acpsdk.SessionNotification
		require.NoError(t, json.Unmarshal(msg.Params, &notification))
		require.Equal(t, acpsdk.SessionId(testSessionID), notification.SessionId)
		updates = append(updates, notification.Update)
	}
	return updates
}

func requireAvailableCommands(t *testing.T, update acpsdk.SessionUpdate) {
	t.Helper()

	require.NotNil(t, update.AvailableCommandsUpdate)
	names := make([]string, 0, len(update.AvailableCommandsUpdate.AvailableCommands))
	for _, cmd := range update.AvailableCommandsUpdate.AvailableCommands {
		names = append(names, cmd.Name)
	}
	assert.Equal(t, []string{"new", "compact", "usage"}, names)
}

func agentMessageText(t *testing.T, update acpsdk.SessionUpdate) string {
	t.Helper()

	require.NotNil(t, update.AgentMessageChunk)
	require.NotNil(t, update.AgentMessageChunk.Content.Text)
	return update.AgentMessageChunk.Content.Text.Text
}

func TestRunAgent_EmitsAvailableCommandsFirst(t *testing.T) {
	t.Parallel()

	f := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 1)
	requireAvailableCommands(t, updates[0])
}

func TestRunAgent_StreamsAssistantAndDiagnosticEvents(t *testing.T) {
	t.Parallel()

	rt := &fakeRuntime{events: []runtime.Event{
		runtime.AgentChoice("root", testSessionID, "Hello"),
		runtime.AgentChoiceReasoning("root", testSessionID, "pondering"),
		runtime.Error("boom"),
		runtime.Warning("careful", "root"),
		runtime.ModelFallback("root", "gpt-5", "gpt-4o", "rate limited", 1, 3),
	}}
	f := newRunAgentFixture(t, rt, &captureWriter{})

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 6)
	requireAvailableCommands(t, updates[0])

	assert.Equal(t, "Hello", agentMessageText(t, updates[1]))

	require.NotNil(t, updates[2].AgentThoughtChunk)
	require.NotNil(t, updates[2].AgentThoughtChunk.Content.Text)
	assert.Equal(t, "pondering", updates[2].AgentThoughtChunk.Content.Text.Text)

	assert.Equal(t, "\n\nError: boom\n", agentMessageText(t, updates[3]))
	assert.Equal(t, "\nWarning: careful\n", agentMessageText(t, updates[4]))
	assert.Equal(t, "\nModel gpt-5 failed, falling back to gpt-4o (rate limited)\n", agentMessageText(t, updates[5]))

	assert.Empty(t, rt.resumeRequests())
}

func TestRunAgent_SessionTitleUpdate(t *testing.T) {
	t.Parallel()

	rt := &fakeRuntime{events: []runtime.Event{
		runtime.SessionTitle(testSessionID, "Refactor plan"),
	}}
	f := newRunAgentFixture(t, rt, &captureWriter{})

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 2)
	require.NotNil(t, updates[1].SessionInfoUpdate)
	require.NotNil(t, updates[1].SessionInfoUpdate.Title)
	assert.Equal(t, "Refactor plan", *updates[1].SessionInfoUpdate.Title)
}

func TestRunAgent_TokenUsageUpdates(t *testing.T) {
	t.Parallel()

	t.Run("with cost", func(t *testing.T) {
		t.Parallel()

		rt := &fakeRuntime{events: []runtime.Event{
			runtime.NewTokenUsageEvent(testSessionID, "root", &runtime.Usage{
				ContextLength: 1234,
				ContextLimit:  200000,
				Cost:          0.75,
			}),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})

		require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

		updates := f.sessionUpdates(t)
		require.Len(t, updates, 2)
		require.NotNil(t, updates[1].UsageUpdate)
		assert.Equal(t, 200000, updates[1].UsageUpdate.Size)
		assert.Equal(t, 1234, updates[1].UsageUpdate.Used)
		require.NotNil(t, updates[1].UsageUpdate.Cost)
		assert.Equal(t, acpsdk.Cost{Amount: 0.75, Currency: "USD"}, *updates[1].UsageUpdate.Cost)
	})

	t.Run("without cost", func(t *testing.T) {
		t.Parallel()

		rt := &fakeRuntime{events: []runtime.Event{
			runtime.NewTokenUsageEvent(testSessionID, "root", &runtime.Usage{
				ContextLength: 42,
				ContextLimit:  1000,
			}),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})

		require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

		updates := f.sessionUpdates(t)
		require.Len(t, updates, 2)
		require.NotNil(t, updates[1].UsageUpdate)
		assert.Equal(t, 1000, updates[1].UsageUpdate.Size)
		assert.Equal(t, 42, updates[1].UsageUpdate.Used)
		assert.Nil(t, updates[1].UsageUpdate.Cost)
	})

	t.Run("nil usage emits nothing", func(t *testing.T) {
		t.Parallel()

		rt := &fakeRuntime{events: []runtime.Event{
			runtime.NewTokenUsageEvent(testSessionID, "root", nil),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})

		require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

		updates := f.sessionUpdates(t)
		require.Len(t, updates, 1)
		requireAvailableCommands(t, updates[0])
	})
}

func TestRunAgent_ToolCallLifecycle(t *testing.T) {
	t.Parallel()

	shellTool := tools.Tool{Name: "shell", Annotations: tools.ToolAnnotations{Title: "Run Shell"}}
	editTool := tools.Tool{Name: "edit_file"}
	editArgs := `{"path":"/tmp/f.go","edits":[{"oldText":"a","newText":"b"}]}`

	rt := &fakeRuntime{events: []runtime.Event{
		runtime.ToolCall(tools.ToolCall{
			ID:       "call-1",
			Function: tools.FunctionCall{Name: "shell", Arguments: `{"command":"ls","path":"/tmp"}`},
		}, shellTool, "root"),
		runtime.ToolCallResponse("call-1", shellTool, tools.ResultSuccess("file.txt"), "file.txt", "root"),
		runtime.ToolCall(tools.ToolCall{
			ID:       "call-2",
			Function: tools.FunctionCall{Name: "shell", Arguments: `{"command":"rm"}`},
		}, shellTool, "root"),
		runtime.ToolCallResponse("call-2", shellTool, tools.ResultError("denied"), "denied", "root"),
		runtime.ToolCall(tools.ToolCall{
			ID:       "call-3",
			Function: tools.FunctionCall{Name: "edit_file", Arguments: editArgs},
		}, editTool, "root"),
		runtime.ToolCallResponse("call-3", editTool, tools.ResultSuccess("ok"), "ok", "root"),
	}}
	f := newRunAgentFixture(t, rt, &captureWriter{})

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 7)
	requireAvailableCommands(t, updates[0])

	start := updates[1].ToolCall
	require.NotNil(t, start)
	assert.Equal(t, acpsdk.ToolCallId("call-1"), start.ToolCallId)
	assert.Equal(t, "Run Shell", start.Title)
	assert.Equal(t, acpsdk.ToolKindExecute, start.Kind)
	assert.Equal(t, acpsdk.ToolCallStatusPending, start.Status)
	assert.Equal(t, map[string]any{"command": "ls", "path": "/tmp"}, start.RawInput)
	assert.Equal(t, []acpsdk.ToolCallLocation{{Path: "/tmp"}}, start.Locations)

	completed := updates[2].ToolCallUpdate
	require.NotNil(t, completed)
	assert.Equal(t, acpsdk.ToolCallId("call-1"), completed.ToolCallId)
	require.NotNil(t, completed.Status)
	assert.Equal(t, acpsdk.ToolCallStatusCompleted, *completed.Status)
	require.Len(t, completed.Content, 1)
	require.NotNil(t, completed.Content[0].Content)
	require.NotNil(t, completed.Content[0].Content.Content.Text)
	assert.Equal(t, "file.txt", completed.Content[0].Content.Content.Text.Text)
	assert.Equal(t, map[string]any{"content": "file.txt"}, completed.RawOutput)

	require.NotNil(t, updates[3].ToolCall)
	failed := updates[4].ToolCallUpdate
	require.NotNil(t, failed)
	assert.Equal(t, acpsdk.ToolCallId("call-2"), failed.ToolCallId)
	require.NotNil(t, failed.Status)
	assert.Equal(t, acpsdk.ToolCallStatusFailed, *failed.Status)

	editStart := updates[5].ToolCall
	require.NotNil(t, editStart)
	assert.Equal(t, acpsdk.ToolKindEdit, editStart.Kind)

	editDone := updates[6].ToolCallUpdate
	require.NotNil(t, editDone)
	require.NotNil(t, editDone.Status)
	assert.Equal(t, acpsdk.ToolCallStatusCompleted, *editDone.Status)
	require.Len(t, editDone.Content, 1)
	diff := editDone.Content[0].Diff
	require.NotNil(t, diff)
	assert.Equal(t, "/tmp/f.go", diff.Path)
	assert.Equal(t, "b\n", diff.NewText)
	require.NotNil(t, diff.OldText)
	assert.Equal(t, "a\n", *diff.OldText)

	assert.Empty(t, rt.resumeRequests())
}

func TestRunAgent_ToolCallResponseWithoutStartFails(t *testing.T) {
	t.Parallel()

	rt := &fakeRuntime{events: []runtime.Event{
		runtime.ToolCallResponse("orphan", tools.Tool{Name: "shell"}, tools.ResultSuccess("ok"), "ok", "root"),
		runtime.AgentChoice("root", testSessionID, "never emitted"),
	}}
	f := newRunAgentFixture(t, rt, &captureWriter{})

	err := f.agent.runAgent(t.Context(), f.sess)
	require.EqualError(t, err, "missing tool call arguments for tool call ID orphan")

	// The failure must stop the loop before later events are mapped.
	updates := f.sessionUpdates(t)
	require.Len(t, updates, 1)
	requireAvailableCommands(t, updates[0])
}

func TestRunAgent_ToolCallConfirmationRequestFields(t *testing.T) {
	t.Parallel()

	tool := tools.Tool{Name: "shell", Annotations: tools.ToolAnnotations{Title: "Run Shell"}}
	call := tools.ToolCall{
		ID:       "confirm-1",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"command":"rm -rf /tmp/scratch"}`},
	}
	rt := &fakeRuntime{events: []runtime.Event{
		runtime.ToolCallConfirmation(call, tool, "root", nil),
		runtime.AgentChoice("root", testSessionID, "approved"),
	}}
	f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any {
		return permissionSelected("allow")
	})

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	reqs := f.peer.recordedRequests()
	require.Len(t, reqs, 1)
	req := reqs[0]
	assert.Equal(t, acpsdk.SessionId(testSessionID), req.SessionId)
	assert.Equal(t, acpsdk.ToolCallId("confirm-1"), req.ToolCall.ToolCallId)
	require.NotNil(t, req.ToolCall.Title)
	assert.Equal(t, "Run Shell", *req.ToolCall.Title)
	require.NotNil(t, req.ToolCall.Kind)
	assert.Equal(t, acpsdk.ToolKindExecute, *req.ToolCall.Kind)
	require.NotNil(t, req.ToolCall.Status)
	assert.Equal(t, acpsdk.ToolCallStatusPending, *req.ToolCall.Status)
	assert.Equal(t, map[string]any{"command": "rm -rf /tmp/scratch"}, req.ToolCall.RawInput)
	assert.Equal(t, []acpsdk.PermissionOption{
		{Kind: acpsdk.PermissionOptionKindAllowOnce, Name: "Allow this action", OptionId: "allow"},
		{Kind: acpsdk.PermissionOptionKindAllowAlways, Name: "Allow and remember my choice", OptionId: "allow-always"},
		{Kind: acpsdk.PermissionOptionKindRejectOnce, Name: "Skip this action", OptionId: "reject"},
	}, req.Options)

	assert.Equal(t, []runtime.ResumeRequest{{Type: runtime.ResumeTypeApprove}}, rt.resumeRequests())

	// The turn keeps streaming after the permission round trip and the
	// interleaved request does not disturb the captured notifications.
	updates := f.sessionUpdates(t)
	require.Len(t, updates, 2)
	requireAvailableCommands(t, updates[0])
	assert.Equal(t, "approved", agentMessageText(t, updates[1]))
}

func TestRunAgent_ToolCallConfirmationOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		result     any
		wantResume []runtime.ResumeRequest
	}{
		{
			name:       "allow approves once",
			result:     permissionSelected("allow"),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeApprove}},
		},
		{
			name:       "allow-always approves session",
			result:     permissionSelected("allow-always"),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeApproveAutonomous}},
		},
		{
			name:       "reject rejects",
			result:     permissionSelected("reject"),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeReject}},
		},
		{
			name:       "cancelled rejects",
			result:     permissionCancelled(),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeReject}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := &fakeRuntime{events: []runtime.Event{
				runtime.ToolCallConfirmation(
					tools.ToolCall{ID: "confirm-1", Function: tools.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}},
					tools.Tool{Name: "shell"},
					"root",
					nil,
				),
			}}
			f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any {
				return tt.result
			})

			require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

			assert.Equal(t, tt.wantResume, rt.resumeRequests())
			assert.Len(t, f.peer.recordedRequests(), 1)
		})
	}
}

func TestRunAgent_ToolCallConfirmationBadOutcomeFailsRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  any
		wantErr string
	}{
		{
			name:    "unexpected selected option",
			result:  permissionSelected("maybe"),
			wantErr: "unexpected permission option: maybe",
		},
		{
			// An empty result leaves both outcome union variants nil.
			name:    "missing outcome",
			result:  map[string]any{},
			wantErr: "unexpected permission outcome",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := &fakeRuntime{events: []runtime.Event{
				runtime.ToolCallConfirmation(
					tools.ToolCall{ID: "confirm-1", Function: tools.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}},
					tools.Tool{Name: "shell"},
					"root",
					nil,
				),
				runtime.AgentChoice("root", testSessionID, "never emitted"),
			}}
			f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any {
				return tt.result
			})

			err := f.agent.runAgent(t.Context(), f.sess)
			require.EqualError(t, err, tt.wantErr)

			assert.Empty(t, rt.resumeRequests())
			// The failure must stop the loop before later events are mapped.
			updates := f.sessionUpdates(t)
			require.Len(t, updates, 1)
			requireAvailableCommands(t, updates[0])
		})
	}
}

func TestRunAgent_MaxIterationsReachedRequestFields(t *testing.T) {
	t.Parallel()

	rt := &fakeRuntime{events: []runtime.Event{
		runtime.MaxIterationsReached(25),
	}}
	f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any {
		return permissionSelected("continue")
	})

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	reqs := f.peer.recordedRequests()
	require.Len(t, reqs, 1)
	req := reqs[0]
	assert.Equal(t, acpsdk.SessionId(testSessionID), req.SessionId)
	assert.Equal(t, acpsdk.ToolCallId("max_iterations"), req.ToolCall.ToolCallId)
	require.NotNil(t, req.ToolCall.Title)
	assert.Equal(t, "Maximum iterations (25) reached", *req.ToolCall.Title)
	require.NotNil(t, req.ToolCall.Kind)
	assert.Equal(t, acpsdk.ToolKindExecute, *req.ToolCall.Kind)
	require.NotNil(t, req.ToolCall.Status)
	assert.Equal(t, acpsdk.ToolCallStatusPending, *req.ToolCall.Status)
	assert.Nil(t, req.ToolCall.RawInput)
	assert.Equal(t, []acpsdk.PermissionOption{
		{Kind: acpsdk.PermissionOptionKindAllowOnce, Name: "Continue", OptionId: "continue"},
		{Kind: acpsdk.PermissionOptionKindRejectOnce, Name: "Stop", OptionId: "stop"},
	}, req.Options)

	assert.Equal(t, []runtime.ResumeRequest{{Type: runtime.ResumeTypeApprove}}, rt.resumeRequests())
}

func TestRunAgent_MaxIterationsReachedOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		result     any
		wantResume []runtime.ResumeRequest
	}{
		{
			name:       "continue approves",
			result:     permissionSelected("continue"),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeApprove}},
		},
		{
			name:       "stop rejects",
			result:     permissionSelected("stop"),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeReject}},
		},
		{
			name:       "cancelled rejects",
			result:     permissionCancelled(),
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeReject}},
		},
		{
			// Unlike tool confirmations, a missing selection rejects
			// instead of failing the run.
			name:       "missing selection rejects",
			result:     map[string]any{},
			wantResume: []runtime.ResumeRequest{{Type: runtime.ResumeTypeReject}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := &fakeRuntime{events: []runtime.Event{runtime.MaxIterationsReached(3)}}
			f := newRunAgentFixtureWithPermissions(t, rt, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any {
				return tt.result
			})

			require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

			assert.Equal(t, tt.wantResume, rt.resumeRequests())
			assert.Len(t, f.peer.recordedRequests(), 1)
		})
	}
}

func TestRunAgent_TodoToolEmitsPlanUpdate(t *testing.T) {
	t.Parallel()

	todoTool := tools.Tool{Name: todo.ToolNameCreateTodos}
	todoCall := tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: todo.ToolNameCreateTodos, Arguments: `{"descriptions":["write tests"]}`},
	}

	t.Run("todo metadata becomes a plan", func(t *testing.T) {
		t.Parallel()

		result := &tools.ToolCallResult{
			Output: "ok",
			Meta: []todo.Todo{
				{ID: "1", Description: "write tests", Status: "in-progress"},
				{ID: "2", Description: "review", Status: "pending"},
			},
		}
		rt := &fakeRuntime{events: []runtime.Event{
			runtime.ToolCall(todoCall, todoTool, "root"),
			runtime.ToolCallResponse("call-1", todoTool, result, "ok", "root"),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})

		require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

		updates := f.sessionUpdates(t)
		require.Len(t, updates, 4)
		require.NotNil(t, updates[1].ToolCall)
		require.NotNil(t, updates[2].ToolCallUpdate)

		plan := updates[3].Plan
		require.NotNil(t, plan)
		assert.Equal(t, []acpsdk.PlanEntry{
			{Content: "write tests", Status: acpsdk.PlanEntryStatusInProgress, Priority: acpsdk.PlanEntryPriorityMedium},
			{Content: "review", Status: acpsdk.PlanEntryStatusPending, Priority: acpsdk.PlanEntryPriorityMedium},
		}, plan.Entries)
	})

	t.Run("unexpected metadata emits no plan", func(t *testing.T) {
		t.Parallel()

		result := &tools.ToolCallResult{Output: "ok", Meta: "not-todos"}
		rt := &fakeRuntime{events: []runtime.Event{
			runtime.ToolCall(todoCall, todoTool, "root"),
			runtime.ToolCallResponse("call-1", todoTool, result, "ok", "root"),
		}}
		f := newRunAgentFixture(t, rt, &captureWriter{})

		require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

		updates := f.sessionUpdates(t)
		require.Len(t, updates, 3)
		assert.Nil(t, updates[2].Plan)
	})
}

func TestRunAgent_SendUpdateFailureStopsRun(t *testing.T) {
	t.Parallel()

	out := &captureWriter{failOn: func(n int) error {
		if n >= 2 {
			return errors.New("peer gone")
		}
		return nil
	}}
	rt := &fakeRuntime{events: []runtime.Event{
		runtime.AgentChoice("root", testSessionID, "one"),
		runtime.AgentChoice("root", testSessionID, "two"),
	}}
	f := newRunAgentFixture(t, rt, out)

	err := f.agent.runAgent(t.Context(), f.sess)
	require.ErrorContains(t, err, "peer gone")

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 1)
	requireAvailableCommands(t, updates[0])
}

func TestRunAgent_AvailableCommandsFailureIsNonFatal(t *testing.T) {
	t.Parallel()

	out := &captureWriter{failOn: func(n int) error {
		if n == 1 {
			return errors.New("transient failure")
		}
		return nil
	}}
	rt := &fakeRuntime{events: []runtime.Event{
		runtime.AgentChoice("root", testSessionID, "hello"),
	}}
	f := newRunAgentFixture(t, rt, out)

	require.NoError(t, f.agent.runAgent(t.Context(), f.sess))

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 1)
	assert.Equal(t, "hello", agentMessageText(t, updates[0]))
}

func TestRunAgent_ContextCancellationStopsEventLoop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	rt := &fakeRuntime{events: []runtime.Event{
		runtime.AgentChoice("root", testSessionID, "never emitted"),
	}}
	// Cancel the turn after available commands were emitted but before the
	// first event is consumed.
	rt.onRunStream = cancel
	f := newRunAgentFixture(t, rt, &captureWriter{})

	err := f.agent.runAgent(ctx, f.sess)
	require.ErrorIs(t, err, context.Canceled)

	updates := f.sessionUpdates(t)
	require.Len(t, updates, 1)
	requireAvailableCommands(t, updates[0])
	assert.Empty(t, rt.resumeRequests())
}
