package acp

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/session"
)

type sessionDeletion struct {
	id   string
	done chan struct{}
	err  error
}

// UnstableDeleteSession implements the SDK's session/delete handler.
func (a *Agent) UnstableDeleteSession(ctx context.Context, params acp.UnstableDeleteSessionRequest) (acp.UnstableDeleteSessionResponse, error) {
	ctx, span := startACPRequest(ctx, "session/delete", params.Meta)
	defer span.End()
	sid := string(params.SessionId)
	if sid == "" {
		return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidParams("sessionId is required")
	}
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return acp.UnstableDeleteSessionResponse{}, err
	}
	if err := a.authenticationErrorLocked(); err != nil {
		a.mu.Unlock()
		return acp.UnstableDeleteSessionResponse{}, err
	}
	if a.stopped || a.team == nil {
		a.mu.Unlock()
		return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidRequest("agent is stopped or not initialized")
	}
	deletion := a.deletion
	if deletion != nil {
		a.mu.Unlock()
		if deletion.id != sid {
			return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidRequest("another session deletion is in progress")
		}
	} else {
		// An unrelated constructor could be loading a descendant not yet registered.
		for op := range a.pending {
			if op.lifecycle != nil && op.lifecycle.id != sid {
				a.mu.Unlock()
				return acp.UnstableDeleteSessionResponse{}, acp.NewInvalidRequest("session construction in progress; retry deletion after it finishes")
			}
		}
		deletion = &sessionDeletion{id: sid, done: make(chan struct{})}
		a.deletion = deletion
		deleteCtx, op := a.startOperationLocked(ctx, nil)
		a.mu.Unlock()
		go func() {
			defer a.finishOperation(op)
			err := a.deleteSession(deleteCtx, sid)
			a.mu.Lock()
			defer a.mu.Unlock()
			deletion.err = err
			a.deletion = nil
			close(deletion.done)
		}()
	}
	if err := waitForCleanup(ctx, deletion.done); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}
	return acp.UnstableDeleteSessionResponse{}, deletion.err
}

func (a *Agent) deleteSession(ctx context.Context, sid string) error {
	saved, err := a.sessionStore.GetSession(ctx, sid)
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		return fmt.Errorf("reading session before deletion: %w", err)
	}
	if saved != nil && saved.ParentID != "" {
		return acp.NewInvalidParams("delete the root session, not an individual child session")
	}

	a.mu.Lock()
	candidates := make(map[string]struct{})
	for owned := range a.owned {
		if owned.id != sid {
			candidates[owned.id] = struct{}{}
		}
	}
	for id, lifecycle := range a.lifecycles {
		if id != sid && lifecycle.err != nil && !lifecycle.newRoot {
			candidates[id] = struct{}{}
		}
	}
	a.mu.Unlock()
	for candidate := range candidates {
		descendant, err := a.isStoredDescendant(ctx, candidate, sid)
		if err != nil {
			return err
		}
		if descendant {
			return acp.NewInvalidRequest("a descendant session is loaded or has unresolved cleanup; close it before deleting the root")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	lifecycle := a.closeSessionLocked(context.WithoutCancel(ctx), sid)
	a.mu.Unlock()
	// Keep admission blocked even if the caller stops waiting for the drain.
	<-lifecycle.done
	if lifecycle.err != nil {
		return lifecycle.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.sessionStore.DeleteSession(ctx, sid); err != nil && !errors.Is(err, session.ErrNotFound) {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

func (a *Agent) isStoredDescendant(ctx context.Context, id, root string) (bool, error) {
	seen := make(map[string]struct{})
	for id != "" {
		if id == root {
			return true, nil
		}
		if _, ok := seen[id]; ok {
			return false, errors.New("cyclic session ancestry")
		}
		seen[id] = struct{}{}
		saved, err := a.sessionStore.GetSession(ctx, id)
		if err != nil {
			return false, fmt.Errorf("checking session ancestry: %w", err)
		}
		id = saved.ParentID
	}
	return false, nil
}
