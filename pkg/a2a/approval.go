package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/runtime"
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
	rt       runtime.Runtime
	events   <-chan runtime.Event
	cancel   context.CancelFunc
	toolName string // function name of the paused call, for approve-tool
	callID   string // tool-call ID reported to the client as long-running
	created  time.Time
}

func (p *pausedRun) cancelAndDrain() {
	p.cancel()
	for range p.events {
	}
}

// approvalRegistry keeps runtimes alive between the request that pauses on a
// tool confirmation and the request that answers it, keyed by A2A contextId.
// It is created per server; a disabled registry (tool approval off) is inert
// beyond routing the run context, so the default A2A behaviour is unchanged.
type approvalRegistry struct {
	enabled bool
	baseCtx context.Context //nolint:containedctx // server-lifetime context paused runs derive from
	ttl     time.Duration

	mu   sync.Mutex
	runs map[string]*pausedRun
}

// newApprovalRegistry builds a registry. When enabled, a background janitor
// evicts expired paused runs and cancels everything on server shutdown
// (baseCtx cancellation). When disabled, no goroutine is started.
func newApprovalRegistry(baseCtx context.Context, enabled bool) *approvalRegistry {
	r := &approvalRegistry{
		enabled: enabled,
		baseCtx: baseCtx,
		ttl:     defaultApprovalTTL,
		runs:    make(map[string]*pausedRun),
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

// runContext returns the context a new run should derive its event stream
// from. In approval mode it is the server-lifetime context, so a run that
// parks on a tool confirmation survives the request that started it;
// otherwise it is the per-invocation context, preserving the default
// drain-on-request-cancel behaviour of the adapter.
func (r *approvalRegistry) runContext(invocation context.Context) context.Context {
	if r.toolApproval() {
		return r.baseCtx
	}
	return invocation
}

// store parks a run under its contextId. Any run already parked under the same
// id is cancelled first: a client that re-asks replaces its pending call.
func (r *approvalRegistry) store(contextID string, run *pausedRun) {
	r.mu.Lock()
	prev, replaced := r.runs[contextID]
	r.runs[contextID] = run
	r.mu.Unlock()
	if replaced && prev != run {
		prev.cancelAndDrain()
	}
}

// take removes and returns the run parked under contextID, if any.
func (r *approvalRegistry) take(contextID string) (*pausedRun, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[contextID]
	if ok {
		delete(r.runs, contextID)
	}
	return run, ok
}

// cancel cancels and drops the run parked under contextID, if any. Used by the
// A2A Cancel path to release a task abandoned before it was answered.
func (r *approvalRegistry) cancel(contextID string) {
	if run, ok := r.take(contextID); ok {
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
			r.evictExpired()
		}
	}
}

func (r *approvalRegistry) evictExpired() {
	deadline := time.Now().Add(-r.ttl)
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
	r.runs = make(map[string]*pausedRun)
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

// resumeDecision maps the FunctionResponse the client sent on the resume
// request to a runtime ResumeRequest. ADK's HandleInputRequired has already
// guaranteed a response with a matching ID is present before the agent runs;
// an absent, malformed, or unrecognised decision is treated as a rejection so
// the model is told rather than left hanging.
func resumeDecision(content *genai.Content, run *pausedRun) runtime.ResumeRequest {
	fr := matchingFunctionResponse(content, run.callID)
	if fr == nil {
		return runtime.ResumeReject("no approval decision provided")
	}
	decision, _ := fr.Response["decision"].(string)
	reason, _ := fr.Response["reason"].(string)
	switch decision {
	case "approve":
		return runtime.ResumeApprove()
	case "approve-tool":
		return runtime.ResumeApproveTool(run.toolName)
	case "reject":
		return runtime.ResumeReject(reason)
	default:
		msg := "unrecognised approval decision"
		if decision != "" {
			msg += ": " + decision
		}
		if reason != "" {
			msg += " (" + reason + ")"
		}
		return runtime.ResumeReject(msg)
	}
}

// matchingFunctionResponse returns the FunctionResponse addressed to callID,
// falling back to the first response present if none matches exactly (ADK only
// lets us run when a matching-ID response exists, so the fallback is defensive).
func matchingFunctionResponse(content *genai.Content, callID string) *genai.FunctionResponse {
	if content == nil {
		return nil
	}
	var fallback *genai.FunctionResponse
	for _, part := range content.Parts {
		if part == nil || part.FunctionResponse == nil {
			continue
		}
		if part.FunctionResponse.ID == callID {
			return part.FunctionResponse
		}
		if fallback == nil {
			fallback = part.FunctionResponse
		}
	}
	return fallback
}
