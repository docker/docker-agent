package app

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
)

type eventGenerationKey struct{}

type generationEvent struct {
	generation uint64
	inner      tea.Msg
}

// RetireEvents discards deliveries belonging to a replaced conversation.
func (a *App) RetireEvents() {
	generation := a.eventGeneration.Add(1)
	a.subsMu.Lock()
	for _, sub := range a.subs {
		if sub.queue != nil {
			sub.queue.retire(generation)
		}
	}
	a.subsMu.Unlock()
	if retire, ok := a.runtime.(interface{ RetireBackgroundEvents() }); ok {
		retire.RetireBackgroundEvents()
		ctx := a.eventContext(a.ctx())
		a.runtime.OnBackgroundEvent(func(event runtime.Event) { a.sendEvent(ctx, event) })
	}
}

// IsEventGeneration checks producer identity at the final consumer boundary.
func (a *App) IsEventGeneration(generation uint64) bool {
	return generation == a.eventGeneration.Load()
}

func (a *App) eventContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, eventGenerationKey{}, a.eventGeneration.Load())
}

func (a *App) stampEvent(ctx context.Context, event tea.Msg) tea.Msg {
	generation, ok := ctx.Value(eventGenerationKey{}).(uint64)
	if !ok {
		generation = a.eventGeneration.Load()
	}
	return generationEvent{generation: generation, inner: event}
}
