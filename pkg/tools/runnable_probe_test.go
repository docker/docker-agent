package tools_test

import (
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/docker/docker-agent/pkg/tools"
)

type probeBoundary struct {
	tools.ToolSet

	calls   atomic.Int32
	enabled atomic.Bool
	reached chan struct{}
	proceed chan struct{}
}

func (w *probeBoundary) Unwrap() tools.ToolSet {
	if w.enabled.Load() && w.calls.Add(1) == 2 {
		close(w.reached)
		<-w.proceed
	}
	return w.ToolSet
}

func TestNestedLifecycleProbeRace(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"healthy", "available"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				raw := &blockingRestartToolSet{entered: make(chan struct{}, 2), release: make(chan struct{})}
				owner := tools.NewStartable(raw)
				boundary := &probeBoundary{ToolSet: owner, reached: make(chan struct{}), proceed: make(chan struct{})}
				middle := tools.NewStartable(boundary)
				outer := tools.NewStartable(middle)
				if err := outer.Start(t.Context()); err != nil {
					t.Fatal(err)
				}
				boundary.enabled.Store(true)
				done := make(chan bool, 1)
				go func() {
					if kind == "healthy" {
						done <- outer.TryIsHealthy()
					} else {
						done <- outer.TryIsAvailable()
					}
				}()
				<-boundary.reached
				restarted := make(chan error, 1)
				go func() { restarted <- owner.RestartIfSupported(t.Context()) }()
				<-raw.entered
				close(boundary.proceed)
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Error("nonblocking probe waits on a nested lifecycle operation after its successful precheck")
				}
				close(raw.release)
				if err := <-restarted; err != nil {
					t.Error(err)
				}
				synctest.Wait()
				if err := outer.Stop(t.Context()); err != nil {
					t.Error(err)
				}
			})
		})
	}
}
