package app

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
)

type operationKey struct{}

// BeginOperation binds asynchronous lifecycle events to accepted foreground work.
func (a *App) BeginOperation(ctx context.Context) context.Context {
	return context.WithValue(ctx, operationKey{}, a.operation.Add(1))
}

func (a *App) operationContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(operationKey{}).(uint64); ok {
		return ctx
	}
	return a.BeginOperation(ctx)
}

// IsCurrentOperation rejects terminal events from a canceled, replaced run.
func (a *App) IsCurrentOperation(msg tea.Msg) bool {
	event, ok := msg.(interface{ Operation() uint64 })
	return !ok || event.Operation() == 0 || event.Operation() == a.operation.Load()
}

func operationEvent(ctx context.Context, msg tea.Msg) tea.Msg {
	id, _ := ctx.Value(operationKey{}).(uint64)
	if id == 0 {
		return msg
	}
	switch event := msg.(type) {
	case *runtime.StreamStartedEvent:
		tagged := *event
		tagged.OperationID = id
		return &tagged
	case *runtime.StreamStoppedEvent:
		tagged := *event
		tagged.OperationID = id
		return &tagged
	case *runtime.ErrorEvent:
		tagged := *event
		tagged.OperationID = id
		return &tagged
	case *runtime.SessionCompactionEvent:
		tagged := *event
		tagged.OperationID = id
		return &tagged
	default:
		return msg
	}
}
