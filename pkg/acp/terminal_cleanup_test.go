package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestTerminalRetirementIsSynchronous(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		manager := newTerminalManager(&acpsdk.AgentSideConnection{}, "owner")
		s := &Session{terminals: manager}
		ctx, op, err := manager.acquire(t.Context())
		require.NoError(t, err)
		closed := s.close(t.Context())
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		_, _, err = manager.acquire(t.Context())
		require.ErrorIs(t, err, errSessionClosed)
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("cleanup returned while an operation still owns a terminal")
		default:
		}
		op.finish()
		<-closed
		require.NoError(t, s.cleanupErr)
	})
}

func TestTerminalCleanupRunsBeforeTurnDrainAndAlongsideRuntime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &Session{}
		_, _, _, peer := terminalFixtureForSession(t, latest.Toolset{}, s)
		manager := s.terminals
		manager.unreleased["owned"] = nil
		_, finishTurn, err := s.startTurn(t.Context())
		require.NoError(t, err)
		releaseRequest := make(chan terminalRequest, 1)
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			if r.method == "terminal/release" {
				releaseRequest <- r
				return nil, nil
			}
			return map[string]any{}, nil
		}
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		s.rt = rt
		closed := s.close(t.Context())
		req := <-releaseRequest
		select {
		case <-rt.started:
			t.Fatal("runtime closed before the active turn drained")
		default:
		}
		finishTurn()
		<-rt.started
		synctest.Wait()
		require.NoError(t, peer.reply(req.id, map[string]any{}, nil))
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("session cleanup returned before runtime cleanup")
		default:
		}
		close(rt.release)
		<-closed
		require.NoError(t, s.cleanupErr)
		assert.Equal(t, 1, strings.Count(strings.Join(peer.methods(), ","), "terminal/release"))
	})
}

func TestTerminalCleanupFailureIsCachedBySession(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &Session{}
		_, _, _, peer := terminalFixtureForSession(t, latest.Toolset{}, s)
		s.terminals.unreleased["owned"] = errors.New("initial release failed")
		releaseRequest := make(chan terminalRequest, 1)
		peer.respond = func(r terminalRequest) (any, *acpsdk.RequestError) {
			releaseRequest <- r
			return nil, nil
		}
		ctx := propagation.TraceContext{}.Extract(t.Context(), propagation.MapCarrier{"traceparent": testTraceParent})
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		closed := s.close(ctx)
		req := <-releaseRequest
		cancel()
		var waiters sync.WaitGroup
		for range 10 {
			waiters.Go(func() {
				assert.Equal(t, closed, s.close(t.Context()))
			})
		}
		waiters.Wait()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("canceling the caller abandoned the final release")
		default:
		}
		require.NoError(t, peer.reply(req.id, nil, acpsdk.NewInternalError("persistent failure")))
		<-closed
		require.ErrorContains(t, s.cleanupErr, "cleanup is unresolved")
		firstErr := s.cleanupErr
		for range 3 {
			<-s.close(t.Context())
			assert.Same(t, firstErr, s.cleanupErr)
		}
		assert.Equal(t, []string{"terminal/release"}, peer.methods())
		var params struct {
			Meta map[string]any `json:"_meta"`
		}
		require.NoError(t, json.Unmarshal(req.params, &params))
		assert.Equal(t, testTraceParent, params.Meta["traceparent"])
	})
}

func TestTerminalAdmissionRacingSessionClose(t *testing.T) {
	t.Parallel()
	for range 20 {
		manager := newTerminalManager(&acpsdk.AgentSideConnection{}, "owner")
		s := &Session{terminals: manager}
		start := make(chan struct{})
		var callers sync.WaitGroup
		for range 10 {
			callers.Go(func() {
				<-start
				ctx, op, err := manager.acquire(t.Context())
				if err != nil {
					assert.ErrorIs(t, err, errSessionClosed)
					return
				}
				defer op.finish()
				<-ctx.Done()
			})
		}
		close(start)
		closed := s.close(t.Context())
		callers.Wait()
		<-closed
		require.NoError(t, s.cleanupErr)
		manager.mu.Lock()
		assert.Empty(t, manager.active)
		manager.mu.Unlock()
	}
}

func TestTerminalCleanupConcurrentFirstClosers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &Session{}
		_, _, _, peer := terminalFixtureForSession(t, latest.Toolset{}, s)
		s.terminals.unreleased["owned"] = errors.New("initial failure")
		requests := make(chan terminalRequest, 10)
		peer.respond = func(req terminalRequest) (any, *acpsdk.RequestError) {
			requests <- req
			return nil, nil
		}
		start := make(chan struct{})
		closures := make(chan (<-chan struct{}), 10)
		for range cap(closures) {
			go func() {
				<-start
				closures <- s.close(t.Context())
			}()
		}
		close(start)
		closed := <-closures
		for range cap(closures) - 1 {
			assert.Equal(t, closed, <-closures)
		}
		req := <-requests
		synctest.Wait()
		assert.Empty(t, requests, "concurrent closes must start one final sweep")
		require.NoError(t, peer.reply(req.id, nil, acpsdk.NewInternalError("persistent failure")))
		<-closed
		require.Error(t, s.cleanupErr)
		assert.Equal(t, []string{"terminal/release"}, peer.methods())
	})
}
