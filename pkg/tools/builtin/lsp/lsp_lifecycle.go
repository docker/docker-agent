package lsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// lspConnector adapts the LSP server lifecycle to lifecycle.Connector. It
// spawns the server process, runs the initialize/initialized handshake,
// and returns an lspSession the supervisor can Wait on and Close.
type lspConnector struct{ h *lspHandler }

// Connect spawns the LSP server, performs the LSP handshake, and returns
// a Session. On success the active session state (cmd, stdin, stdout,
// capabilities, serverInfo) is published on h under h.mu so per-request
// methods can use them without going through the supervisor.
func (c *lspConnector) Connect(ctx context.Context) (lifecycle.Session, error) {
	h := c.h
	slog.DebugContext(ctx, "Starting LSP server", "command", h.command, "args", h.args)

	p, err := spawnLSPProcess(h)
	if err != nil {
		return nil, err
	}

	h.publishSession(p.cmd, p.stdin, p.stdout, p.cancel)

	if err := h.runHandshake(ctx, p.stdin); err != nil {
		h.clearSession()
		_ = p.stdin.Close()
		p.cancel()
		_ = p.cmd.Wait()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, lifecycle.Classify(err)
	}

	slog.DebugContext(ctx, "LSP server initialized", "command", h.command)
	h.fireToolsChanged()

	return &lspSession{
		h:             h,
		processCancel: p.cancel,
		stdin:         p.stdin,
		cmd:           p.cmd,
		waitDone:      make(chan struct{}),
	}, nil
}

// lspProcess is the cohesive set of resources produced by spawnLSPProcess.
// It does not store a Context (which would trip the containedctx lint);
// the process-bound context exists only inside spawnLSPProcess where it
// is consumed by the stderr drain goroutine before the function returns.
type lspProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	cancel context.CancelFunc // terminates the process and the stderr drain.
}

// spawnLSPProcess starts the LSP server process, wires up stdio, and
// kicks off a stderr-drain goroutine bound to the process lifetime.
// Errors are mapped to typed lifecycle errors so the supervisor can
// apply the right policy.
func spawnLSPProcess(h *lspHandler) (*lspProcess, error) {
	// The process must outlive the caller's request context (which is
	// often cancelled when an HTTP/agent turn ends). The supervisor
	// calls Close to shut it down on Stop or restart.
	processCtx, processCancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(processCtx, h.command, h.args...)
	cmd.Env = append(os.Environ(), h.env...)
	cmd.Dir = h.workingDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		processCancel()
		return nil, fmt.Errorf("%w: stdin pipe: %w", lifecycle.ErrServerUnavailable, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		processCancel()
		return nil, fmt.Errorf("%w: stdout pipe: %w", lifecycle.ErrServerUnavailable, err)
	}
	stderrBuf := &concurrent.Buffer{}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		processCancel()
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", lifecycle.ErrServerUnavailable, err)
		}
		return nil, fmt.Errorf("failed to start LSP server: %w", err)
	}

	// Drain stderr until the process context is cancelled. Started here
	// so the process-bound ctx never leaks out of this function.
	go h.readNotifications(processCtx, stderrBuf)

	return &lspProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		cancel: processCancel,
	}, nil
}

// runHandshake performs the LSP initialize/initialized exchange under
// h.mu. Honours ctx cancellation by closing stdin if the caller goes
// away during the handshake (e.g. user pressed Ctrl-C during Start).
func (h *lspHandler) runHandshake(ctx context.Context, stdin io.WriteCloser) error {
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = stdin.Close()
		case <-handshakeDone:
		}
	}()
	defer close(handshakeDone)

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.initializeLocked()
}

// publishSession atomically writes session state to the handler under
// h.mu so per-request methods see consistent fields. Open-files state is
// reset because a fresh server has no knowledge of previously-opened
// files.
func (h *lspHandler) publishSession(cmd *exec.Cmd, stdin io.WriteCloser, stdout *bufio.Reader, cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmd = cmd
	h.cancel = cancel
	h.stdin = stdin
	h.stdout = stdout
	h.initialized.Store(false)
	h.capabilities = nil
	h.serverInfo = nil
	h.openFilesMu.Lock()
	h.openFiles = make(map[string]int)
	h.openFilesMu.Unlock()
}

// clearSession is the inverse of publishSession: it nils all session
// state on h. Called by Connect on handshake failure and by Close on
// teardown.
func (h *lspHandler) clearSession() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearSessionLocked()
}

// clearSessionLocked is the inner half of clearSession; the caller must
// already hold h.mu. Used by Close so the shutdown handshake and the
// state nilling happen atomically (no concurrent per-request method can
// slip in and write to a soon-to-be-closed stdin).
//
// initialized is cleared BEFORE the field nilling so a concurrent
// ensureInitialized fast-path (which only reads the atomic) sees
// initialized=false and falls through to the slow path that re-checks
// under h.mu.
func (h *lspHandler) clearSessionLocked() {
	h.initialized.Store(false)
	h.cmd = nil
	h.cancel = nil
	h.stdin = nil
	h.stdout = nil
	h.capabilities = nil
	h.serverInfo = nil
	h.openFilesMu.Lock()
	h.openFiles = make(map[string]int)
	h.openFilesMu.Unlock()
}

// fireToolsChanged invokes the registered tools-changed handler if any,
// outside h.mu so the runtime is free to call back into the toolset.
func (h *lspHandler) fireToolsChanged() {
	h.mu.Lock()
	handler := h.toolsChangedHandler
	h.mu.Unlock()
	if handler != nil {
		handler()
	}
}

// lspSession is a single live LSP server session. cmd.Wait must be called
// exactly once per *exec.Cmd; both Wait and Close can race to be the
// caller. A shared sync.Once runs the wait body in one goroutine and
// exposes the result via the pre-allocated waitDone channel; both Wait
// and Close block on waitDone to observe the process exit.
type lspSession struct {
	h             *lspHandler
	processCancel context.CancelFunc
	stdin         io.WriteCloser
	cmd           *exec.Cmd // captured at construction; never nilled by handler teardown.

	mu     sync.Mutex
	closed bool

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{} // pre-allocated in Connect; closed by the wait goroutine.
}

// Wait blocks until the LSP process exits. Safe to call concurrently with
// Close.
func (s *lspSession) Wait() error {
	s.startWait()
	<-s.waitDone
	return s.waitErr
}

// startWait launches a single goroutine to drive cmd.Wait. Idempotent
// via sync.Once; both Wait and Close call it.
func (s *lspSession) startWait() {
	s.waitOnce.Do(func() {
		go func() {
			defer close(s.waitDone)
			err := s.cmd.Wait()
			if err == nil {
				return
			}
			// An *exec.ExitError after a signal-induced shutdown
			// (Close → cancel) is expected; treat it as a clean exit
			// so the supervisor only restarts on real crashes.
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.waitErr = fmt.Errorf("%w: %w", lifecycle.ErrServerCrashed, err)
		}()
	})
}

// Close performs the LSP shutdown handshake and tears down the process.
// Idempotent; safe to call concurrently with Wait.
func (s *lspSession) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	slog.DebugContext(ctx, "Stopping LSP server")

	// Hold h.mu across the entire teardown (shutdown handshake AND state
	// clearing) so a concurrent per-request method can't slip in between
	// the two and write to a soon-to-be-closed stdin while the server
	// has already received "exit".
	s.h.mu.Lock()
	if s.h.initialized.Load() {
		// Best-effort: the process is going away regardless.
		_, _ = s.h.sendRequestLocked("shutdown", nil)
		_ = s.h.sendNotificationLocked("exit", nil)
	}
	s.h.clearSessionLocked()
	s.h.mu.Unlock()

	_ = s.stdin.Close()
	s.processCancel()

	// Block until cmd.Wait completes (in startWait's goroutine) so Close
	// returns only after the OS process is reaped.
	s.startWait()
	<-s.waitDone

	if ctx.Err() != nil {
		return nil
	}
	slog.DebugContext(ctx, "LSP server stopped")
	return nil
}
