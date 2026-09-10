package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// backgroundReconnectKey is a context key that the supervisor attaches to
// connector.Connect calls made during background watcher reconnect attempts,
// distinguishing them from the initial interactive Start. Connector
// implementations (e.g. the MCP clientConnector) use this to apply
// non-interactive constraints on background reconnects so a 401 defers
// cleanly rather than blocking on a dead elicitation bridge.
type backgroundReconnectKey struct{}

// withBackgroundReconnect returns a copy of ctx marked as a background
// reconnect attempt. It is set by tryRestart before calling
// connector.Connect so the connector can distinguish watcher reconnects
// from the initial interactive Start.
func withBackgroundReconnect(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundReconnectKey{}, true)
}

// IsBackgroundReconnect reports whether ctx was created by the supervisor
// for a background reconnect attempt. Connector.Connect implementations can
// use this to disable interactive operations (e.g. OAuth prompts) that
// should not run in the background.
func IsBackgroundReconnect(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundReconnectKey{}).(bool)
	return v
}

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

// CrashLoop configures the supervisor's crash-loop detector: Threshold
// disconnects classified as ErrServerCrashed within Window stop the
// supervisor (state -> Failed, wrapped in ErrCrashLooping) instead of
// restarting again right away, so the caller's own backoff gate (e.g.
// StartableToolSet) paces the next attempt. Zero values default to
// Threshold=3, Window=1 minute.
//
// A single crash, or crashes spread out wider than Window apart, are
// ordinary restarts handled entirely by Restart/Backoff above and never
// trip this detector.
type CrashLoop struct {
	Threshold int
	Window    time.Duration
}

func (c CrashLoop) threshold() int {
	if c.Threshold <= 0 {
		return 3
	}
	return c.Threshold
}

func (c CrashLoop) window() time.Duration {
	if c.Window <= 0 {
		return time.Minute
	}
	return c.Window
}

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

	// StartupTimeout bounds the initial Connect performed by Start. Zero
	// means no timeout. It is enforced by racing Connect against a timer
	// rather than via a context deadline, so it still interrupts connectors
	// (notably MCP) that detach the context they are handed with
	// context.WithoutCancel. On expiry Start returns ErrInitTimeout and the
	// toolset stays in StateStopped so the caller can retry.
	StartupTimeout time.Duration

	// CallTimeout is carried through to the toolset for enforcement on
	// individual tool calls; the Supervisor itself does not use it.
	CallTimeout time.Duration

	// CrashLoop tunes the crash-loop detector (see CrashLoop's doc). Zero
	// value uses the CrashLoop defaults.
	CrashLoop CrashLoop

	// OnDisconnect is called when the session ends, with Wait()'s result.
	// Useful for cache invalidation.
	OnDisconnect func(err error)
	// OnRestart is called after each successful reconnect. Useful for
	// re-fetching server-side state (tools, prompts).
	OnRestart func(ctx context.Context)
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

	// watchDone is closed by the current watcher goroutine. Stop waits on it
	// after closing the session so no transport goroutines are left behind.
	watchDone chan struct{}

	// inflightConnect holds a connector.Connect that a startup timeout left
	// running (see connect). At most one exists at a time; the next Start
	// adopts it and Stop reaps it, so overlapping Connects never race on the
	// shared MCP session.
	inflightConnect *pendingConnect

	// crashTimes holds the timestamps of recent ErrServerCrashed disconnects
	// (watch), pruned to policy.CrashLoop.window() on every record. Cleared
	// whenever crashLoopErr is consumed or set, and on Stop.
	crashTimes []time.Time
	// crashLoopErr is a one-shot report: set by watch when the crash-loop
	// threshold is reached, and returned by the very next Start instead of
	// reconnecting, then cleared so the Start after that attempts a genuine
	// reconnect. The one-shot handshake exists because only the caller (the
	// StartableToolSet backoff gate) knows when enough time has passed to
	// retry for real.
	crashLoopErr error

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

// PendingCrashLoopError returns the crash-loop report awaiting the next
// Start, without consuming it — a peek, unlike Start's one-shot read.
// Repeated calls keep returning the same error until the Start that
// actually consumes it runs. Callers that reach the supervisor outside
// their own backoff gate (e.g. a toolset's lazy per-request start) should
// check this before calling Start, so they fail fast on a known crash
// loop instead of being the one to consume — and reconnect on behalf of
// — a one-shot report the gate hasn't paced yet.
func (s *Supervisor) PendingCrashLoopError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crashLoopErr
}

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
//
// When policy.StartupTimeout is non-zero the connect is bounded by it; on
// expiry Start returns ErrInitTimeout and stays in StateStopped.
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
	// A crash loop detected by watch reports itself here exactly once
	// (see crashLoopErr's doc): the very next Start after this one attempts
	// a genuine reconnect rather than short-circuiting again.
	if err := s.crashLoopErr; err != nil {
		s.crashLoopErr = nil
		s.crashTimes = nil
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	s.tracker.Set(StateStarting)

	sess, err := s.connect(ctx)
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
	if spawnWatcher {
		s.watchDone = make(chan struct{})
	}
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

// connectResult is the outcome of a single connector.Connect call.
type connectResult struct {
	sess Session
	err  error
}

// pendingConnect is a single in-flight connector.Connect whose result is
// adopted by a Start or reaped by Stop, whichever happens first. adopted is
// set exactly once (guarded by Supervisor.mu) so a session is never both
// handed to a caller and closed by the reaper.
type pendingConnect struct {
	done    chan struct{} // closed when Connect returns
	res     connectResult // valid once done is closed
	adopted bool          // set by the first of {Start adopts, Stop reaps}
}

// connect performs connector.Connect bounded by policy.StartupTimeout. When
// the timeout is zero it calls Connect directly.
//
// Otherwise it guarantees at most one Connect is ever in flight: on timeout it
// returns ErrInitTimeout but leaves the goroutine running, recorded in
// s.inflightConnect. A later Start reuses that same goroutine instead of
// launching a concurrent Connect, and Stop reaps it. This matters because the
// MCP transport shares one underlying client across Connect calls, so two
// overlapping Connects would race on the shared session (a late one could
// clobber or close a newer one). The timer (rather than a ctx deadline) is
// deliberate: the MCP connector detaches its ctx with context.WithoutCancel,
// so a deadline on ctx would be stripped before it could interrupt a wedged
// initialize handshake.
//
// A consequence is that a Connect wedged forever is never superseded by a
// fresh attempt (each Start returns ErrInitTimeout). That is intentional:
// spawning a new attempt per turn would accumulate orphaned handshakes and,
// for stdio, leak subprocesses. A handshake that eventually completes is
// adopted by the next Start.
func (s *Supervisor) connect(ctx context.Context) (Session, error) {
	if s.policy.StartupTimeout <= 0 {
		return s.connector.Connect(ctx)
	}

	s.mu.Lock()
	p := s.inflightConnect
	if p == nil {
		p = &pendingConnect{done: make(chan struct{})}
		s.inflightConnect = p
		// Detach from this caller's ctx: the goroutine outlives the Start
		// that launched it (it may be adopted by a later Start after this one
		// times out), so a cancellation of the original ctx must not surface
		// as a stale context.Canceled to the adopter. Matches Stop and watch.
		connectCtx := context.WithoutCancel(ctx)
		go func() {
			sess, err := s.connector.Connect(connectCtx)
			s.mu.Lock()
			defer s.mu.Unlock()
			p.res = connectResult{sess: sess, err: err}
			close(p.done)
		}()
	}
	s.mu.Unlock()

	timer := time.NewTimer(s.policy.StartupTimeout)
	defer timer.Stop()

	select {
	case <-p.done:
		s.mu.Lock()
		if p.adopted {
			// Stop reaped (and closed) this connect while we waited.
			s.mu.Unlock()
			return nil, ErrNotStarted
		}
		p.adopted = true
		if s.inflightConnect == p {
			s.inflightConnect = nil
		}
		res := p.res
		s.mu.Unlock()
		// Defensive: a well-behaved connector returns (sess, nil) or
		// (nil, err), but if it returns both, close the session rather than
		// leak it since Start discards sess when err is non-nil.
		if res.err != nil && res.sess != nil {
			_ = res.sess.Close(context.WithoutCancel(ctx))
			return nil, res.err
		}
		return res.sess, res.err
	case <-timer.C:
		return nil, wrap(ErrInitTimeout, fmt.Errorf("%q did not connect within %s", s.name, s.policy.StartupTimeout))
	}
}

// reapPendingConnect waits for an orphaned Connect (left in flight by a
// startup timeout) to finish and closes its session unless a Start already
// adopted it. Called only from Stop.
func (s *Supervisor) reapPendingConnect(ctx context.Context, p *pendingConnect) {
	<-p.done
	s.mu.Lock()
	if p.adopted {
		s.mu.Unlock()
		return
	}
	p.adopted = true
	sess := p.res.sess
	s.mu.Unlock()
	if sess != nil {
		_ = sess.Close(ctx)
	}
}

// Stop tears the supervisor down. Idempotent. Blocks until the underlying
// session is closed.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopping {
		watchDone := s.watchDone
		s.mu.Unlock()
		return waitForWatcher(ctx, watchDone)
	}
	s.stopping = true
	sess := s.session
	s.session = nil
	watchDone := s.watchDone
	pending := s.inflightConnect
	s.inflightConnect = nil
	// A deliberate Stop discards any pending crash-loop verdict and its
	// history: a future Start begins a fresh window rather than resuming
	// one that spans an intentional stop/start cycle.
	s.crashLoopErr = nil
	s.crashTimes = nil
	s.mu.Unlock()

	s.tracker.Set(StateStopped)
	s.signalDone()

	// Reap a Connect left in flight by a startup timeout so its eventual
	// session is closed rather than leaked. Runs in the background: a wedged
	// handshake must not block Stop.
	if pending != nil {
		go s.reapPendingConnect(context.WithoutCancel(ctx), pending)
	}

	var closeErr error
	if sess != nil {
		closeErr = sess.Close(context.WithoutCancel(ctx))
	}
	waitErr := waitForWatcher(ctx, watchDone)
	if closeErr != nil && ctx.Err() == nil {
		return closeErr
	}
	return waitErr
}

func waitForWatcher(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
		watchDone := s.watchDone
		s.watchDone = nil
		s.mu.Unlock()
		if watchDone != nil {
			close(watchDone)
		}
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
		// Only an actual crash (not a forced/deliberate close, not a clean
		// exit) ever counts toward the loop: those are handled entirely by
		// the ordinary restart policy below.
		crashLooping := !forced && errors.Is(waitErr, ErrServerCrashed) && s.recordCrashLocked(time.Now())
		s.mu.Unlock()

		s.tracker.Fail(StateRestarting, waitErr)
		log.Warn("supervisor: session lost", "name", s.name, "error", waitErr, "forced", forced)

		if cb := s.policy.OnDisconnect; cb != nil {
			cb(waitErr)
		}

		if crashLooping {
			err := wrap(ErrCrashLooping, waitErr)
			s.mu.Lock()
			if s.stopping {
				// Stop won the race (landed between the unlock above and here)
				// and already set StateStopped: let it own the terminal state
				// rather than clobbering it back to Failed.
				s.mu.Unlock()
				return
			}
			s.crashLoopErr = err
			s.mu.Unlock()
			s.tracker.Fail(StateFailed, err)
			log.Error("supervisor: crash-looping; giving up", "name", s.name, "threshold", s.policy.CrashLoop.threshold(), "window", s.policy.CrashLoop.window())
			if cb := s.policy.OnFailed; cb != nil {
				cb(err)
			}
			s.signalDone()
			return
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
			cb(ctx)
		}
	}
}

// recordCrashLocked appends a crash observed at now to the crash-loop
// window, pruning entries older than policy.CrashLoop.window(), and
// reports whether the loop threshold has been reached. s.mu must be held.
func (s *Supervisor) recordCrashLocked(now time.Time) bool {
	cutoff := now.Add(-s.policy.CrashLoop.window())
	kept := s.crashTimes[:0]
	for _, t := range s.crashTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.crashTimes = append(kept, now)
	return len(s.crashTimes) >= s.policy.CrashLoop.threshold()
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
		// Clean exits are not failures for stdio MCP and LSP child processes.
		// Remote MCP streams that should reconnect after an idle clean close opt
		// into RestartAlways when their supervisor policy is constructed.
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

		sess, err := s.connector.Connect(withBackgroundReconnect(ctx))
		if err != nil {
			// A permanent error on reconnect (e.g. ErrAuthRequired from a
			// server-side invalid_token) must not be retried: doing so would
			// burn through the budget and mask the real failure. Symmetric
			// with the shouldRestart check on the Wait() path.
			if IsPermanent(err) {
				log.Warn("supervisor: permanent error on reconnect; not retrying", "name", s.name, "error", err)
				s.tracker.Fail(StateFailed, err)
				if cb := s.policy.OnFailed; cb != nil {
					cb(err)
				}
				s.signalDone()
				return false
			}
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
