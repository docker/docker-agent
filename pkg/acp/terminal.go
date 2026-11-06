package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	terminalOutputLimit   = 64 << 10
	terminalCreateBudget  = 10 * time.Second
	terminalCleanupBudget = 5 * time.Second
)

type terminalOwnerKey struct{}

type terminalManager struct {
	conn       *acp.AgentSideConnection
	sid        acp.SessionId
	mu         sync.Mutex
	active     map[*terminalOperation]struct{}
	unreleased map[string]error
	uncertain  error
	closing    bool
	pending    sync.WaitGroup
}

type terminalOperation struct {
	manager *terminalManager
	cancel  context.CancelFunc
	id      string
}

func newTerminalManager(conn *acp.AgentSideConnection, sid string) *terminalManager {
	return &terminalManager{conn: conn, sid: acp.SessionId(sid), active: make(map[*terminalOperation]struct{}), unreleased: make(map[string]error)}
}

func (m *terminalManager) acquire(ctx context.Context) (context.Context, *terminalOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return nil, nil, errSessionClosed
	}
	if m.conn == nil {
		return nil, nil, errors.New("ACP terminal connection unavailable")
	}
	if err := m.failureLocked(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	op := &terminalOperation{manager: m, cancel: cancel}
	m.active[op] = struct{}{}
	m.pending.Add(1)
	return ctx, op, nil
}

func (op *terminalOperation) finish() {
	op.cancel()
	m := op.manager
	m.mu.Lock()
	delete(m.active, op)
	m.mu.Unlock()
	m.pending.Done()
}

func (m *terminalManager) failure() error { m.mu.Lock(); defer m.mu.Unlock(); return m.failureLocked() }

func (m *terminalManager) failureLocked() error {
	if m.uncertain != nil {
		return m.uncertain
	}
	for _, err := range m.unreleased {
		if err != nil {
			return errors.New("ACP terminal cleanup is unresolved; close the session before continuing")
		}
	}
	return nil
}

func (op *terminalOperation) create(ctx context.Context, req acp.CreateTerminalRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Join create even after tool cancellation so a late ID can still be released.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalCreateBudget)
	defer cancel()
	req.Meta = traceMeta(createCtx, req.Meta)
	resp, err := op.manager.conn.CreateTerminal(createCtx, req)
	if err == nil && resp.TerminalId == "" {
		err = errors.New("client returned an empty terminal ID")
	}
	m := op.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		var rejection *acp.RequestError
		if errors.As(err, &rejection) && (rejection.Code == -32600 || rejection.Code == -32601 || rejection.Code == -32602) {
			return fmt.Errorf("terminal creation rejected: %w", err)
		}
		m.uncertain = errors.Join(m.uncertain, fmt.Errorf("terminal creation outcome is unknown; client cleanup required: %w", err))
		return m.uncertain
	}
	if _, exists := m.unreleased[resp.TerminalId]; exists {
		m.uncertain = errors.Join(m.uncertain, errors.New("client reused an owned terminal ID"))
		return m.uncertain
	}
	op.id = resp.TerminalId
	m.unreleased[op.id] = nil
	return nil
}

func (op *terminalOperation) release(ctx context.Context) error {
	if op.id == "" {
		return nil
	}
	return op.manager.release(ctx, op.id)
}

func (m *terminalManager) release(ctx context.Context, id string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalCleanupBudget)
	defer cancel()
	_, err := m.conn.ReleaseTerminal(cleanup, acp.ReleaseTerminalRequest{Meta: traceMeta(cleanup, nil), SessionId: m.sid, TerminalId: id})
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.unreleased, id)
	} else {
		m.unreleased[id] = err
	}
	return err
}

func (op *terminalOperation) kill(ctx context.Context) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalCleanupBudget)
	defer cancel()
	_, err := op.manager.conn.KillTerminal(cleanup, acp.KillTerminalRequest{Meta: traceMeta(cleanup, nil), SessionId: op.manager.sid, TerminalId: op.id})
	return err
}

// retire closes admission before the session starts asynchronous cleanup.
func (m *terminalManager) retire() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return
	}
	m.closing = true
	for op := range m.active {
		op.cancel()
	}
}

// cleanup is run once by the owning session, after retirement.
func (m *terminalManager) cleanup(ctx context.Context) error {
	m.retire()
	m.pending.Wait()
	// Operations have relinquished ownership; only this final sweep can release now.
	m.mu.Lock()
	ids := make([]string, 0, len(m.unreleased))
	for id := range m.unreleased {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.release(ctx, id)
	}
	return m.failure()
}
