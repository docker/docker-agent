package toolexec_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// captureEmitter records every event the dispatcher emits, in order, so
// tests can make precise assertions about the dispatch flow. A confirm
// channel signals when a confirmation event lands so cancellation tests
// don't need to busy-wait.
type captureEmitter struct {
	calls         []tools.ToolCall
	responses     []responseRecord
	confirmations []tools.ToolCall
	hookBlocks    []hookBlockRecord
	messages      []*session.Message
	confirmed     chan struct{} // optional; closed after the first confirmation event
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
	e.calls = append(e.calls, tc)
}

func (e *captureEmitter) EmitToolCallResponse(toolCallID string, _ tools.Tool, result *tools.ToolCallResult, output, _ string) {
	e.responses = append(e.responses, responseRecord{
		ToolCallID: toolCallID,
		Output:     output,
		IsError:    result.IsError,
	})
}

func (e *captureEmitter) EmitToolCallConfirmation(tc tools.ToolCall, _ tools.Tool, _ string) {
	e.confirmations = append(e.confirmations, tc)
	if e.confirmed != nil {
		select {
		case <-e.confirmed:
			// already closed
		default:
			close(e.confirmed)
		}
	}
}

func (e *captureEmitter) EmitHookBlocked(tc tools.ToolCall, _ tools.Tool, message, _ string) {
	e.hookBlocks = append(e.hookBlocks, hookBlockRecord{ToolCall: tc, Message: message})
}

func (e *captureEmitter) EmitMessageAdded(_ string, msg *session.Message, _ string) {
	e.messages = append(e.messages, msg)
}

func newAgent() *agent.Agent {
	return agent.New("test", "test agent")
}

func TestDispatcher_RoutesToToolsetHandler(t *testing.T) {
	a := newAgent()
	sess := session.New()
	sess.ToolsApproved = true // skip approval so we exercise the happy path

	var handlerCalls int
	tool := tools.Tool{
		Name: "echo",
		Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
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

func TestDispatcher_RoutesToRuntimeHandler(t *testing.T) {
	a := newAgent()
	sess := session.New()
	sess.ToolsApproved = true

	var handlerCalls int
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Handlers: map[string]toolexec.ToolHandler{
			"transfer_task": func(_ context.Context, _ *session.Session, _ tools.ToolCall) (*tools.ToolCallResult, error) {
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
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) {
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

func TestDispatcher_UserCancellationStopsBatchAndSynthesizesRemaining(t *testing.T) {
	a := newAgent()
	sess := session.New()

	resume := make(chan toolexec.ResumeRequest, 1)
	d := &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Resume:   resume,
	}
	em := &captureEmitter{confirmed: make(chan struct{})}

	tool := tools.Tool{
		Name:    "shell",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) { panic("must not run") },
	}

	// Cancel as soon as the dispatcher asks for confirmation on the first
	// call. The remaining two calls in the batch must receive synthetic
	// error responses so the conversation history stays consistent (the
	// Responses API rejects orphaned tool calls).
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
	}
	assert.Contains(t, em.responses[0].Output, "canceled by the user")
	assert.Contains(t, em.responses[1].Output, "previous tool call")
	assert.Contains(t, em.responses[2].Output, "previous tool call")
}

func TestDispatcher_ResumeApproveRunsTool(t *testing.T) {
	a := newAgent()
	sess := session.New()

	var ran bool
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) {
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
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name:    "shell",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) { panic("must not run") },
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

func TestDispatcher_ResumeApproveToolPersistsToSessionPermissions(t *testing.T) {
	a := newAgent()
	sess := session.New()

	var ran bool
	tool := tools.Tool{
		Name: "shell",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) {
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

func TestDispatcher_ReadOnlyHintAutoApproves(t *testing.T) {
	a := newAgent()
	sess := session.New() // ToolsApproved=false; no permissions configured

	var ran bool
	tool := tools.Tool{
		Name: "read_file",
		Annotations: tools.ToolAnnotations{
			ReadOnlyHint: true,
		},
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) {
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
	assert.Empty(t, em.confirmations, "read-only tool must not prompt the user")
	require.Len(t, em.responses, 1)
	assert.False(t, em.responses[0].IsError)
}

func TestDispatcher_DenyByPermissionsEmitsErrorResponse(t *testing.T) {
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name:    "shell",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) { panic("must not run") },
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
	a := newAgent()
	sess := session.New()
	sess.ToolsApproved = true

	original := "raw output with a secret"
	rewritten := "output with [REDACTED]"

	tool := tools.Tool{
		Name: "leaky",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) {
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
	a := newAgent()
	sess := session.New()
	sess.ToolsApproved = true

	original := "untouched output"

	tool := tools.Tool{
		Name: "leaky",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) {
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
	a := newAgent()
	sess := session.New()

	tool := tools.Tool{
		Name:    "shell",
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolCallResult, error) { panic("must not run") },
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
