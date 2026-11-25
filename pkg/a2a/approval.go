package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// defaultApprovalTTL bounds how long a paused run (a tool call awaiting an
// INPUT_REQUIRED decision) is kept alive before the janitor cancels it. A
// client that parks a tool confirmation and never answers must not leak the
// runtime forever.
const defaultApprovalTTL = time.Hour

// approvalJanitorInterval is how often the registry sweeps for expired runs.
const approvalJanitorInterval = time.Minute

// pausedRun captures a runtime parked inside askUser, waiting for the A2A
// client to approve or reject a tool call on a subsequent request. Ownership
// of the runtime — and the obligation to cancel and drain it — belongs to
// whoever holds the pausedRun: the registry while it waits, or the adapter
// goroutine once it takes it back to resume.
type pausedRun struct {
	taskID    a2a.TaskID
	contextID string
	rt        runtime.Runtime
	sess      *session.Session
	events    <-chan runtime.Event
	cancel    context.CancelFunc
	toolName  string // function name of the paused call, for approve-tool
	callID    string // tool-call ID reported to the client as long-running
	created   time.Time
}

func (p *pausedRun) cancelAndDrain() {
	p.cancel()
	for range p.events {
	}
}

// approvalRegistry keeps runtimes alive between the request that pauses on a
// tool confirmation and the request that answers it, keyed by A2A task ID so
// another task in the same context can neither answer nor cancel it. It is
// created per server; a disabled registry (tool approval off) is inert beyond
// routing the run context, so the default A2A behaviour is unchanged.
type approvalRegistry struct {
	enabled bool
	baseCtx context.Context //nolint:containedctx // server lifetime bounding detached runs
	ttl     time.Duration

	mu   sync.Mutex
	runs map[a2a.TaskID]*pausedRun
}

// newApprovalRegistry builds a registry. When enabled, a background janitor
// evicts expired paused runs and cancels everything on server shutdown
// (baseCtx cancellation). When disabled, no goroutine is started.
func newApprovalRegistry(baseCtx context.Context, enabled bool) *approvalRegistry {
	r := &approvalRegistry{
		enabled: enabled,
		baseCtx: baseCtx,
		ttl:     defaultApprovalTTL,
		runs:    make(map[a2a.TaskID]*pausedRun),
	}
	if enabled {
		go r.janitor()
	}
	return r
}

// toolApproval reports whether out-of-band tool approval is enabled. Nil-safe
// so callers that never configured a registry behave as if it were disabled.
func (r *approvalRegistry) toolApproval() bool {
	return r != nil && r.enabled
}

// runContext returns the context a new run streams from. In approval mode it
// keeps the invocation's values (trace, baggage) but not its cancellation, so
// a run that parks survives the request that started it; server shutdown
// still cancels it. Callers bind it to each invocation while the run is active
// (see bindToInvocation). Otherwise it is the invocation context itself.
func (r *approvalRegistry) runContext(invocation context.Context) (context.Context, context.CancelFunc) {
	if !r.toolApproval() {
		return context.WithCancel(invocation)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(invocation))
	stop := context.AfterFunc(r.baseCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// bindToInvocation cancels a run when the invocation serving it ends (request
// gone, A2A cancel). The returned stop detaches it once the run is parked.
func bindToInvocation(invocation context.Context, cancel context.CancelFunc) (stop func() bool) {
	return context.AfterFunc(invocation, cancel)
}

// park stores a run under its task. A run already parked for the same task is
// cancelled first.
func (r *approvalRegistry) park(run *pausedRun) {
	r.mu.Lock()
	prev, replaced := r.runs[run.taskID]
	r.runs[run.taskID] = run
	r.mu.Unlock()
	if replaced && prev != run {
		prev.cancelAndDrain()
	}
}

// pending returns the run parked for taskID without removing it.
func (r *approvalRegistry) pending(taskID a2a.TaskID) (*pausedRun, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[taskID]
	return run, ok
}

// pendingInContext returns a task of contextID that is waiting for approval.
func (r *approvalRegistry) pendingInContext(contextID string) (a2a.TaskID, bool) {
	if r == nil || contextID == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, run := range r.runs {
		if run.contextID == contextID {
			return id, true
		}
	}
	return "", false
}

// take removes and returns the run parked for taskID, only if it waits on
// callID.
func (r *approvalRegistry) take(taskID a2a.TaskID, callID string) (*pausedRun, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[taskID]
	if !ok || run.callID != callID {
		return nil, false
	}
	delete(r.runs, taskID)
	return run, true
}

// cancel cancels and drops the run parked for taskID, if any. Used by the A2A
// Cancel path to release a task abandoned before it was answered.
func (r *approvalRegistry) cancel(taskID a2a.TaskID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	run, ok := r.runs[taskID]
	delete(r.runs, taskID)
	r.mu.Unlock()
	if ok {
		run.cancelAndDrain()
	}
}

func (r *approvalRegistry) janitor() {
	ticker := time.NewTicker(approvalJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.baseCtx.Done():
			r.cancelAll()
			return
		case <-ticker.C:
			r.evictExpired(time.Now())
		}
	}
}

func (r *approvalRegistry) evictExpired(now time.Time) {
	deadline := now.Add(-r.ttl)
	var expired []*pausedRun
	r.mu.Lock()
	for id, run := range r.runs {
		if run.created.Before(deadline) {
			expired = append(expired, run)
			delete(r.runs, id)
		}
	}
	r.mu.Unlock()
	for _, run := range expired {
		run.cancelAndDrain()
	}
}

func (r *approvalRegistry) cancelAll() {
	r.mu.Lock()
	runs := r.runs
	r.runs = make(map[a2a.TaskID]*pausedRun)
	r.mu.Unlock()
	for _, run := range runs {
		run.cancelAndDrain()
	}
}

// inputRequiredEvent builds the ADK event that drives the A2A task into
// TASK_STATE_INPUT_REQUIRED: a FunctionCall part whose ID is listed in
// LongRunningToolIDs. ADK's input-required processor turns this into the
// status-message DataPart (metadata adk_type=function_call,
// adk_is_long_running=true) that the client replies to.
func inputRequiredEvent(agentName string, tc tools.ToolCall) *adksession.Event {
	var args map[string]any
	if raw := strings.TrimSpace(tc.Function.Arguments); raw != "" {
		// Malformed arguments should still surface the call for approval, just
		// without a parsed args map.
		_ = json.Unmarshal([]byte(raw), &args)
	}
	return &adksession.Event{
		Author: agentName,
		LLMResponse: model.LLMResponse{
			Content: genai.NewContentFromParts([]*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: args,
				},
			}}, genai.RoleModel),
		},
		LongRunningToolIDs: []string{tc.ID},
	}
}

// resumeDecision maps a validated approval response to a runtime
// ResumeRequest. The interceptor has already rejected unknown decisions.
func resumeDecision(fr *genai.FunctionResponse, toolName string) runtime.ResumeRequest {
	decision, _ := fr.Response["decision"].(string)
	reason, _ := fr.Response["reason"].(string)
	switch decision {
	case decisionApprove:
		return runtime.ResumeApprove()
	case decisionApproveTool:
		return runtime.ResumeApproveTool(toolName)
	default:
		return runtime.ResumeReject(reason)
	}
}

// functionResponse returns the FunctionResponse addressed to callID.
func functionResponse(content *genai.Content, callID string) *genai.FunctionResponse {
	if content == nil {
		return nil
	}
	for _, part := range content.Parts {
		if part != nil && part.FunctionResponse != nil && part.FunctionResponse.ID == callID {
			return part.FunctionResponse
		}
	}
	return nil
}
