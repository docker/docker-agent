package app

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

type eventGenerationKey struct{}

type generationEvent struct {
	generation uint64
	inner      tea.Msg
}

// RetireEvents discards deliveries belonging to a replaced conversation.
// Its pending elicitations are cancelled: once their events are discarded
// nothing could answer them, and the tools that raised them would hang.
func (a *App) RetireEvents() {
	generation := a.eventGeneration.Add(1)
	if canceler, ok := a.runtime.(interface{ CancelElicitations() }); ok {
		canceler.CancelElicitations()
	}
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

// ensureEventContext preserves the identity of work that was already stamped.
func (a *App) ensureEventContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(eventGenerationKey{}).(uint64); ok {
		return ctx
	}
	return a.eventContext(ctx)
}

func (a *App) stampEvent(ctx context.Context, event tea.Msg) tea.Msg {
	return generationEvent{generation: a.generationOf(ctx), inner: event}
}

func (a *App) generationOf(ctx context.Context) uint64 {
	if generation, ok := ctx.Value(eventGenerationKey{}).(uint64); ok {
		return generation
	}
	return a.eventGeneration.Load()
}

// cancelRetiredElicitation answers a request raised by a retired conversation
// (e.g. its background job) after RetireEvents ran; delivering it would only
// get it discarded downstream.
func (a *App) cancelRetiredElicitation(ctx context.Context, event tea.Msg) bool {
	request, ok := event.(*runtime.ElicitationRequestEvent)
	if !ok || a.IsEventGeneration(a.generationOf(ctx)) {
		return false
	}
	_ = a.runtime.ResumeElicitation(context.WithoutCancel(ctx), tools.ElicitationActionCancel, nil, request.ElicitationID)
	return true
}
