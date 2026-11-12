package app

import (
	"context"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestReplacementDiscardsOldThrottledContentAndLateStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		var got []tea.Msg
		ready := make(chan struct{})
		go a.SubscribeReliable(ctx, func(msg tea.Msg) { got = append(got, msg) }, WithSubscriptionReady(func() { close(ready) }))
		<-ready
		oldCtx := a.eventContext(ctx)
		a.sendEvent(oldCtx, runtime.AgentChoice("root", a.session.ID, "OLD-ANSWER", "old"))
		synctest.Wait()
		require.Empty(t, got, "fixture must leave text in the throttle buffer")
		a.ReplaceSession(ctx, session.New())
		a.sendEvent(context.WithoutCancel(oldCtx), runtime.StreamStopped("old-session", "root", "canceled"))
		want := runtime.AgentChoice("root", a.session.ID, "NEW-ANSWER", "new")
		a.sendEvent(ctx, want)
		a.sendEvent(ctx, runtime.StreamStopped(a.session.ID, "root", "normal"))
		synctest.Wait()
		require.Len(t, got, 2)
		require.Same(t, want, got[0])
		require.Equal(t, "normal", got[1].(*runtime.StreamStoppedEvent).Reason)
	})
}

func TestRetireEventsDiscardsRawAndThrottledDeliveries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		oldCtx := a.eventContext(ctx)
		a.sendEvent(oldCtx, runtime.AgentChoice("root", a.session.ID, "RAW-OLD-ANSWER", "old"))
		a.RetireEvents()
		var got []tea.Msg
		ready := make(chan struct{})
		go a.SubscribeReliable(ctx, func(msg tea.Msg) { got = append(got, msg) }, WithSubscriptionReady(func() { close(ready) }))
		<-ready
		synctest.Wait()
		marker := runtime.StreamStopped(a.session.ID, "root", "normal")
		a.sendEvent(ctx, marker)
		synctest.Wait()
		require.Equal(t, []tea.Msg{marker}, got)
	})
}

func TestRoutingRetainsOriginWhenRetirementRacesMapping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		mapping := make(chan struct{})
		resume := make(chan struct{})
		ready := make(chan struct{})
		type delivery struct{ generation uint64 }
		var got []tea.Msg
		go a.SubscribeReliable(ctx, func(msg tea.Msg) { got = append(got, msg) }, WithSubscriptionReady(func() { close(ready) }), WithGenerationEventMapper(func(_ tea.Msg, generation uint64) tea.Msg {
			close(mapping)
			select {
			case <-resume:
			case <-ctx.Done():
				return nil
			}
			return delivery{generation: generation}
		}))
		<-ready
		a.sendEvent(ctx, runtime.StreamStopped(a.session.ID, "root", "normal"))
		<-mapping
		a.RetireEvents()
		close(resume)
		synctest.Wait()
		require.Len(t, got, 1)
		require.False(t, a.IsEventGeneration(got[0].(delivery).generation), "the consumer must reject an event accepted just before retirement")
	})
}

type contextualEventsRuntime struct {
	mockRuntime

	background  func(context.Context, runtime.Event)
	elicitation func(context.Context, runtime.Event)
}

func (r *contextualEventsRuntime) OnBackgroundEventWithContext(handler func(context.Context, runtime.Event)) {
	r.background = handler
}

func (r *contextualEventsRuntime) OnElicitationRequestWithContext(handler func(context.Context, runtime.Event)) {
	r.elicitation = handler
}

func TestDetachedCallbacksKeepOriginAcrossReplacement(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rt := &contextualEventsRuntime{}
		a := New(ctx, rt, session.New())
		a.Start(ctx)
		var got []tea.Msg
		ready := make(chan struct{})
		go a.SubscribeReliable(ctx, func(msg tea.Msg) { got = append(got, msg) }, WithSubscriptionReady(func() { close(ready) }))
		<-ready
		old := a.eventContext(ctx)
		a.ReplaceSession(ctx, session.New())
		rt.background(old, runtime.StreamStopped("old-child", "root", "normal"))
		rt.elicitation(old, runtime.ElicitationRequest("OLD REQUEST", "form", nil, "", "old", "", "old-child", nil, "root"))
		current := a.eventContext(ctx)
		request := runtime.ElicitationRequest("CURRENT REQUEST", "form", nil, "", "new", "", "new-child", nil, "root")
		rt.elicitation(current, request)
		synctest.Wait()
		require.Equal(t, []tea.Msg{request}, got)
	})
}

func TestCanceledDetachedAccountingStillDeliversWithOriginalGeneration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rt := &contextualEventsRuntime{}
		a := New(ctx, rt, session.New())
		a.Start(ctx)
		var got []tea.Msg
		ready := make(chan struct{})
		go a.SubscribeReliable(ctx, func(msg tea.Msg) { got = append(got, msg) }, WithSubscriptionReady(func() { close(ready) }))
		<-ready
		origin, stop := context.WithCancel(a.eventContext(ctx))
		stop()
		for range 100 {
			rt.background(origin, runtime.NewTokenUsageEvent("child", "root", &runtime.Usage{}))
		}
		synctest.Wait()
		require.Len(t, got, 100)
		a.RetireEvents()
		rt.background(origin, runtime.NewTokenUsageEvent("child", "root", &runtime.Usage{}))
		synctest.Wait()
		require.Len(t, got, 100)
	})
}
