package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Connector creates new sessions for a Supervisor. Implementations are
// transport-specific: stdio MCP, remote MCP, LSP stdio.
type Connector interface {
	// Connect establishes a new underlying connection (e.g. spawns a
	// process, dials HTTP, runs the initialize handshake). The returned
	// Session is owned by the supervisor; the supervisor calls Close on
	// it. Errors should be classified via Classify so the supervisor can
	// apply policy via errors.Is.
	Connect(ctx context.Context) (Session, error)
}

// Session is the supervisor's view of an active connection. Wait blocks
// until the session ends; Close terminates it. Close must be idempotent
// and safe to call concurrently with an in-flight Wait.
type Session interface {
	Wait() error
	Close(ctx context.Context) error
}

// Restart controls how the Supervisor reacts to an unexpected disconnect.
type Restart int

const (
	// RestartOnFailure reconnects after a non-nil Wait result or a forced
	// reconnect via RestartAndWait. Default; matches historical mcp.Toolset.
	RestartOnFailure Restart = iota
	// RestartNever transitions to Failed when the session ends.
	RestartNever
	// RestartAlways reconnects even after a clean (nil) Wait result.
	RestartAlways
)

// Backoff parameters for restart attempts. Zero values default to
// 1s..32s exponential (matching historical MCP behaviour).
type Backoff struct {
	Initial    time.Duration // first wait (default 1s)
	Max        time.Duration // cap (default 32s)
	Multiplier float64       // (default 2.0)
	Jitter     float64       // 0..1 fraction; 0 disables (default)
}

// delay returns the wait time before attempt n (0-based).
func (b Backoff) delay(attempt int, randFloat func() float64) time.Duration {
	initial := b.Initial
	if initial <= 0 {
		initial = time.Second
	}
	mul := b.Multiplier
	if mul <= 0 {
		mul = 2
	}
	maxDelay := b.Max
	if maxDelay <= 0 {
		maxDelay = 32 * time.Second
	}
	d := min(time.Duration(float64(initial)*math.Pow(mul, float64(attempt))), maxDelay)
	if b.Jitter > 0 {
		j := min(b.Jitter, 1.0)
		offset := (randFloat()*2 - 1) * j * float64(d)
		d = max(time.Duration(float64(d)+offset), 0)
	}
	return d
}

// Policy controls how a Supervisor manages a connection over time. The
// zero value gives the historical mcp.Toolset behaviour: RestartOnFailure,
// 5 attempts, 1s..32s backoff, no jitter, no callbacks.
type Policy struct {
	Restart     Restart // see Restart constants; default RestartOnFailure
	MaxAttempts int     // 0 = default (5); negative = unlimited
	Backoff     Backoff // zero fields use Backoff defaults

	// OnDisconnect is called when the session ends, with Wait()'s result.
	// Useful for cache invalidation.
	OnDisconnect func(err error)
	// OnRestart is called after each successful reconnect. Useful for
	// re-fetching server-side state (tools, prompts).
	OnRestart func()
	// OnFailed is called once when the supervisor enters StateFailed.
	OnFailed func(err error)

	// Logger is used for lifecycle logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// maxAttempts resolves MaxAttempts to an effective value: 0 → default 5,
// negative → unlimited (returned as -1), positive → as configured.
func (p Policy) maxAttempts() int {
	if p.MaxAttempts == 0 {
		return 5
	}
	return p.MaxAttempts
}

// logger returns p.Logger or slog.Default if nil.
func (p Policy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// Supervisor manages the lifecycle of a single connection: initial connect,
// watcher goroutine, restart with backoff, graceful Stop. It is the shared
// implementation for MCP (stdio + remote) and LSP transports; per-transport
// behaviour is captured in the Connector.
//
// Supervisor is safe for concurrent use.
type Supervisor struct {
	name      string
	connector Connector
	policy    Policy
	tracker   *Tracker

	// startMu serializes Start so two concurrent first-callers don't both
	// invoke Connector.Connect.
	startMu sync.Mutex

	// mu guards the rest of the fields.
	mu           sync.Mutex
	session      Session
	stopping     bool
	watcherAlive bool
	forceRestart bool          // set by RestartAndWait so the watcher reconnects
	restarted    chan struct{} // closed and replaced on each successful restart
	// done is closed when the supervisor enters a terminal state (Stopped
	// or Failed) so RestartAndWait can wake up promptly. Replaced with a
	// fresh channel by Start when transitioning out of a terminal state.
	done chan struct{}

	// randFloat is the jitter source; tests may override.
	randFloat func() float64
}

// New returns a Supervisor that drives connector with policy. The name is
// used in lifecycle log messages and should uniquely identify the toolset.
func New(name string, connector Connector, policy Policy) *Supervisor {
	return &Supervisor{
		name:      name,
		connector: connector,
		policy:    policy,
		tracker:   NewTracker(),
		randFloat: rand.Float64,
		restarted: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// State returns a snapshot of the supervisor's current state.
func (s *Supervisor) State() StateInfo { return s.tracker.Snapshot() }

// IsReady reports whether the supervisor is in a state that should serve
// requests (Ready or Degraded).
func (s *Supervisor) IsReady() bool { return s.tracker.State().IsUsable() }

// MarkReadyForTesting forces the supervisor into StateReady without going
// through Connect. Test-only backdoor; production code must not call this.
func (s *Supervisor) MarkReadyForTesting() { s.tracker.Set(StateReady) }

// Restarted returns a channel closed the next time the supervisor
// completes a successful restart. The channel is replaced after each
// restart, so callers should re-read it on each new wait.
func (s *Supervisor) Restarted() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarted
}

// Start performs the initial connect. On Connector error the supervisor
// stays in StateStopped and the caller is expected to retry. On success
// the watcher goroutine is launched (if not already alive) and state moves
// to Ready. Concurrent Start calls serialize.
func (s *Supervisor) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()

	s.mu.Lock()
	if s.session != nil {
		s.mu.Unlock()
		return nil
	}
	if s.stopping {
		s.mu.Unlock()
		return ErrNotStarted
	}
	s.mu.Unlock()

	s.tracker.Set(StateStarting)

	sess, err := s.connector.Connect(ctx)
	if err != nil {
		s.tracker.Fail(StateStopped, err)
		return err
	}

	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = sess.Close(context.WithoutCancel(ctx))
		s.tracker.Set(StateStopped)
		return ErrNotStarted
	}
	s.session = sess
	spawnWatcher := !s.watcherAlive
	s.watcherAlive = true
	// Recovering from a terminal state (Failed → Start, or a watcher
	// that previously exited): refresh `done` so RestartAndWait callers
	// don't see a stale close, and clear forceRestart so a leftover flag
	// from a prior session doesn't force-restart this fresh one.
	select {
	case <-s.done:
		s.done = make(chan struct{})
	default:
	}
	s.forceRestart = false
	s.mu.Unlock()

	s.tracker.Set(StateReady)
	s.tracker.ResetRestarts()

	if spawnWatcher {
		// The watcher must outlive ctx; the only way to stop it is Stop.
		go s.watch(context.WithoutCancel(ctx))
	}

	s.policy.logger().Debug("supervisor: ready", "name", s.name)
	return nil
}

// Stop tears the supervisor down. Idempotent. Blocks until the underlying
// session is closed.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	sess := s.session
	s.session = nil
	s.mu.Unlock()

	s.tracker.Set(StateStopped)
	s.signalDone()

	if sess == nil {
		return nil
	}
	if err := sess.Close(context.WithoutCancel(ctx)); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// RestartAndWait closes the current session (if any) so the watcher
// reconnects, then blocks until the next successful reconnect, ctx
// cancellation, supervisor shutdown (Stop or Failed), or timeout.
//
// RestartAndWait does NOT recover from a terminal state (Stopped/Failed):
// callers that want "restart even if Failed" should consult State() and
// call Start when terminal. The Toolset.Restart wrappers in pkg/tools/mcp
// and pkg/tools/builtin do exactly that.
func (s *Supervisor) RestartAndWait(ctx context.Context, timeout time.Duration) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return ErrNotStarted
	}
	restartCh := s.restarted
	doneCh := s.done
	state := s.tracker.State()
	sess := s.session
	s.forceRestart = true
	s.mu.Unlock()

	// Only force-close if currently usable. If the watcher already detected
	// the disconnect, closing now would race with tryRestart.
	if state.IsUsable() && sess != nil {
		_ = sess.Close(context.WithoutCancel(ctx))
	}

	select {
	case <-restartCh:
		return nil
	case <-doneCh:
		// Stop or terminal Failed; surface the supervisor's last error.
		if err := s.tracker.LastError(); err != nil {
			return err
		}
		return ErrNotStarted
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return errors.New("timed out waiting for supervisor reconnect")
	}
}

// signalDone closes the done channel if it is not already closed. Idempotent.
// Takes mu so concurrent Start can replace `done` without racing.
func (s *Supervisor) signalDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// watch runs in a single goroutine for the lifetime of the supervisor,
// from first Start until Stop. It blocks on session.Wait, reacts to
// disconnects, and triggers restarts according to policy.
func (s *Supervisor) watch(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.watcherAlive = false
		s.mu.Unlock()
	}()

	log := s.policy.logger()

	for {
		s.mu.Lock()
		sess := s.session
		s.mu.Unlock()
		if sess == nil {
			return // defensive: shouldn't happen after a successful Start.
		}

		waitErr := sess.Wait()

		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			return
		}
		forced := s.forceRestart
		s.forceRestart = false
		s.session = nil
		s.mu.Unlock()

		s.tracker.Fail(StateRestarting, waitErr)
		log.Warn("supervisor: session lost", "name", s.name, "error", waitErr, "forced", forced)

		if cb := s.policy.OnDisconnect; cb != nil {
			cb(waitErr)
		}

		if !s.shouldRestart(waitErr, forced) {
			s.tracker.Fail(StateFailed, waitErr)
			if cb := s.policy.OnFailed; cb != nil {
				cb(waitErr)
			}
			s.signalDone()
			return
		}

		if !s.tryRestart(ctx) {
			return // tryRestart already set Failed/Stopped as appropriate.
		}

		if cb := s.policy.OnRestart; cb != nil {
			cb()
		}
	}
}

// shouldRestart applies the supervisor's restart policy to decide whether
// the watcher should reconnect after a Wait result. A forced reconnect
// (RestartAndWait) bypasses the policy.
func (s *Supervisor) shouldRestart(err error, forced bool) bool {
	if forced {
		return true
	}
	switch s.policy.Restart {
	case RestartNever:
		return false
	case RestartAlways:
		return true
	default: // RestartOnFailure
		return err != nil && !IsPermanent(err)
	}
}

// tryRestart loops with backoff until reconnect succeeds, ctx is cancelled,
// or the budget is exhausted. Returns true on success (state → Ready),
// false otherwise (state → Failed or Stopped).
func (s *Supervisor) tryRestart(ctx context.Context) bool {
	maxAttempts := s.policy.maxAttempts()
	log := s.policy.logger()

	for attempt := 0; ; attempt++ {
		if maxAttempts > 0 && attempt >= maxAttempts {
			lastErr := s.tracker.LastError()
			log.Error("supervisor: giving up after max attempts", "name", s.name, "attempts", attempt)
			s.tracker.Fail(StateFailed, lastErr)
			if cb := s.policy.OnFailed; cb != nil {
				cb(lastErr)
			}
			s.signalDone()
			return false
		}

		delay := s.policy.Backoff.delay(attempt, s.randFloat)
		log.Debug("supervisor: restart attempt", "name", s.name, "attempt", attempt+1, "backoff", delay)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false
		}

		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			return false
		}
		s.mu.Unlock()

		sess, err := s.connector.Connect(ctx)
		if err != nil {
			s.tracker.Fail(StateRestarting, err)
			s.tracker.IncRestarts()
			log.Warn("supervisor: restart failed", "name", s.name, "attempt", attempt+1, "error", err)
			continue
		}

		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			_ = sess.Close(context.WithoutCancel(ctx))
			return false
		}
		s.session = sess
		// Transition to Ready before unblocking RestartAndWait so callers
		// observe the new state, not a stale Restarting.
		s.tracker.Set(StateReady)
		s.tracker.ResetRestarts()
		close(s.restarted)
		s.restarted = make(chan struct{})
		s.mu.Unlock()

		log.Info("supervisor: restarted", "name", s.name, "attempt", attempt+1)
		return true
	}
}
