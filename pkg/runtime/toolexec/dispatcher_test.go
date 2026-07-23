package toolexec_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// captureEmitter records every event the dispatcher emits, in order, so
// tests can make precise assertions about the dispatch flow. A confirm
// channel signals when a confirmation event lands so cancellation tests
// don't need to busy-wait.
type captureEmitter struct {
	mu               sync.Mutex
	confirmedOnce    sync.Once
	calls            []tools.ToolCall
	outputs          []outputRecord
	responses        []responseRecord
	confirmations    []tools.ToolCall
	confirmationMeta []map[string]string
	hookBlocks       []hookBlockRecord
	messages         []*session.Message
	confirmed        chan struct{} // optional; closed after the first confirmation event
}

type outputRecord struct {
	ToolCallID string
	Output     string
}

type responseRecord struct {
	ToolCallID string
	Output     string
	IsError    bool
}

type hookBlockRecord struct {
	ToolCall tools.ToolCall
	Message  string
}

func (e *captureEmitter) EmitToolCall(tc tools.ToolCall, _ tools.Tool, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, tc)
}

func (e *captureEmitter) EmitToolCallOutput(toolCallID string, _ tools.Tool, output, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.outputs = append(e.outputs, outputRecord{ToolCallID: toolCallID, Output: output})
}

func (e *captureEmitter) EmitToolCallResponse(toolCallID string, _ tools.Tool, result *tools.ToolCallResult, output, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.responses = append(e.responses, responseRecord{
		ToolCallID: toolCallID,
		Output:     output,
		IsError:    result.IsError,
	})
}

func (e *captureEmitter) EmitToolCallConfirmation(tc tools.ToolCall, _ tools.Tool, _ string, metadata map[string]string) {
	e.mu.Lock()
	e.confirmations = append(e.confirmations, tc)
	e.confirmationMeta = append(e.confirmationMeta, metadata)
	e.mu.Unlock()
	if e.confirmed != nil {
		e.confirmedOnce.Do(func() { close(e.confirmed) })
	}
}

func (e *captureEmitter) EmitHookBlocked(tc tools.ToolCall, _ tools.Tool, message, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hookBlocks = append(e.hookBlocks, hookBlockRecord{ToolCall: tc, Message: message})
}

func (e *captureEmitter) EmitMessageAdded(_ string, msg *session.Message, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.messages = append(e.messages, msg)
}

func newAgent() *agent.Agent {
	return agent.New("test", "test agent")
}

func TestDispatcher_RoutesToToolsetHandler(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous) // skip approval so we exercise the happy path

	var handlerCalls int
	tool := tools.Tool{
		Name: "echo",
		Handler: func(_ context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			handlerCalls++
			return tools.ResultSuccess("hello " + tc.Function.Arguments), nil
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "call_1",
		Function: tools.FunctionCall{Name: "echo", Arguments: `{"x":1}`},
	}}, []tools.Tool{tool}, em)

	assert.Equal(t, 1, handlerCalls)
	require.Len(t, em.responses, 1)
	assert.Equal(t, `hello {"x":1}`, em.responses[0].Output)
	assert.False(t, em.responses[0].IsError)
}

func TestDispatcher_RunsToolHandlersInParallel(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	started := make(chan string, 2)
	release := make(chan struct{})
	var running atomic.Int32
	var maxRunning atomic.Int32
	tool := tools.Tool{
		Name: "slow",
		Handler: func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			current := running.Add(1)
			for {
				observed := maxRunning.Load()
				if current <= observed || maxRunning.CompareAndSwap(observed, current) {
					break
				}
			}
			started <- tc.ID
			defer running.Add(-1)
			select {
			case <-release:
				return tools.ResultSuccess("done " + tc.ID), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	d := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }}
	em := &captureEmitter{}
	done := make(chan struct{})
	go func() {
		d.Process(t.Context(), sess, []tools.ToolCall{
			{ID: "a", Function: tools.FunctionCall{Name: "slow", Arguments: `{}`}},
			{ID: "b", Function: tools.FunctionCall{Name: "slow", Arguments: `{}`}},
		}, []tools.Tool{tool}, em)
		close(done)
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for both tool handlers to start")
		}
	}
	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Process to finish")
	}

	assert.GreaterOrEqual(t, maxRunning.Load(), int32(2))
	require.Len(t, em.responses, 2)
}

func TestDispatcher_EmitsToolOutputFromHandlerContext(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	tool := tools.Tool{
		Name: "streamer",
		Handler: func(ctx context.Context, _ tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
			rt.EmitOutput(ctx, "first\n")
			rt.EmitOutput(ctx, "second\n")
			return tools.ResultSuccess("done"), nil
		},
	}

	d := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }}
	em := &captureEmitter{}
	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "call_stream",
		Function: tools.FunctionCall{Name: "streamer", Arguments: `{}`},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.outputs, 2)
	assert.Equal(t, outputRecord{ToolCallID: "call_stream", Output: "first\n"}, em.outputs[0])
	assert.Equal(t, outputRecord{ToolCallID: "call_stream", Output: "second\n"}, em.outputs[1])
	require.Len(t, em.responses, 1)
	assert.Equal(t, "done", em.responses[0].Output)
}

func TestDispatcher_RecordsDocumentToolResult(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	tool := tools.Tool{
		Name: "report",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return &tools.ToolCallResult{
				Output: "created report",
				Documents: []tools.DocumentContent{{
					Name:     "report.pdf",
					URI:      "file:///report.pdf",
					MimeType: "application/pdf",
					Data:     "UERG",
				}},
			}, nil
		},
	}

	d := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }}
	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "call_report",
		Function: tools.FunctionCall{Name: "report", Arguments: `{}`},
	}}, []tools.Tool{tool}, &captureEmitter{})

	require.Len(t, sess.Messages, 1)
	msg := sess.Messages[0].Message.Message
	require.Len(t, msg.MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeText, msg.MultiContent[0].Type)
	assert.Equal(t, "created report", msg.MultiContent[0].Text)
	require.NotNil(t, msg.MultiContent[1].Document)
	assert.Equal(t, chat.MessagePartTypeDocument, msg.MultiContent[1].Type)
	assert.Equal(t, "report.pdf", msg.MultiContent[1].Document.Name)
	assert.Equal(t, "application/pdf", msg.MultiContent[1].Document.MimeType)
	assert.Equal(t, []byte("PDF"), msg.MultiContent[1].Document.Source.InlineData)
}

func TestDispatcher_RoutesToRuntimeHandler(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	var handlerCalls int
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Handlers: map[string]toolexec.ToolHandler{
			"transfer_task": func(_ context.Context, _ *session.Session, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
				handlerCalls++
				return tools.ResultSuccess("transferred"), nil
			},
		},
	}
	em := &captureEmitter{}

	// Toolset handler must NOT be called when a runtime handler is registered
	// for the same name.
	tool := tools.Tool{
		Name: "transfer_task",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			t.Fatal("toolset handler must not be called when runtime handler exists")
			return nil, nil
		},
	}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "call_t",
		Function: tools.FunctionCall{Name: "transfer_task", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.Equal(t, 1, handlerCalls)
	require.Len(t, em.responses, 1)
	assert.Equal(t, "transferred", em.responses[0].Output)
}

func TestDispatcher_UnknownToolEmitsErrorResponse(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "ghost",
		Function: tools.FunctionCall{Name: "non_existent", Arguments: "{}"},
	}}, []tools.Tool{}, em)

	require.Len(t, em.responses, 1)
	assert.Equal(t, "ghost", em.responses[0].ToolCallID)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "not available")
}

func TestDispatcher_UserCancellationStopsBatchAndErrorsAllCalls(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{confirmed: make(chan struct{})}

	tool := tools.Tool{
		Name:     "shell",
		Category: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	// Cancel as soon as the dispatcher asks for confirmation on the first
	// call. Every call in the batch must receive an error response so the
	// conversation history stays consistent (the Responses API rejects
	// orphaned tool calls).
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		<-em.confirmed
		cancel()
	}()

	calls := []tools.ToolCall{
		{ID: "a", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}},
		{ID: "b", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}},
		{ID: "c", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}},
	}
	d.Process(ctx, sess, calls, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 3, "every call must produce a response")
	for _, r := range em.responses {
		assert.True(t, r.IsError, "every cancelled call must surface as an error response")
		assert.Contains(t, r.Output, "canceled")
	}
}

func TestDispatcher_ResumeApproveRunsTool(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	var ran bool
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("done"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	// Pre-approve via the resume channel before invoking Process.
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran)
	require.Len(t, em.responses, 1)
	assert.False(t, em.responses[0].IsError)
	assert.Equal(t, "done", em.responses[0].Output)
}

func TestDispatcher_ResumeRejectEmitsErrorResponseWithReason(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject, Reason: "wrong arguments"}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "user rejected")
	assert.Contains(t, em.responses[0].Output, "wrong arguments")
}

func TestDispatcher_ResumeApproveBalancedFlipsSessionToBalanced(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	var ran bool
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApproveBalanced}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran, "approve-balanced must run the pending tool")
	assert.Equal(t, session.SafetyPolicyBalanced, sess.SafetyPolicy,
		"approve-balanced must persist balanced on the session")
	assert.False(t, sess.ToolsApproved, "balanced must not backfill ToolsApproved")
}

func TestDispatcher_ResumeApproveToolPersistsToSessionPermissions(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	var ran bool
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApproveTool, ToolName: "shell"}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran)
	require.NotNil(t, sess.Permissions, "approve-tool must seed session permissions")
	assert.Contains(t, sess.Permissions.Allow, "shell")
}

// Under Balanced mode, a ReadOnlyHint tool auto-approves without
// prompting: mode(balanced) × label(safe) = allow.
func TestDispatcher_ReadOnlyHintAutoApprovesUnderBalanced(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))

	var ran bool
	tool := tools.Tool{
		Name: "read_file",
		Annotations: tools.ToolAnnotations{
			ReadOnlyHint: true,
		},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("contents"), nil
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "r",
		Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran)
	assert.Empty(t, em.confirmations, "balanced + read-only must not prompt the user")
	require.Len(t, em.responses, 1)
	assert.False(t, em.responses[0].IsError)
}

// A batch of shell calls the classifier can't recognise (label =
// unknown): user picks approve-tool with a per-command shell pattern
// on the first prompt. Subsequent sibling calls that match the same
// pattern should auto-approve without emitting a second confirmation.
// Mirrors Gordon in prod where most commands (docker build, npm
// install, mise run …) fall into the unknown bucket and users grant
// per-command allow rules from the confirmation card.
func TestDispatcher_ApproveToolAutoApprovesSiblingCallsMatchingPattern(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	// safer_shell can't classify these — falls back to unknown, which
	// under Balanced/Strict would keep asking. approve-tool is the
	// escape hatch.
	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:  true,
				Decision: hooks.DecisionAllow,
				Metadata: map[string]string{"blast_radius": "unknown"},
			},
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
		Resume:   resume,
		// Real runtimes expose the session's own Permissions here (see
		// pkg/runtime/tool_dispatch.go). Without it, approve-tool's
		// session-level allow rule never reaches Decide().
		Permissions: func(s *session.Session) []toolexec.NamedChecker {
			perms := s.ClonePermissions()
			if perms == nil {
				return nil
			}
			return []toolexec.NamedChecker{{
				Checker: permissions.NewCheckerFromRules(perms.Allow, perms.Ask, perms.Deny),
				Source:  "session permissions",
			}}
		},
	}
	em := &captureEmitter{confirmed: make(chan struct{})}

	go func() {
		<-em.confirmed
		resume <- toolexec.ResumeRequest{
			Type:     toolexec.ResumeTypeApproveTool,
			ToolName: "shell:cmd=docker*",
		}
	}()

	d.Process(t.Context(), sess, []tools.ToolCall{
		{ID: "a", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"docker ps"}`}},
		{ID: "b", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"docker images"}`}},
		{ID: "c", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"docker build ."}`}},
	}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 3)
	for i, r := range em.responses {
		assert.Falsef(t, r.IsError, "response %d must not be an error", i)
	}
	assert.Len(t, em.confirmations, 1,
		"approve-tool with a shell pattern must auto-approve every matching sibling")
}

// A batch of safe shell calls under Strict, with the safer_shell
// classifier wired in: user approve-balanced on the first prompt
// should auto-approve every remaining safe shell call in the batch.
// Mirrors production Gordon where the classifier tags each shell
// call with blast_radius via the preempt-yolo lane.
func TestDispatcher_ApproveBalancedAutoApprovesSiblingSafeShellCalls(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	// safer_shell always returns Allow + blast_radius=safe for safe
	// commands under the new pure-labeller design.
	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:  true,
				Decision: hooks.DecisionAllow,
				Metadata: map[string]string{"blast_radius": "safe"},
			},
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
		Resume:   resume,
	}
	em := &captureEmitter{confirmed: make(chan struct{})}

	go func() {
		<-em.confirmed
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApproveBalanced}
	}()

	d.Process(t.Context(), sess, []tools.ToolCall{
		{ID: "a", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}},
		{ID: "b", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"pwd"}`}},
		{ID: "c", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"git status"}`}},
	}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 3)
	for i, r := range em.responses {
		assert.Falsef(t, r.IsError, "response %d must not be an error", i)
	}
	assert.Len(t, em.confirmations, 1,
		"only the first call should prompt; balanced flip must auto-approve safe siblings")
}

// Under Strict (default), ReadOnlyHint tools bypass the prompt —
// preserves the pre-three-mode default of auto-approving read-only
// calls so headless callers ({--exec}, MCP, A2A) don't regress.
func TestDispatcher_ReadOnlyHintAutoApprovesUnderStrict(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New() // empty policy ≡ strict

	var ran atomic.Int32
	tool := tools.Tool{
		Name: "read_file",
		Annotations: tools.ToolAnnotations{
			ReadOnlyHint: true,
		},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran.Add(1)
			return tools.ResultSuccess("contents"), nil
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "r",
		Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.Equal(t, int32(1), ran.Load(), "read-only tool must run")
	assert.Empty(t, em.confirmations, "read-only tool must not prompt under Strict")
}

// DestructiveHint on a tool that reaches the confirmation event must
// surface blast_radius=high metadata so the UI can render a warning
// tier without duplicating the classification logic.
func TestDispatcher_DestructiveHintEmitsHighBlastRadiusMetadata(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	destructive := true
	tool := tools.Tool{
		Name: "delete_task",
		Annotations: tools.ToolAnnotations{
			DestructiveHint: &destructive,
		},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "delete_task", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1)
	require.Len(t, em.confirmationMeta, 1)
	assert.Equal(t, "high", em.confirmationMeta[0]["blast_radius"])
}

// A tool without ReadOnlyHint / DestructiveHint must not leak a
// blast_radius verdict into the confirmation event.
func TestDispatcher_UnannotatedToolEmitsNoBlastRadius(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "custom_tool",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "custom_tool", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1)
	if len(em.confirmationMeta) > 0 {
		_, present := em.confirmationMeta[0]["blast_radius"]
		assert.False(t, present, "unannotated tools must not emit blast_radius")
	}
}

// Under Autonomous mode, tools auto-approve without prompting even
// when marked destructive: the mode wins over the classifier's
// label. The user opted in explicitly.
func TestDispatcher_AutonomousAutoApprovesDestructiveTools(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))

	destructive := true
	var ran bool
	tool := tools.Tool{
		Name: "delete_task",
		Annotations: tools.ToolAnnotations{
			DestructiveHint: &destructive,
		},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "delete_task", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran, "autonomous must auto-approve every tool")
	assert.Empty(t, em.confirmations)
}

// Custom ask: pattern beats Autonomous — new behaviour under the
// mode model.
func TestDispatcher_CustomAskBeatsAutonomous(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run without confirmation")
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
		Permissions: func(*session.Session) []toolexec.NamedChecker {
			return []toolexec.NamedChecker{{
				Checker: permissions.NewCheckerFromRules(nil, []string{"shell"}, nil),
				Source:  toolexec.SessionPermissionsSource,
			}}
		},
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1,
		"session-level ask must prompt even under autonomous")
}

// Balanced mode prompts on destructive tools (mode × destructive = ask).
func TestDispatcher_BalancedPromptsForDestructiveHintTools(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))

	destructive := true
	tool := tools.Tool{
		Name: "delete_task",
		Annotations: tools.ToolAnnotations{
			DestructiveHint: &destructive,
		},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "delete_task", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1)
}

// Balanced mode also prompts on unknown (readOnlyHint=false, no
// destructive hint) tools.
func TestDispatcher_BalancedPromptsForUnknownTools(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))

	tool := tools.Tool{
		Name: "custom_tool",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "custom_tool", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1,
		"balanced + unknown tool must prompt the user")
}

func TestDispatcher_ResumeApproveAutonomousFlipsSessionToAutonomous(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	var ran bool
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApproveAutonomous}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.True(t, sess.ToolsApproved,
		"approve-autonomous must backfill ToolsApproved for legacy branches")
}

func TestDispatcher_DenyByPermissionsEmitsErrorResponse(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Permissions: func(*session.Session) []toolexec.NamedChecker {
			return []toolexec.NamedChecker{
				{Checker: newDenyChecker("shell"), Source: "test policy"},
			}
		},
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "denied by test policy")
}

// TestDispatcher_ToolResponseTransformRewritesOutput pins the contract
// of the new tool_response_transform hook: when a configured hook
// returns HookSpecificOutput.UpdatedToolResponse, the dispatcher
// applies it BEFORE event emission, the recorded chat message, and
// the post_tool_use hook input. This is the third leg of the
// redact_secrets feature — unit-tested here at the contract level
// with a stub HookDispatcher so the test doesn't depend on the
// portcullis ruleset shipping a particular set of patterns.
func TestDispatcher_ToolResponseTransformRewritesOutput(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	original := "raw output with a secret"
	rewritten := "output with [REDACTED]"

	tool := tools.Tool{
		Name:     "leaky",
		Category: "filesystem",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess(original), nil
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventToolResponseTransform: {Allowed: true, UpdatedToolResponse: &rewritten},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "r",
		Function: tools.FunctionCall{Name: "leaky", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.Equal(t, rewritten, em.responses[0].Output,
		"emitted response must carry the rewritten payload")
	require.Len(t, em.messages, 1)
	assert.Equal(t, rewritten, em.messages[0].Message.Content,
		"recorded chat message must carry the rewritten payload")

	// post_tool_use must see the rewritten payload in its Input —
	// proves the rewrite happens before the post-hook fires, not
	// after.
	require.NotNil(t, hd.lastPostToolInput, "post_tool_use must have been dispatched")
	assert.Equal(t, rewritten, hd.lastPostToolInput.ToolResponse,
		"post_tool_use input must reflect the rewrite")
}

// TestDispatcher_ToolResponseTransformIsNoOpWithoutHooks pins the
// opt-in semantics: with no Hooks dispatcher (or with a hook that
// returns nil for the event), the original output flows through
// untouched and no surprise allocations happen.
func TestDispatcher_ToolResponseTransformIsNoOpWithoutHooks(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	original := "untouched output"

	tool := tools.Tool{
		Name:     "leaky",
		Category: "filesystem",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess(original), nil
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		// Hooks deliberately nil.
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "r",
		Function: tools.FunctionCall{Name: "leaky", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.Equal(t, original, em.responses[0].Output)
	require.Len(t, em.messages, 1)
	assert.Equal(t, original, em.messages[0].Message.Content)
}

// TestDispatcher_ToolResponseTransformAppliesToErrorResponse covers
// the synthesised-error path: rejection / hook-block / cancellation
// messages also flow through tool_response_transform so a configured
// scrubber sees the same payload the runtime would otherwise emit.
// Without this, a permission_request hook that quoted a secret in its
// rejection reason would leak — errorResponse used to bypass the
// rewrite chain entirely.
func TestDispatcher_ToolResponseTransformAppliesToErrorResponse(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	rewritten := "rejected with [REDACTED] secret"
	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventToolResponseTransform: {Allowed: true, UpdatedToolResponse: &rewritten},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
		Permissions: func(*session.Session) []toolexec.NamedChecker {
			return []toolexec.NamedChecker{
				{Checker: newDenyChecker("shell"), Source: "test policy"},
			}
		},
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Equal(t, rewritten, em.responses[0].Output,
		"synthesised error response must also flow through the transform")
}

// TestDispatcher_ConfirmationCarriesToolMetadata verifies that a
// toolset's static [tools.Tool.Metadata] reaches the tool-call
// confirmation event when the user is prompted.
func TestDispatcher_ConfirmationCarriesToolMetadata(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name:     "shell",
		Metadata: map[string]string{"danger": "high", "category": "exec"},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1)
	require.Len(t, em.confirmationMeta, 1)
	assert.Equal(t, map[string]string{"danger": "high", "category": "exec"}, em.confirmationMeta[0])
}

// TestDispatcher_ConfirmationMergesHookMetadata verifies that a
// permission_request hook can enrich the confirmation metadata and that
// hook keys win over the toolset's own keys on a clash.
func TestDispatcher_ConfirmationMergesHookMetadata(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name:     "shell",
		Metadata: map[string]string{"danger": "high", "source": "toolset"},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	// Hook allows the prompt to proceed (no short-circuit) but contributes
	// metadata, overriding the toolset's "source" key.
	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPermissionRequest: {
				Allowed:  true,
				Metadata: map[string]string{"source": "hook", "reason": "policy-x"},
			},
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmationMeta, 1)
	assert.Equal(t, map[string]string{
		"danger": "high",
		"source": "hook",
		"reason": "policy-x",
	}, em.confirmationMeta[0])
}

// TestDispatcher_ConfirmationMetadataNilWhenNoneSupplied verifies that
// the confirmation event carries nil metadata when neither the toolset
// nor a hook contributed any, keeping the wire payload lean.
func TestDispatcher_ConfirmationMetadataNilWhenNoneSupplied(t *testing.T) {
	t.Parallel()
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run")
		},
	}

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{}

	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: "{}"},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmationMeta, 1)
	assert.Nil(t, em.confirmationMeta[0])
}

// TestDispatcher_PreToolUsePreYoloAskPreemptsYolo pins the load-
// bearing property of the preempt-yolo lane of pre_tool_use: an Ask
// verdict must route to user confirmation even when ToolsApproved
// (--yolo / Allow All) would otherwise auto-run the call. Also
// verifies the hook's Metadata reaches the confirmation event.
func TestDispatcher_PreToolUsePreYoloAskPreemptsYolo(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run before approval")
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:        true,
				Decision:       hooks.DecisionAsk,
				DecisionReason: "rm -rf <path>: irreversible",
				Metadata: map[string]string{
					"blast_radius": "high",
					"category":     "fs-delete",
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{confirmed: make(chan struct{})}
	go func() {
		<-em.confirmed
		cancel()
	}()

	d.Process(ctx, sess, []tools.ToolCall{{
		ID:       "danger",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rm -rf /tmp/x"}`},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1, "Ask must route to user confirmation despite --yolo")
	require.Len(t, em.confirmationMeta, 1)
	assert.Equal(t, "high", em.confirmationMeta[0]["blast_radius"],
		"preempt-yolo hook Metadata must reach the confirmation event")
	assert.Equal(t, "fs-delete", em.confirmationMeta[0]["category"])
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "canceled by the user")
}

// TestDispatcher_PreToolUsePreYoloAskHonorsSessionAllow pins the fix
// for the "always allow this tool" grant being ignored under a
// preempt-yolo hook: a session-scoped allow pattern (the interactive
// "T" decision, stored in sess.Permissions) must auto-run a matching
// call even when the preempt-yolo lane returns Ask. Without this the
// user is re-prompted on every matching call despite having chosen
// "always allow".
func TestDispatcher_PreToolUsePreYoloAskHonorsSessionAllow(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New()
	// The exact pattern BuildPermissionPattern produces for a "T" grant
	// on `mkdir foo`.
	sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=mkdir*"}}

	ran := false
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:        true,
				Decision:       hooks.DecisionAsk,
				DecisionReason: "unknown command",
			},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"mkdir bar"}`},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran, "session allow grant must auto-run despite preempt-yolo Ask")
	assert.Empty(t, em.confirmations, "an informed 'always allow' grant must not re-prompt")
}

// TestDispatcher_PreToolUsePreYoloAskSessionAllowRefusesCompound pins
// the strict reading of the session grant on the safety-override path:
// a "T = always allow mkdir*" grant is a plain prefix pattern in the
// generic matcher and would also cover "mkdir x && rm -rf ~" — a
// compound command that smuggles a destructive call behind the
// approved word. Overriding a preempt-yolo Ask therefore requires the
// word-boundary, no-metacharacter reading (shellGrantCoversCommand):
// the compound call must still prompt.
func TestDispatcher_PreToolUsePreYoloAskSessionAllowRefusesCompound(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New()
	sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=mkdir*"}}

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run: compound command must not be covered by the mkdir* grant")
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:        true,
				Decision:       hooks.DecisionAsk,
				DecisionReason: "rm -rf <path>: irreversible",
				Metadata:       map[string]string{"blast_radius": "high"},
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{confirmed: make(chan struct{})}
	go func() {
		<-em.confirmed
		cancel()
	}()

	d.Process(ctx, sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"mkdir x && rm -rf ~"}`},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1,
		"a prefix grant must not silence the safety Ask for a compound command")
}

// TestDispatcher_PreToolUsePreYoloAskGuardsTeamAllow pins the boundary
// of the fix: a matching allow pattern in the TEAM/config layer (the
// dispatcher's Permissions checker, e.g. global settings or agent YAML)
// must NOT override a preempt-yolo Ask — that ahead-of-time,
// non-interactive layer is exactly what the safety lane keeps guarding.
// Only the session-scoped layer (interactive grants) overrides.
func TestDispatcher_PreToolUsePreYoloAskGuardsTeamAllow(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New() // no session-scoped permissions

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run: team allow must stay subordinate to preempt-yolo")
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:        true,
				Decision:       hooks.DecisionAsk,
				DecisionReason: "unknown command",
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
		// A team/config-layer allow that matches the call. It must be
		// ignored on the preempt-yolo Ask path.
		Permissions: func(*session.Session) []toolexec.NamedChecker {
			return []toolexec.NamedChecker{{
				Checker: permissions.NewCheckerFromRules([]string{"shell:cmd=mkdir*"}, nil, nil),
				Source:  "permissions configuration",
			}}
		},
	}
	em := &captureEmitter{confirmed: make(chan struct{})}
	go func() {
		<-em.confirmed
		cancel()
	}()

	d.Process(ctx, sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"mkdir bar"}`},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.confirmations, 1, "team/config allow must not override preempt-yolo Ask")
}

// TestDispatcher_PreToolUsePreYoloDenyShortCircuits pins the Deny
// path: a preempt-yolo Deny verdict goes straight to errorResponse
// without emitting a confirmation event or consulting
// permission_request.
func TestDispatcher_PreToolUsePreYoloDenyShortCircuits(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run when denied")
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:        false,
				Decision:       hooks.DecisionDeny,
				DecisionReason: "blocked by policy",
			},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rm -rf /tmp/x"}`},
	}}, []tools.Tool{tool}, em)

	assert.Empty(t, em.confirmations, "Deny must not emit a confirmation event")
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "pre_tool_use hook")
	assert.Contains(t, em.responses[0].Output, "blocked by policy")
}

// TestDispatcher_PreToolUsePreYoloAllowIsAdvisory pins the Allow
// path: an Allow verdict from the preempt-yolo lane is informational,
// the pipeline still proceeds. With ToolsApproved=true the call must
// auto-run.
func TestDispatcher_PreToolUsePreYoloAllowIsAdvisory(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	ran := false
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:  true,
				Decision: hooks.DecisionAllow,
			},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls /tmp"}`},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran, "Allow from preempt-yolo lane is advisory; --yolo must still allow the call")
	assert.Empty(t, em.confirmations)
}

// TestDispatcher_PreToolUseDefaultLaneSkippedUnderYolo pins the
// backwards-compat contract for default-lane pre_tool_use hooks: when
// --yolo (ToolsApproved) auto-approves the call, the default lane is
// NOT consulted. This protects existing pre_tool_use hooks
// (llm_judge, redact_secrets) from a latency tax on every yolo'd
// call — only entries that explicitly opt into preempt_yolo:true run
// before Decide().
func TestDispatcher_PreToolUseDefaultLaneSkippedUnderYolo(t *testing.T) {
	t.Parallel()

	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)

	ran := false
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			ran = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	// A default-lane pre_tool_use hook that WOULD ask the user — but
	// must not be consulted because --yolo auto-approved upstream.
	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUse: {
				Allowed:        true,
				Decision:       hooks.DecisionAsk,
				DecisionReason: "would normally ask",
			},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls /tmp"}`},
	}}, []tools.Tool{tool}, em)

	assert.True(t, ran, "--yolo must auto-run when no preempt-yolo hook is registered")
	assert.NotContains(t, hd.dispatched, hooks.EventPreToolUse,
		"default pre_tool_use lane must not fire under --yolo; only preempt_yolo:true entries do")
	assert.Empty(t, em.confirmations)
}

// TestDispatcher_NonInteractiveAskAutoDenies pins the universal
// headless guard: any path that would reach askUser in a
// non-interactive session (eval, MCP serve, A2A adapter) must
// auto-deny rather than block on the Resume channel. This covers the
// preempt-yolo Ask lane (this test), checker ForceAsk
// (TestDispatcher_NonInteractiveCheckerForceAskAutoDenies), and the
// default Ask
// (TestDispatcher_NonInteractiveDefaultAskAutoDenies). Without this
// guard each path would hang indefinitely with no Resume listener.
func TestDispatcher_NonInteractiveAskAutoDenies(t *testing.T) {
	a := newAgent()
	sess := session.New()
	sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)
	sess.NonInteractive = true

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run when denied")
		},
	}

	hd := &stubHookDispatcher{
		on: map[hooks.EventType]*hooks.Result{
			hooks.EventPreToolUsePreYolo: {
				Allowed:        true,
				Decision:       hooks.DecisionAsk,
				DecisionReason: "rm -rf <path>: irreversible",
				Metadata:       map[string]string{"blast_radius": "high"},
			},
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "danger",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rm -rf /tmp/x"}`},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError, "non-interactive ask must produce an error tool response")
	assert.Contains(t, em.responses[0].Output, "non-interactive",
		"deny message should name the cause so eval traces are debuggable")
	assert.Empty(t, em.confirmations,
		"confirmation event must not be emitted in non-interactive mode — there is no one to confirm")
}

// TestDispatcher_NonInteractiveDefaultAskAutoDenies covers the
// no-rule-matched path: a non-interactive session with no preempt-yolo
// hook, no checker rules, no readonly hint, and no --yolo. Today this
// falls through to askUser and would hang. The guard must deny.
func TestDispatcher_NonInteractiveDefaultAskAutoDenies(t *testing.T) {
	a := newAgent()
	sess := session.New()
	sess.NonInteractive = true

	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("must not run when denied")
		},
	}

	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
	}
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{{
		ID:       "x",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"docker ps"}`},
	}}, []tools.Tool{tool}, em)

	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	assert.Contains(t, em.responses[0].Output, "non-interactive")
}
