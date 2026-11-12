package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/teamloader"
)

// sessionLifecycle is also the publication token for constructors admitted before close.
type sessionLifecycle struct {
	id      string
	newRoot bool // NewSession allocates a root ID before it may be persisted.
	closing bool
	active  int
	pending sync.WaitGroup
	done    chan struct{}
	err     error
}

type agentOperation struct {
	cancel    context.CancelFunc
	done      chan struct{}
	lifecycle *sessionLifecycle
}

// startOperationLocked enrolls work before shutdown can stop admission.
func (a *Agent) startOperationLocked(ctx context.Context, lifecycle *sessionLifecycle) (context.Context, *agentOperation) {
	ctx, cancel := context.WithCancel(ctx)
	op := &agentOperation{cancel: cancel, lifecycle: lifecycle, done: make(chan struct{})}
	if a.pending == nil {
		a.pending = make(map[*agentOperation]struct{})
	}
	a.pending[op] = struct{}{}
	a.operations.Add(1)
	if lifecycle != nil {
		lifecycle.active++
		lifecycle.pending.Add(1)
	}
	return ctx, op
}

func (a *Agent) finishOperation(op *agentOperation) {
	op.cancel()
	a.mu.Lock()
	delete(a.pending, op)
	if op.lifecycle != nil {
		op.lifecycle.active--
		if op.lifecycle.active == 0 && !op.lifecycle.closing && op.lifecycle.err == nil {
			sid := op.lifecycle.id
			if a.lifecycles[sid] == op.lifecycle && a.sessions[sid] == nil {
				delete(a.lifecycles, sid)
			}
		}
		op.lifecycle.pending.Done()
	}
	a.mu.Unlock()
	a.operations.Done()
	close(op.done)
}

func (a *Agent) beginOperation(ctx context.Context) (context.Context, *agentOperation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil, nil, errors.New("agent stopped")
	}
	if err := a.authenticationErrorLocked(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	ctx, op := a.startOperationLocked(ctx, nil)
	return ctx, op, nil
}

func (a *Agent) beginSessionConstruction(ctx context.Context, sid string) (context.Context, *agentOperation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil, nil, errors.New("agent stopped")
	}
	if a.deletion != nil {
		return nil, nil, errors.New("session deletion in progress; retry after it finishes")
	}
	if err := a.authenticationErrorLocked(); err != nil {
		return nil, nil, err
	}
	if a.team == nil {
		return nil, nil, errors.New("agent not initialized")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	lifecycle := a.lifecycles[sid]
	if lifecycle != nil && lifecycle.err != nil {
		return nil, nil, fmt.Errorf("session cleanup failed; cannot resume: %w", lifecycle.err)
	}
	if lifecycle != nil && lifecycle.closing {
		select {
		case <-lifecycle.done:
			lifecycle = nil
		default:
			return nil, nil, errors.New("session is closing; retry resume after close completes")
		}
	}
	if lifecycle == nil {
		lifecycle = &sessionLifecycle{id: sid}
		if a.lifecycles == nil {
			a.lifecycles = make(map[string]*sessionLifecycle)
		}
		a.lifecycles[sid] = lifecycle
	}
	ctx, op := a.startOperationLocked(ctx, lifecycle)
	return ctx, op, nil
}

func (a *Agent) loadTeamSerialized(ctx context.Context, workingDir string) (*teamloader.LoadResult, error) {
	// Source.Read and EncryptedConfig must belong to the same load.
	select {
	case a.loadGate <- struct{}{}:
		defer func() { <-a.loadGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.loadTeam(ctx, workingDir)
}

func (a *Agent) discardSession(ctx context.Context, op *agentOperation, s *Session) error {
	<-s.close(ctx)
	if s.cleanupErr != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		if op.lifecycle != nil {
			op.lifecycle.err = errors.Join(op.lifecycle.err, s.cleanupErr)
		} else {
			a.cleanupErr = errors.Join(a.cleanupErr, s.cleanupErr)
		}
	}
	return s.cleanupErr
}

// closeSessionLocked commits closure before removing the routable session.
func (a *Agent) closeSessionLocked(ctx context.Context, sid string) *sessionLifecycle {
	lifecycle := a.lifecycles[sid]
	if lifecycle == nil {
		lifecycle = &sessionLifecycle{id: sid}
		if a.sessions[sid] == nil {
			lifecycle.done = make(chan struct{})
			close(lifecycle.done)
			return lifecycle
		}
		if a.lifecycles == nil {
			a.lifecycles = make(map[string]*sessionLifecycle)
		}
		a.lifecycles[sid] = lifecycle
	}
	if lifecycle.closing {
		return lifecycle
	}
	lifecycle.closing = true
	lifecycle.done = make(chan struct{})
	s := a.sessions[sid]
	if s != nil {
		s.close(ctx)
		delete(a.sessions, sid)
	}
	for op := range a.pending {
		if op.lifecycle == lifecycle {
			op.cancel()
		}
	}
	go func() {
		lifecycle.pending.Wait()
		if s != nil {
			<-s.close(ctx)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if s != nil {
			lifecycle.err = errors.Join(lifecycle.err, s.cleanupErr)
			delete(a.owned, s)
		}
		close(lifecycle.done)
	}()
	return lifecycle
}

func waitForCleanup(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop rejects new work and joins the final drain, even if ctx is canceled.
func (a *Agent) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.stopDone != nil {
		done := a.stopDone
		a.mu.Unlock()
		<-done
		return a.stopErr
	}
	a.stopped = true
	a.stopDone = make(chan struct{})
	for op := range a.pending {
		op.cancel()
	}
	for sid := range a.sessions {
		a.closeSessionLocked(ctx, sid)
	}
	var closing []*sessionLifecycle
	for sid := range a.lifecycles {
		closing = append(closing, a.closeSessionLocked(ctx, sid))
	}
	validationTeam := a.team
	a.team = nil
	a.mu.Unlock()

	a.operations.Wait()
	var errs []error
	for _, lifecycle := range closing {
		<-lifecycle.done
		errs = append(errs, lifecycle.err)
	}
	if validationTeam != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		errs = append(errs, validationTeam.StopToolSets(cleanupCtx))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopErr = errors.Join(append(errs, a.cleanupErr)...)
	close(a.stopDone)
	return a.stopErr
}
