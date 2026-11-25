package a2a

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/genai"
)

// executorWrapper wraps an ADK executor and fixes artifact update events
// to ensure they have non-nil Parts slices, which is required by the A2A spec.
type executorWrapper struct {
	executor  *adka2a.Executor
	approvals *approvalRegistry
}

var (
	_ a2asrv.AgentExecutor         = (*executorWrapper)(nil)
	_ a2asrv.AgentExecutionCleaner = (*executorWrapper)(nil)
)

func newExecutorWrapper(config adka2a.ExecutorConfig, approvals *approvalRegistry) *executorWrapper {
	return &executorWrapper{
		executor:  adka2a.NewExecutor(config),
		approvals: approvals,
	}
}

func (w *executorWrapper) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	ctx = withTaskInfo(ctx, execCtx.TaskInfo())
	if !w.approvals.toolApproval() {
		return fixArtifactEvents(w.executor.Execute(ctx, execCtx))
	}
	return func(yield func(a2a.Event, error) bool) {
		runCtx := ctx
		if task := execCtx.StoredTask; task != nil && task.Status.State == a2a.TaskStateInputRequired {
			ticket, ok := w.takeResume(execCtx)
			if !ok {
				// The parked run expired or was lost in a restart: nothing can
				// resume it, and starting a new model turn would answer a
				// question nobody asked.
				msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(approvalExpiredReason))
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, msg), nil)
				return
			}
			defer func() {
				if !ticket.used.Load() {
					ticket.run.cancelAndDrain()
				}
			}()
			runCtx = withResume(ctx, ticket)
		}
		for event, err := range fixArtifactEvents(w.executor.Execute(runCtx, execCtx)) {
			if !yield(event, err) {
				return
			}
		}
	}
}

// approvalExpiredReason is the status message of a task whose approval can no
// longer be resumed.
const approvalExpiredReason = "Tool approval expired: the paused run is no longer available"

// takeResume takes the run parked for the task if the message answers its
// pending call.
func (w *executorWrapper) takeResume(execCtx *a2asrv.ExecutorContext) (*resumeTicket, bool) {
	run, ok := w.approvals.pending(execCtx.TaskID)
	if !ok || execCtx.Message == nil {
		return nil, false
	}
	parts, err := adka2a.ToGenAIParts(execCtx.Message.Parts)
	if err != nil {
		return nil, false
	}
	fr := functionResponse(&genai.Content{Parts: parts}, run.callID)
	if fr == nil {
		return nil, false
	}
	run, ok = w.approvals.take(execCtx.TaskID, run.callID)
	if !ok {
		return nil, false
	}
	return &resumeTicket{run: run, response: fr}, true
}

func (w *executorWrapper) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	// A task waiting in INPUT_REQUIRED has a runtime parked in the registry.
	// Cancelling the A2A task must cancel and release it so it does not leak.
	// An active run is cancelled with the execution context the SDK cancels.
	w.approvals.cancel(execCtx.TaskID)
	return w.executor.Cancel(ctx, execCtx)
}

// Cleanup delegates to the ADK executor, which implements
// a2asrv.AgentExecutionCleaner; dropping it would change cleanup semantics.
func (w *executorWrapper) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	w.executor.Cleanup(ctx, execCtx, result, err)
}

// fixArtifactEvents wraps an event sequence and fixes artifact update events
// with nil Parts before yielding them. Everything else passes through unchanged.
func fixArtifactEvents(events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for event, err := range events {
			if artifactEvent, ok := event.(*a2a.TaskArtifactUpdateEvent); ok {
				if artifactEvent.Artifact != nil && artifactEvent.Artifact.Parts == nil {
					// Replace nil with an empty slice
					artifactEvent.Artifact.Parts = a2a.ContentParts{}
				}
			}
			if !yield(event, err) {
				return
			}
		}
	}
}
