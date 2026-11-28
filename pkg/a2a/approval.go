package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"go.opentelemetry.io/otel/trace"
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

// approvalShutdownTimeout bounds how long shutdown waits for runtimes to stop
// before the stores they write to are closed.
const approvalShutdownTimeout = 10 * time.Second

// abandonTimeout bounds failing the task of a run dropped without an answer.
const abandonTimeout = 5 * time.Second

// approvalShutdownReason is the status message of a task whose pending
// approval is dropped at server shutdown.
const approvalShutdownReason = "Tool approval canceled: the A2A server shut down"

// abandonFunc moves the A2A task of a parked run dropped without an answer to
// state (failed, or canceled when a cancel lost the race to the pause), so
// clients polling or watching it learn it will not resume. It returns the
// updated task, or nil if the task was not moved.
type abandonFunc func(ctx context.Context, run *pausedRun, state a2a.TaskState, reason string) *a2a.Task

// approvalHookPoint names a step of the pause/cancel handoff; tests use hooks
// to force interleavings.
type approvalHookPoint int

const (
	hookBeforePark approvalHookPoint = iota
	hookAfterPark
	hookBeforeCancelEvent
	hookAfterCancelEvent
	hookAfterFinalEvent
)

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
	grant     string // permission pattern approve-in-context persists (shell:cmd=ls* for commands); "" disables it
	callID    string // tool-call ID reported to the client as long-running
	rawArgs   string // call arguments when they don't decode to a JSON object; handlers may still repair and run them
	created   time.Time
	user      *a2asrv.User      // caller that owns the task in the task store
	spanCtx   trace.SpanContext // invocation that started the run, linked from resumes
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
	stop    context.CancelFunc
	done    chan struct{} // closed once the janitor has released every parked run
	ttl     time.Duration

	mu          sync.Mutex
	runs        map[a2a.TaskID]*pausedRun
	canceling   map[a2a.TaskID]bool      // CancelTask in progress: the execution must not pause
	cancelCalls map[a2a.TaskID]int       // CancelTask requests in flight; they may share one SDK cancellation
	canceled    map[a2a.TaskID]*a2a.Task // canceled by resolveCancel; reported to every in-flight CancelTask
	closed      bool                     // shutdown released the parked runs; later parks are abandoned
	handoffs    map[a2a.TaskID]*handoff  // parked runs whose INPUT_REQUIRED is not persisted yet
	settling    int                      // handoffs not ended yet, deferred abandon included
	settled     chan struct{}            // closed while settling is zero
	onAbandon   abandonFunc
	live        int           // runtimes whose event stream is still open
	idle        chan struct{} // closed while live is zero

	hook func(approvalHookPoint, a2a.TaskID) // tests only
}

// handoff tracks a run from park until the execution publishing its
// INPUT_REQUIRED ends. The stored task is still WORKING meanwhile, so an
// abandon is deferred until the pause is persisted (see endHandoff).
type handoff struct {
	run       *pausedRun
	abandoned bool
	state     a2a.TaskState
	reason    string
}

// newApprovalRegistry builds a registry. When enabled, a background janitor
// evicts expired paused runs and cancels everything on server shutdown
// (baseCtx cancellation or close). When disabled, no goroutine is started.
func newApprovalRegistry(baseCtx context.Context, enabled bool) *approvalRegistry {
	ctx, stop := context.WithCancel(baseCtx)
	idle := make(chan struct{})
	close(idle)
	settled := make(chan struct{})
	close(settled)
	r := &approvalRegistry{
		enabled:     enabled,
		baseCtx:     ctx,
		stop:        stop,
		done:        make(chan struct{}),
		ttl:         defaultApprovalTTL,
		runs:        make(map[a2a.TaskID]*pausedRun),
		canceling:   make(map[a2a.TaskID]bool),
		cancelCalls: make(map[a2a.TaskID]int),
		canceled:    make(map[a2a.TaskID]*a2a.Task),
		handoffs:    make(map[a2a.TaskID]*handoff),
		idle:        idle,
		settled:     settled,
	}
	if enabled {
		go r.janitor()
	} else {
		close(r.done)
	}
	return r
}

// setAbandonHandler installs the callback that fails the task of a parked run
// dropped on expiry or shutdown.
func (r *approvalRegistry) setAbandonHandler(fn abandonFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onAbandon = fn
}

// close cancels every parked run, fails its task, and waits (bounded) for all
// runtimes to stop and all handoffs to end, so nothing writes to the stores or
// enqueues a webhook once they are closed after it.
func (r *approvalRegistry) close() {
	if r == nil {
		return
	}
	r.stop()
	timeout := time.NewTimer(approvalShutdownTimeout)
	defer timeout.Stop()
	select {
	case <-r.done:
	case <-timeout.C:
		return
	}
	// Runtimes first: a run parking late opens its handoff before it is drained.
	for _, quiet := range []*chan struct{}{&r.idle, &r.settled} {
		r.mu.Lock()
		ch := *quiet
		r.mu.Unlock()
		select {
		case <-ch:
		case <-timeout.C:
			return
		}
	}
}

// track relays a runtime's event stream, counting the runtime as live until
// the stream closes.
func (r *approvalRegistry) track(events <-chan runtime.Event) <-chan runtime.Event {
	if r == nil {
		return events
	}
	r.mu.Lock()
	if r.live == 0 {
		r.idle = make(chan struct{})
	}
	r.live++
	r.mu.Unlock()

	out := make(chan runtime.Event)
	go func() {
		defer func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.live--
			if r.live == 0 {
				close(r.idle)
			}
		}()
		defer close(out)
		for ev := range events {
			out <- ev
		}
	}()
	return out
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
	if _, open := r.handoffs[run.taskID]; !open {
		if r.settling == 0 {
			r.settled = make(chan struct{})
		}
		r.settling++
	}
	r.handoffs[run.taskID] = &handoff{run: run}
	if r.closed {
		r.mu.Unlock()
		r.abandon(run, a2a.TaskStateFailed, approvalShutdownReason)
		return
	}
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

// cancel cancels and drops the run parked for taskID, if any. Used once an
// execution ends with its task no longer waiting for input.
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

// markCanceling records a CancelTask for taskID until its outcome is known
// (see resolveCancel), so a concurrent execution does not pause the task.
func (r *approvalRegistry) markCanceling(taskID a2a.TaskID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceling[taskID] = true
}

func (r *approvalRegistry) isCanceling(taskID a2a.TaskID) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.canceling[taskID]
}

// resolveCancel releases the run parked for taskID once a CancelTask is over.
// If the task did not end canceled, the execution published INPUT_REQUIRED
// before the cancel event (a2a-go then reports ErrTaskNotCancelable, or
// ErrQueueClosed once that execution closed its pipe): the task
// is canceled here instead, so it never advertises a dropped approval, and
// endCancelCall reports it to the CancelTask requests waiting on the outcome.
func (r *approvalRegistry) resolveCancel(taskID a2a.TaskID, canceled bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.canceling, taskID)
	run, ok := r.runs[taskID]
	delete(r.runs, taskID)
	r.mu.Unlock()
	switch {
	case !ok:
	case canceled:
		run.cancelAndDrain()
	default:
		if task := r.abandon(run, a2a.TaskStateCanceled, ""); task != nil {
			r.mu.Lock()
			if r.cancelCalls[taskID] > 0 {
				r.canceled[taskID] = task
			}
			r.mu.Unlock()
		}
	}
}

// beginCancelCall records a CancelTask request for taskID until endCancelCall.
func (r *approvalRegistry) beginCancelCall(taskID a2a.TaskID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelCalls[taskID]++
}

// endCancelCall returns the task resolveCancel canceled for taskID, if any.
// a2a-go hands one cancellation result to every overlapping CancelTask, so the
// task is kept until the last of them ends.
func (r *approvalRegistry) endCancelCall(taskID a2a.TaskID) (*a2a.Task, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.canceled[taskID]
	r.cancelCalls[taskID]--
	if r.cancelCalls[taskID] <= 0 {
		delete(r.cancelCalls, taskID)
		delete(r.canceled, taskID)
	}
	return task, ok
}

func (r *approvalRegistry) runHook(point approvalHookPoint, taskID a2a.TaskID) {
	if r != nil && r.hook != nil {
		r.hook(point, taskID)
	}
}

func (r *approvalRegistry) janitor() {
	defer close(r.done)
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
		r.abandon(run, a2a.TaskStateFailed, approvalExpiredReason)
	}
}

func (r *approvalRegistry) cancelAll() {
	r.mu.Lock()
	runs := r.runs
	r.runs = make(map[a2a.TaskID]*pausedRun)
	r.closed = true
	r.mu.Unlock()
	for _, run := range runs {
		r.abandon(run, a2a.TaskStateFailed, approvalShutdownReason)
	}
}

// abandon moves the task of a run dropped without an answer to state, then
// releases the run.
func (r *approvalRegistry) abandon(run *pausedRun, state a2a.TaskState, reason string) *a2a.Task {
	r.mu.Lock()
	h, deferred := r.handoffs[run.taskID]
	deferred = deferred && h.run == run
	if deferred {
		h.abandoned, h.state, h.reason = true, state, reason
	}
	r.mu.Unlock()
	var task *a2a.Task
	if !deferred {
		task = r.endTask(run, state, reason)
	}
	run.cancelAndDrain()
	return task
}

// endHandoff ends the handoff of the run parked for taskID once the execution
// that paused it is over, applying an abandon that came before INPUT_REQUIRED
// was persisted. The handoff stays counted until the task is ended, so close
// does not return before the write and the webhook enqueue.
func (r *approvalRegistry) endHandoff(taskID a2a.TaskID, published bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	h, ok := r.handoffs[taskID]
	delete(r.handoffs, taskID)
	r.mu.Unlock()
	if !ok {
		return
	}
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.settling--
		if r.settling == 0 {
			close(r.settled)
		}
	}()
	if h.abandoned && published {
		r.endTask(h.run, h.state, h.reason)
	}
}

func (r *approvalRegistry) endTask(run *pausedRun, state a2a.TaskState, reason string) *a2a.Task {
	r.mu.Lock()
	fn := r.onAbandon
	r.mu.Unlock()
	if fn == nil {
		return nil
	}
	// Detached: at shutdown the server context is already canceled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.baseCtx), abandonTimeout)
	defer cancel()
	return fn(ctx, run, state, reason)
}

// inputRequiredEvent builds the ADK event that drives the A2A task into
// TASK_STATE_INPUT_REQUIRED: a FunctionCall part whose ID is listed in
// LongRunningToolIDs. ADK's input-required processor turns this into the
// status-message DataPart (metadata adk_type=function_call,
// adk_is_long_running=true) that the client replies to.
func inputRequiredEvent(agentName string, tc tools.ToolCall) *adksession.Event {
	args, _ := decodeToolArgs(tc.Function.Arguments)
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

// decodeToolArgs decodes call arguments as a JSON object. ok is false when
// non-empty arguments are not one.
func decodeToolArgs(raw string) (args map[string]any, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
		return nil, false
	}
	return args, true
}

// resumeDecision maps a validated approval response to a runtime
// ResumeRequest. The interceptor has already rejected unknown decisions.
func resumeDecision(fr *genai.FunctionResponse, grant string) runtime.ResumeRequest {
	decision, _ := fr.Response["decision"].(string)
	reason, _ := fr.Response["reason"].(string)
	switch decision {
	case decisionApprove:
		return runtime.ResumeApprove()
	case decisionApproveInContext:
		if grant == "" {
			// ResumeApproveTool("") would grant the whole tool.
			return runtime.ResumeReject(reason)
		}
		return runtime.ResumeApproveTool(grant)
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
