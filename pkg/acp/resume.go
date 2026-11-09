package acp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/coder/acp-go-sdk"
)

func validateResumeWorkingDir(saved, requested string) error {
	if saved == "" {
		if requested != "" {
			return errors.New("session has no saved working directory; create a new session to select a workspace")
		}
		return nil
	}
	savedInfo, err := os.Stat(saved)
	if err != nil {
		return fmt.Errorf("session working directory is unavailable: %w", err)
	}
	if !savedInfo.IsDir() {
		return errors.New("session working directory must be a directory")
	}
	if requested == "" {
		return nil
	}
	requestedInfo, err := os.Stat(requested)
	if err != nil {
		return fmt.Errorf("requested working directory is unavailable: %w", err)
	}
	if !requestedInfo.IsDir() || !os.SameFile(savedInfo, requestedInfo) {
		return errors.New("resume working directory must match the session's working directory")
	}
	return nil
}

func (a *Agent) reconnectRegisteredSession(ctx context.Context, s *Session, workingDir string, additionalDirs []string, servers []clientMCPServer, op *agentOperation, replay bool, configuration *sessionConfiguration) error {
	saved, _ := s.workspaceSnapshot()
	if err := validateResumeWorkingDir(saved, workingDir); err != nil {
		return acp.NewInvalidParams(err.Error())
	}
	if err := a.reserveReconnect(ctx, s, replay); err != nil {
		return err
	}
	defer func() {
		if replay {
			s.finishLoading()
		} else {
			s.turns <- struct{}{}
		}
	}()

	var next *clientMCPGeneration
	if s.clientMCP != nil {
		var err error
		next, err = prepareClientMCP(ctx, servers, cmp.Or(saved, a.defaultWorkingDir()))
		if err != nil {
			return errors.Join(err, a.discardClientMCP(ctx, op, s, next))
		}
	}
	a.mu.Lock()
	s.mu.Lock()
	var commitErr error
	switch {
	case a.stopped || a.sessions[s.id] != s || s.closed:
		commitErr = errSessionClosed
	case s.failed != nil:
		commitErr = s.failed
	case op.lifecycle.err != nil:
		commitErr = op.lifecycle.err
	case ctx.Err() != nil:
		commitErr = ctx.Err()
	}
	var previous *clientMCPGeneration
	if commitErr == nil {
		s.additionalDirs = slices.Clone(additionalDirs)
		if s.clientMCP != nil {
			previous = s.clientMCP.swap(next)
		}
	}
	s.mu.Unlock()
	a.mu.Unlock()
	if commitErr != nil {
		return errors.Join(commitErr, a.discardClientMCP(ctx, op, s, next))
	}
	if err := a.discardClientMCP(ctx, op, s, previous); err != nil {
		return err
	}
	if configuration != nil {
		*configuration = s.configuration(ctx)
		s.commandMu.Lock()
		s.lastConfig = nil
		s.commandMu.Unlock()
	}
	if replay {
		return a.replayLoadedSession(ctx, s)
	}
	a.refreshCommands(ctx, s)
	return nil
}

func (a *Agent) discardClientMCP(ctx context.Context, op *agentOperation, s *Session, generation *clientMCPGeneration) error {
	err := generation.close(ctx)
	if err != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		op.lifecycle.err = errors.Join(op.lifecycle.err, err)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.failed = fmt.Errorf("client MCP cleanup failed: %w", err)
		if s.cancel != nil {
			s.cancel()
		}
		if generation := s.clientMCP.generation(); generation != nil {
			generation.retire()
		}
	}
	return err
}

func (a *Agent) reserveReconnect(ctx context.Context, s *Session, replay bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return errors.New("agent stopped")
	}
	if a.sessions[s.id] != s {
		return errSessionClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSessionClosed
	}
	if s.terminals != nil {
		if err := s.terminals.failure(); err != nil {
			return err
		}
	}
	if s.failed != nil {
		return s.failed
	}
	if lifecycle := a.lifecycles[s.id]; lifecycle != nil && lifecycle.err != nil {
		return lifecycle.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	busy := acp.NewInvalidRequest("session has a running or pending prompt; retry resume after it finishes")
	if s.cancel != nil {
		return busy
	}
	s.initTurns()
	select {
	case <-s.turns:
		s.loading = replay
	default:
		// A canceled queued prompt can clear cancel while an earlier turn drains.
		return busy
	}
	return nil
}
