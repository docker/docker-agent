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

func TestReliableSubscriberRetainsFinalResponseWhenStalled(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		resume := make(chan struct{})
		var received []tea.Msg
		go a.SubscribeReliable(ctx, func(msg tea.Msg) {
			select {
			case <-resume:
			case <-ctx.Done():
				return
			}
			received = append(received, msg)
		})
		var witness []tea.Msg
		go a.SubscribeWith(ctx, func(msg tea.Msg) { witness = append(witness, msg) })
		synctest.Wait()

		want := []tea.Msg{runtime.StreamStarted(a.session.ID, "root")}
		for range subscriberBufferSize + 1 {
			want = append(want, runtime.NewTokenUsageEvent(a.session.ID, "root", &runtime.Usage{}))
		}
		want = append(want,
			runtime.AgentChoiceReasoning("root", a.session.ID, "thinking", "answer"),
			runtime.AgentChoice("root", a.session.ID, "FINAL-RESPONSE", "answer"),
			runtime.StreamStopped(a.session.ID, "root", "normal"),
		)
		for _, msg := range want {
			a.sendEvent(ctx, msg)
			// Drain the best-effort witness while the reliable subscriber stays blocked.
			synctest.Wait()
		}
		require.Equal(t, want, witness, "a stalled TUI must not block other subscribers")

		close(resume)
		synctest.Wait()
		require.Len(t, received, len(want), "all content must reach the TUI before completion")
		for i, msg := range want {
			require.Equal(t, msg, received[i], "delivery %d must preserve event order", i)
		}
	})
}

func TestReliableSubscriberCancellationReleasesBlockedQueue(t *testing.T) {
	t.Parallel()
	for _, cancelOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "subscriber", true: "owner"}[cancelOwner], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				a := New(ctx, &mockRuntime{}, session.New())
				a.Start(ctx)
				subCtx, cancelSub := context.WithCancel(t.Context())
				defer cancelSub()
				release := make(chan struct{})
				defer close(release)
				go func() {
					a.SubscribeReliable(subCtx, func(tea.Msg) { <-release })
				}()
				synctest.Wait()
				require.Len(t, a.subs, 1)
				queue := a.subs[0].queue
				for range subscriberBufferSize + 2 {
					a.sendEvent(ctx, runtime.SessionTitle(a.session.ID, "queued"))
				}
				synctest.Wait()
				require.NotEmpty(t, queue.pending)
				if cancelOwner {
					cancel()
				} else {
					cancelSub()
				}
				synctest.Wait()
				require.Empty(t, a.subs, "cleanup must not wait for a blocked callback")
				require.Empty(t, queue.pending)
				queue.pushGeneration(runtime.SessionTitle(a.session.ID, "late"), 0)
				require.Empty(t, queue.pending, "retired fan-out snapshots cannot retain new events")
			})
		})
	}
}

func TestReliableSubscriberMapsBeforeBuffering(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		resume := make(chan struct{})
		mapped := make(chan tea.Msg, 4)
		type delivery struct {
			generation int
			msg        tea.Msg
		}
		generation := 1
		var received []tea.Msg
		go a.SubscribeReliable(ctx, func(msg tea.Msg) {
			select {
			case <-resume:
			case <-ctx.Done():
				return
			}
			received = append(received, msg)
		}, WithEventMapper(func(msg tea.Msg) tea.Msg {
			result := delivery{generation, msg}
			mapped <- result
			return result
		}))
		synctest.Wait()
		first := runtime.SessionTitle(a.session.ID, "first")
		second := runtime.SessionTitle(a.session.ID, "second")
		a.sendEvent(ctx, first)
		a.sendEvent(ctx, second)
		synctest.Wait()
		require.Equal(t, delivery{1, first}, <-mapped)
		require.Equal(t, delivery{1, second}, <-mapped)
		generation = 2
		third := runtime.SessionTitle(a.session.ID, "replacement")
		a.sendEvent(ctx, third)
		synctest.Wait()
		require.Equal(t, delivery{2, third}, <-mapped)
		close(resume)
		synctest.Wait()
		require.Equal(t, []tea.Msg{delivery{1, first}, delivery{1, second}, delivery{2, third}}, received)
	})
}

func TestReliableSubscriptionReadyPrecedesFirstProducedEvent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := New(ctx, &mockRuntime{}, session.New())
		go a.SubscribeWith(ctx, func(tea.Msg) {})
		synctest.Wait()
		ready := make(chan struct{})
		var received []tea.Msg
		go a.SubscribeReliable(ctx, func(msg tea.Msg) { received = append(received, msg) }, WithSubscriptionReady(func() { close(ready) }))
		<-ready
		want := runtime.AgentChoice("root", a.session.ID, "FAST-FINAL-ANSWER", "answer")
		a.sendEvent(ctx, want)
		a.sendEvent(ctx, runtime.StreamStopped(a.session.ID, "root", "normal"))
		synctest.Wait()
		require.Len(t, received, 2)
		require.Same(t, want, received[0])
	})
}
