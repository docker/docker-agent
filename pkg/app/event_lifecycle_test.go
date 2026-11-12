package app

import (
	"context"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestEventBusOwnerCancellation(t *testing.T) {
	t.Parallel()

	for _, subscribeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "start first", true: "subscribe first"}[subscribeFirst], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				a := New(t.Context(), &mockRuntime{}, session.New())
				ownerCtx, cancelOwner := context.WithCancel(t.Context())
				defer cancelOwner()
				subscriberDone := make(chan struct{})
				if !subscribeFirst {
					a.Start(ownerCtx)
				}
				go func() {
					defer close(subscriberDone)
					a.SubscribeWith(t.Context(), func(tea.Msg) {})
				}()
				synctest.Wait()
				if subscribeFirst {
					a.Start(ownerCtx)
				}
				a.sendEvent(t.Context(), runtime.SessionTitle(a.session.ID, "before close"))
				synctest.Wait()

				cancelOwner()
				synctest.Wait()
				select {
				case <-subscriberDone:
				default:
					t.Fatal("retired App still has an active subscriber")
				}
				require.Empty(t, a.subs)
				// Detached final events must not block after the bus has stopped.
				for range cap(a.events) + 1 {
					a.sendEvent(context.WithoutCancel(ownerCtx), runtime.StreamStopped(a.session.ID, "root", "canceled"))
				}
				// synctest also requires the throttler and fan-out goroutines to exit.
			})
		})
	}
}

func TestEventBusSurvivesSubscriberCancellation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ownerCtx, cancelOwner := context.WithCancel(t.Context())
		defer cancelOwner()
		a := New(t.Context(), &mockRuntime{}, session.New())
		a.Start(ownerCtx)
		subCtx, cancelSub := context.WithCancel(t.Context())
		defer cancelSub()
		go a.SubscribeWith(subCtx, func(tea.Msg) {})
		synctest.Wait()
		cancelSub()
		synctest.Wait()
		require.Empty(t, a.subs)

		received := make(chan tea.Msg, 1)
		go a.SubscribeWith(t.Context(), func(msg tea.Msg) { received <- msg })
		synctest.Wait()
		want := runtime.SessionTitle(a.session.ID, "reconnected")
		a.sendEvent(t.Context(), want)
		synctest.Wait()
		select {
		case got := <-received:
			assert.Same(t, want, got)
		default:
			t.Fatal("the first subscriber must not own the shared event bus")
		}
		cancelOwner()
		synctest.Wait()
	})
}

func TestEventBusCancellationUnblocksProducer(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		for range cap(a.events) {
			a.events <- runtime.SessionTitle(a.session.ID, "full")
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.sendEvent(context.WithoutCancel(ctx), runtime.StreamStopped(a.session.ID, "root", "canceled"))
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("a detached producer is blocked on the retired bus")
		}
	})
}

type teardownEventsRuntime struct {
	mockRuntime

	started chan struct{}
	drained chan struct{}
}

func (r *teardownEventsRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, sink runtime.EventSink) {
	r.emit(ctx, sess, sink)
}

func (r *teardownEventsRuntime) Summarize(ctx context.Context, sess *session.Session, _ string, sink runtime.EventSink) {
	r.emit(ctx, sess, sink)
}

func (r *teardownEventsRuntime) emit(ctx context.Context, sess *session.Session, sink runtime.EventSink) {
	close(r.started)
	<-ctx.Done()
	for range 256 {
		sink.Emit(runtime.SessionTitle(sess.ID, "cleanup"))
	}
	close(r.drained)
}

func TestReplacementStartupDiscardsCanceledTail(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		rt := &teardownEventsRuntime{started: make(chan struct{}), drained: make(chan struct{})}
		a := New(t.Context(), rt, session.New())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a.reEmitStartupInfo(ctx)
		<-rt.started
		cancel()
		synctest.Wait()
		require.Empty(t, a.events, "canceled startup events must not reach the replacement page")
		select {
		case <-rt.drained:
		default:
			t.Fatal("canceled startup producer was not drained")
		}
	})
}

func TestAppDrainsProducersAfterTeardown(t *testing.T) {
	t.Parallel()

	for _, entry := range []struct {
		name string
		run  func(*App, context.Context, context.CancelFunc)
	}{
		{"startup", func(a *App, ctx context.Context, _ context.CancelFunc) { a.Start(ctx) }},
		{"replacement", func(a *App, ctx context.Context, _ context.CancelFunc) { a.reEmitStartupInfo(ctx) }},
		{"compaction", func(a *App, ctx context.Context, cancel context.CancelFunc) { a.CompactSession(ctx, cancel, "") }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				rt := &teardownEventsRuntime{started: make(chan struct{}), drained: make(chan struct{})}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				a := New(ctx, rt, session.New())
				entry.run(a, ctx, cancel)
				<-rt.started
				cancel()
				synctest.Wait()
				select {
				case <-rt.drained:
				default:
					t.Fatal("App stopped draining before the runtime finished cleanup")
				}
			})
		})
	}
}
