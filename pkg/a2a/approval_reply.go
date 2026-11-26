package a2a

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/genai"
)

const (
	decisionApprove          = "approve"
	decisionApproveInContext = "approve-in-context"
	decisionReject           = "reject"
)

// allowedDecisions are the decisions accepted for a call, in schema order.
// approve-in-context needs a grant: a command call that cannot be scoped to
// its command has none.
func allowedDecisions(grant string) []string {
	if grant == "" {
		return []string{decisionApprove, decisionReject}
	}
	return []string{decisionApprove, decisionApproveInContext, decisionReject}
}

// approvalInterceptor validates approval replies before the SDK records them,
// so an invalid reply leaves the task waiting with its history untouched.
type approvalInterceptor struct {
	a2asrv.PassthroughCallInterceptor

	approvals *approvalRegistry
}

type cancelTaskIDKey struct{}

func (i *approvalInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if cancelReq, ok := req.Payload.(*a2a.CancelTaskRequest); ok && cancelReq != nil && i.approvals.toolApproval() {
		return context.WithValue(ctx, cancelTaskIDKey{}, cancelReq.ID), nil, nil
	}
	sendReq, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || sendReq.Message == nil || !i.approvals.toolApproval() {
		return ctx, nil, nil
	}
	ext := toolApprovalExtension()
	if callCtx.Extensions().Requested(ext) || slices.Contains(sendReq.Message.Extensions, ext.URI) {
		callCtx.Extensions().Activate(ext)
	}
	return ctx, nil, i.approvals.validateReply(sendReq.Message)
}

// After reports a CancelTask that lost the race to INPUT_REQUIRED, but whose
// cleanup canceled the task, as the success it is: a2a-go only sees the
// INPUT_REQUIRED result and returns ErrTaskNotCancelable.
func (i *approvalInterceptor) After(ctx context.Context, _ *a2asrv.CallContext, resp *a2asrv.Response) error {
	taskID, ok := ctx.Value(cancelTaskIDKey{}).(a2a.TaskID)
	if !ok {
		return nil
	}
	task, ok := i.approvals.takeCanceled(taskID)
	if ok && errors.Is(resp.Err, a2a.ErrTaskNotCancelable) {
		resp.Payload, resp.Err = task, nil
	}
	return nil
}

// validateReply rejects a message that targets a task waiting for approval
// without a valid decision for its pending call, and a new task in a context
// whose conversation is held by such a task.
func (r *approvalRegistry) validateReply(msg *a2a.Message) error {
	// a2a-go decodes a JSON null part as a nil *Part, which ADK's converters
	// dereference.
	if slices.Contains(msg.Parts, nil) {
		return invalidReply("message parts must not be null")
	}
	if msg.TaskID == "" {
		if id, ok := r.pendingInContext(msg.ContextID); ok {
			return invalidReply(fmt.Sprintf("task %s is waiting for tool approval in this context; reply with its taskId or cancel it", id))
		}
		return nil
	}
	run, ok := r.pending(msg.TaskID)
	if !ok {
		return nil
	}
	parts, err := adka2a.ToGenAIParts(msg.Parts)
	if err != nil {
		return invalidReply("malformed approval reply")
	}
	fr := functionResponse(&genai.Content{Parts: parts}, run.callID)
	if fr == nil {
		return invalidReply(fmt.Sprintf("expected a function_response for call '%s'", run.callID))
	}
	decision, _ := fr.Response["decision"].(string)
	if allowed := allowedDecisions(run.grant); !slices.Contains(allowed, decision) {
		return invalidReply(fmt.Sprintf("decision '%s' not allowed; expected [%s]", decision, strings.Join(allowed, ", ")))
	}
	return nil
}

func invalidReply(reason string) error {
	return a2a.NewError(a2a.ErrInvalidParams, reason).WithDetails(map[string]any{"reason": reason})
}

// resumeTicket hands a parked run, taken by the executor for the reply that
// answers it, to the agent invocation serving that reply.
type resumeTicket struct {
	run      *pausedRun
	response *genai.FunctionResponse
	used     atomic.Bool
}

type taskInfoKey struct{}

type resumeKey struct{}

func withTaskInfo(ctx context.Context, info a2a.TaskInfo) context.Context {
	return context.WithValue(ctx, taskInfoKey{}, info)
}

func taskInfoFrom(ctx context.Context) a2a.TaskInfo {
	info, _ := ctx.Value(taskInfoKey{}).(a2a.TaskInfo)
	return info
}

func withResume(ctx context.Context, t *resumeTicket) context.Context {
	return context.WithValue(ctx, resumeKey{}, t)
}

func resumeFrom(ctx context.Context) (*resumeTicket, bool) {
	t, ok := ctx.Value(resumeKey{}).(*resumeTicket)
	return t, ok
}
