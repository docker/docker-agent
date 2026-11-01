package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// extraToolsetRuns owns raw runnable extras; prewrapped extras remain borrowed
// from their team or embedder. Only lifecycle is cached, never filtered tools.
type extraToolsetRuns struct {
	mu      sync.Mutex
	entries []extraToolsetRun
	closed  bool
	done    chan struct{}
	err     error
}

type extraToolsetRun struct {
	provider tools.Runnable
	sess     *session.Session
	slot     int
	managed  *tools.StartableToolSet
}

func (e *extraToolsetRuns) start(ctx context.Context, sess *session.Session, slot int, ts tools.ToolSet) error {
	if startable, ok := tools.As[tools.Startable](ts); ok {
		return startable.Start(ctx)
	}
	provider, ok := tools.As[tools.Runnable](ts)
	if !ok {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("runtime is closed")
	}
	keyComparable := reflect.ValueOf(provider).Comparable()
	for _, entry := range e.entries {
		if keyComparable && reflect.ValueOf(entry.provider).Comparable() {
			if entry.provider == provider {
				return entry.managed.Start(ctx)
			}
		} else if !keyComparable && entry.sess == sess && entry.slot == slot {
			return entry.managed.Start(ctx)
		}
	}
	managed := tools.NewStartable(ts)
	e.entries = append(e.entries, extraToolsetRun{provider: provider, sess: sess, slot: slot, managed: managed})
	// Runnable has no startup handshake, so Start never waits for Run here.
	return managed.Start(ctx)
}

func (e *extraToolsetRuns) close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		done := e.done
		e.mu.Unlock()
		<-done
		return e.err
	}
	e.closed = true
	e.done = make(chan struct{})
	entries := e.entries
	e.entries = nil
	e.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if err := entry.managed.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	e.err = errors.Join(errs...)
	close(e.done)
	return e.err
}
