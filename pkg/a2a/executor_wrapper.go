package a2a

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
)

// executorWrapper wraps an ADK executor and fixes artifact update events
// to ensure they have non-nil Parts slices, which is required by the A2A spec.
type executorWrapper struct {
	executor *adka2a.Executor
}

var (
	_ a2asrv.AgentExecutor         = (*executorWrapper)(nil)
	_ a2asrv.AgentExecutionCleaner = (*executorWrapper)(nil)
)

func newExecutorWrapper(config adka2a.ExecutorConfig) *executorWrapper {
	return &executorWrapper{
		executor: adka2a.NewExecutor(config),
	}
}

func (w *executorWrapper) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return fixArtifactEvents(w.executor.Execute(ctx, execCtx))
}

func (w *executorWrapper) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
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
