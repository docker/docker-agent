package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Describer can be implemented by a ToolSet to provide a short, user-visible
// description that uniquely identifies the toolset instance (e.g. for use in
// error messages and warnings). The string must never contain secrets.
type Describer interface {
	Describe() string
}

// DescribeToolSet returns a short description for ts suitable for user-visible
// messages. It walks the wrapper chain (e.g. through WithName /
// StartableToolSet) so any inner Describer is reachable; falls back to
// the Go type name when no inner toolset implements Describer.
func DescribeToolSet(ts ToolSet) string {
	if d, ok := As[Describer](ts); ok {
		if desc := d.Describe(); desc != "" {
			return desc
		}
	}
	// Unwrap once for the type-name fallback so wrappers don't show up
	// as e.g. "*tools.namedToolSet".
	if u, ok := ts.(Unwrapper); ok {
		ts = u.Unwrap()
	}
	return fmt.Sprintf("%T", ts)
}

// failureStreak implements once-per-streak warning de-duplication. A streak
// begins on the first fail() and ends on reset() (a success or a Stop).
// shouldReport() returns true exactly once per streak — for the first failure —
// so repeated failures don't re-queue duplicate warnings.
type failureStreak struct {
	active  bool // true between the first failure and the next reset
	pending bool // true if the current streak's first failure is unreported
}

func (f *failureStreak) fail() {
	if !f.active {
		f.active = true
		f.pending = true
	}
}

func (f *failureStreak) reset() {
	f.active = false
	f.pending = false
}

func (f *failureStreak) shouldReport() bool {
	if !f.pending {
		return false
	}
	f.pending = false
	return true
}

// PartialStartError is returned from Start by composite toolsets (toolsets
// aggregating several inner toolsets, e.g. Code Mode) when only part of
// their inner toolsets came up. StartableToolSet treats it specially: the
// wrapper is latched as started so the healthy subset stays listed and
// usable, while the error still propagates so callers can warn about the
// failed subset. The composite must also implement StartReporter, returning
// false while degraded, so the next Start call takes the recovery path and
// retries the failed inner toolsets.
//
// PartialStartError is only for genuinely partial outcomes: a composite
// whose inner toolsets all failed should return a TotalStartError instead,
// so the wrapper stays unlatched and the next Start is a cold retry.
//
// Use NewPartialStartError so AuthOnly is classified from the causes.
type PartialStartError struct {
	// Err aggregates the individual inner-toolset failures (usually an
	// errors.Join of one error per failed toolset).
	Err error
	// AuthOnly is true when every individual cause is an
	// authorization-required deferral (see IsAuthorizationRequired). A mixed
	// batch (auth + real failure) must stay false so the non-auth cause is
	// surfaced as a failure instead of being hidden behind the silent
	// auth-deferral handling.
	AuthOnly bool
	// LostAfterStart is true when at least one failed inner toolset had
	// previously started successfully and was lost since (died in the
	// background or failed to recover). The composite sets it so
	// StartableToolSet can tell a real recovery failure — worth the targeted
	// re-auth notice — from a retried initial failure (e.g. an OAuth
	// deferral), which must stay silent across turns even though the
	// wrapper latched started on an earlier partial start.
	LostAfterStart bool
}

// allCausesAuthorizationRequired reports whether there is at least one
// non-nil cause and every one of them classifies as authorization-required.
// Aggregate start errors must be classified with these all-causes semantics:
// matching the batch through plain errors.As (ANY semantics) would let a
// single OAuth deferral hide the real failures joined next to it behind the
// silent auth-deferral handling.
func allCausesAuthorizationRequired(causes []error) bool {
	authOnly := false
	for _, cause := range causes {
		if cause == nil {
			continue
		}
		if !IsAuthorizationRequired(cause) {
			return false
		}
		authOnly = true
	}
	return authOnly
}

// NewPartialStartError joins the given per-toolset start failures and
// classifies the batch as auth-only when every (non-nil) cause reports
// IsAuthorizationRequired. LostAfterStart is left false: the composite must
// set it afterwards when a failed inner had already started successfully.
func NewPartialStartError(causes ...error) *PartialStartError {
	return &PartialStartError{Err: errors.Join(causes...), AuthOnly: allCausesAuthorizationRequired(causes)}
}

func (e *PartialStartError) Error() string {
	if e == nil || e.Err == nil {
		return "partial toolset start failure"
	}
	return e.Err.Error()
}

// Unwrap exposes the underlying error(s) to errors.Is/errors.As, e.g. so
// IsAuthorizationRequired can detect a deferred-OAuth inner toolset.
func (e *PartialStartError) Unwrap() error { return e.Err }

// IsPartialStart reports whether err (or any error wrapped by it) signals
// that a composite toolset started with only part of its inner toolsets
// available.
func IsPartialStart(err error) bool {
	var target *PartialStartError
	return errors.As(err, &target)
}

// TotalStartError is the total-failure counterpart of PartialStartError,
// returned from Start by composite toolsets when every inner toolset
// failed. It is deliberately not a PartialStartError: with no healthy
// subset to preserve, StartableToolSet must not latch the wrapper as
// started, so the composite's own tool is not listed and the next Start
// retries from cold.
//
// The dedicated type exists so IsAuthorizationRequired applies the same
// all-causes classification as for partial failures: a bare errors.Join
// would match a single OAuth deferral via errors.As (ANY semantics) and
// silently suppress the real failures joined next to it.
//
// Use NewTotalStartError so AuthOnly is classified from the causes.
type TotalStartError struct {
	// Err aggregates the individual inner-toolset failures (usually an
	// errors.Join of one error per failed toolset).
	Err error
	// AuthOnly is true when every individual cause is an
	// authorization-required deferral — same contract as
	// PartialStartError.AuthOnly.
	AuthOnly bool
}

// NewTotalStartError joins the given per-toolset start failures and
// classifies the batch as auth-only when every (non-nil) cause reports
// IsAuthorizationRequired.
func NewTotalStartError(causes ...error) *TotalStartError {
	return &TotalStartError{Err: errors.Join(causes...), AuthOnly: allCausesAuthorizationRequired(causes)}
}

func (e *TotalStartError) Error() string {
	if e == nil || e.Err == nil {
		return "total toolset start failure"
	}
	return e.Err.Error()
}

// Unwrap exposes the underlying error(s) to errors.Is/errors.As so the
// individual causes stay reachable.
func (e *TotalStartError) Unwrap() error { return e.Err }

// lifecycleMutex is a mutex built on a 1-buffered channel: the lock is held
// while the buffer is full. Unlike sync.Mutex, acquisition can be bounded by
// a context, which the shutdown path needs when an in-flight Start ignores
// cancellation and never releases the lock (#4001). The zero value is ready
// to use; like sync.Mutex, a lifecycleMutex must not be copied after first
// use.
type lifecycleMutex struct {
	once sync.Once
	ch   chan struct{}
}

// init makes the zero value usable; every method calls it before touching ch.
func (m *lifecycleMutex) init() {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
}

func (m *lifecycleMutex) Lock() {
	m.init()
	m.ch <- struct{}{}
}

// LockContext acquires the lock unless ctx ends first, in which case it
// returns ctx.Err() without acquiring. An uncontended lock is always
// acquired, even when ctx is already done, so a shutdown arriving with an
// expired deadline still stops responsive toolsets.
func (m *lifecycleMutex) LockContext(ctx context.Context) error {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return nil
	default:
	}
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryLock acquires the lock only when it is immediately available.
func (m *lifecycleMutex) TryLock() bool {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *lifecycleMutex) Unlock() {
	m.init()
	select {
	case <-m.ch:
	default:
		panic("tools: Unlock of an unlocked lifecycleMutex")
	}
}

// StartableToolSet wraps a ToolSet with lazy, single-flight start semantics.
// This is the canonical way to manage toolset lifecycle.
//
// It also de-duplicates failure warnings: when Start() fails repeatedly
// (e.g. an MCP server is down), only the *first* failure of each streak is
// reported via ShouldReportFailure(). A successful Start() automatically
// clears the streak, so a future failure is again reported as fresh — no
// caller-visible "recovery" event is needed. The same once-per-streak guard
// applies to Tools() listing failures via ShouldReportListFailure(); a remote
// MCP server stuck returning "toolset not started" therefore surfaces a single
// warning per streak instead of one on every conversation turn.
type StartableToolSet struct {
	ToolSet

	// mu serializes lifecycle operations (Start/Stop) and guards started
	// and the failure streaks. TryStart and TryIsStarted use TryLock to
	// detect an in-flight lifecycle operation without blocking (#4001);
	// StopIfStarted uses LockContext so shutdown deadlines are honored even
	// when an in-flight Start ignores cancellation and keeps the lock past
	// them. Every holder must release through unlock — never mu.Unlock
	// directly — so a stop request abandoned by a timed-out StopIfStarted
	// is consumed by whichever holder releases the lock next.
	mu      lifecycleMutex
	started bool

	// stopRequestMu guards the stop request that StopIfStarted publishes
	// before waiting for mu. A requester that times out waiting leaves the
	// request behind for the current holder's unlock handshake, so it can
	// neither go stale (leaking a started toolset) nor linger to reap a
	// start a later caller deliberately performs. Lock order: mu may be
	// held when taking stopRequestMu; stopRequestMu is never held while
	// acquiring mu.
	stopRequestMu  sync.Mutex
	stopRequested  bool
	stopRequestCtx context.Context //nolint:containedctx // carries the requester's values into a late stop; set iff stopRequested

	startStreak failureStreak // Start() failures
	listStreak  failureStreak // Tools() listing failures
	// recoveryStreak tracks once-per-streak notices specifically for
	// recovery failures (the toolset was previously started and working,
	// then Start failed again). Distinct from startStreak so callers can
	// emit a different, more targeted message (e.g. "needs re-auth" vs
	// "start failed") for the recovery case.
	recoveryStreak failureStreak
}

// NewStartable wraps a ToolSet for lazy initialization.
func NewStartable(ts ToolSet) *StartableToolSet {
	return &StartableToolSet{ToolSet: ts}
}

// IsStarted returns whether the toolset has been successfully started.
// For toolsets that don't implement Startable, this always returns true.
// It waits for any in-flight lifecycle operation (Start/Stop) to settle,
// so callers deciding whether a Stop is needed observe the final state;
// use TryIsStarted on paths that must never block. Releasing the lifecycle
// lock on return can settle a pending StopIfStarted request, so a call may
// invoke — and block on — the underlying Stop.
func (s *StartableToolSet) IsStarted() bool {
	s.mu.Lock()
	defer s.unlock()
	return s.started
}

// TryIsStarted is the non-blocking variant of IsStarted: when another
// lifecycle operation (Start/Stop) holds the single-flight lock it returns
// false immediately instead of waiting for it to settle. false therefore
// means "unstarted, or a lifecycle operation is in flight". It exists for
// callers that must skip a toolset that isn't immediately ready (e.g.
// collecting tools for a turn) rather than stall behind a wedged Start
// (#4001); callers that need the settled state must use IsStarted.
func (s *StartableToolSet) TryIsStarted() bool {
	if !s.mu.TryLock() {
		return false
	}
	started := s.started
	// The release handshake may consume a pending StopIfStarted request and
	// stop the toolset this probe just observed started: report a reaped
	// start as not-ready rather than as started.
	if s.unlock() {
		return false
	}
	return started
}

// Start starts the toolset with single-flight semantics.
// Concurrent callers block until the start attempt completes.
// If start fails, a future call will retry.
// If the underlying toolset doesn't implement Startable, this is a no-op.
func (s *StartableToolSet) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.unlock()
	return s.startLocked(ctx)
}

// TryStart is the non-blocking variant of Start for turn-critical callers:
// it never joins an in-flight lifecycle operation. When another Start or
// Stop holds the single-flight lock it returns (false, nil) immediately —
// start already in progress, not ready yet — without recording a failure
// streak. Otherwise it runs exactly the same start logic as Start:
// (true, nil) when the toolset is started (freshly or already latched),
// (false, err) when the attempt ran and failed, and (false, nil) also when
// a pending shutdown (StopIfStarted) reaped the toolset within the attempt.
func (s *StartableToolSet) TryStart(ctx context.Context) (started bool, err error) {
	if !s.mu.TryLock() {
		return false, nil
	}
	// Return values are computed before deferred calls run, and the release
	// handshake in unlock may consume a pending shutdown request and stop
	// the toolset this attempt just started: re-check after the release so
	// a reaped start is reported as (false, nil), not as started.
	defer func() {
		s.unlock()
		if started {
			started = s.TryIsStarted()
		}
	}()
	if err := s.startLocked(ctx); err != nil {
		return false, err
	}
	return s.started, nil
}

// DefaultStartTimeout bounds a toolset start when the caller supplies no
// budget of its own. It is shared by the agent's turn path and the runtime's
// startup probe so both give up on a wedged toolset after the same grace
// period; it is deliberately generous because a cold start can legitimately
// include an image pull.
const DefaultStartTimeout = 30 * time.Second

// TryStartWithTimeout runs TryStart bounded by timeout (DefaultStartTimeout
// when non-positive). The attempt runs in its own goroutine raced against
// the bound — not only under the derived context — because a wedged toolset
// can ignore cancellation (the MCP connector detaches the context it is
// handed). Outcomes:
//
//   - (true, nil): the toolset is started (freshly, latched or recovered).
//   - (false, nil): skipped — another lifecycle operation holds the
//     single-flight lock, or a pending shutdown reaped the fresh start.
//   - (false, ctx.Err() of the bound): the attempt outlived the budget and
//     was abandoned. It keeps running in the background holding the
//     single-flight lock, so later Start/Stop calls wait for it rather than
//     race it; if it eventually completes, a later call picks the toolset up.
//   - (false, err): the attempt ran and failed with err.
func (s *StartableToolSet) TryStartWithTimeout(ctx context.Context, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = DefaultStartTimeout
	}
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type startResult struct {
		started bool
		err     error
	}
	done := make(chan startResult, 1) // buffered so a late send never blocks
	go func() {
		started, err := s.TryStart(startCtx)
		done <- startResult{started: started, err: err}
	}()

	select {
	case <-startCtx.Done():
		return false, startCtx.Err()
	case res := <-done:
		return res.started, res.err
	}
}

// startLocked implements the start sequence shared by Start and TryStart.
// s.mu must be held.
func (s *StartableToolSet) startLocked(ctx context.Context) (err error) {
	recovering := false
	if s.started {
		if reporter, ok := As[StartReporter](s.ToolSet); !ok || reporter.IsStarted() {
			return nil
		}
		s.started = false
		recovering = true
	}

	// Span the toolset startup — MCP handshake, OAuth probes,
	// tool discovery, etc. can take seconds to minutes and the
	// "tools loading…" UI was previously unattributable. Only
	// fires when the toolset has work to do (Restartable on a
	// recovering run, or Startable on a cold start); cheap
	// toolsets without either skip the span entirely.
	//
	// Unwrap once so the kind attribute names the underlying toolset
	// (e.g. *mcp.Toolset, *builtin.ShellTool) instead of the
	// *tools.namedToolSet wrapper that every toolset gets in the
	// registry — same pattern DescribeToolSet uses.
	inner := s.ToolSet
	if u, ok := inner.(Unwrapper); ok {
		inner = u.Unwrap()
	}
	if restarter, hasRestarter := As[Restartable](s.ToolSet); recovering && hasRestarter {
		ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/tools").Start(
			ctx,
			"toolset.start",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String("cagent.toolset.kind", fmt.Sprintf("%T", inner))),
		)
		defer func() {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}()
		if err := restarter.Restart(ctx); err != nil {
			s.startStreak.fail()
			s.recoveryStreak.fail()
			return err
		}
	} else if startable, ok := As[Startable](s.ToolSet); ok {
		ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/tools").Start(
			ctx,
			"toolset.start",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String("cagent.toolset.kind", fmt.Sprintf("%T", inner))),
		)
		defer func() {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}()
		if err := startable.Start(ctx); err != nil {
			s.startStreak.fail()
			var partial *PartialStartError
			if errors.As(err, &partial) {
				// A partial start still latches started: the composite's
				// healthy inner toolsets must stay listed and usable, and
				// its StartReporter keeps returning false while degraded,
				// so the failed subset is retried on the next Start.
				s.started = true
				// The latch makes every later Start a recovery run, so
				// recovering alone cannot tell an inner that was started
				// and lost from one that never came up (e.g. an initial
				// OAuth deferral retried each turn, which must stay
				// silent). Only actual post-start loss, as classified by
				// the composite, marks the recovery streak.
				if partial.LostAfterStart {
					s.recoveryStreak.fail()
				}
			} else if recovering {
				// A failed recovery marks the recovery streak here too, not
				// only in the Restartable branch above: toolsets recovering
				// through plain Start (a StartReporter without Restartable,
				// or a composite whose inner toolsets all went down) need
				// the targeted re-auth notice as well.
				s.recoveryStreak.fail()
			}
			return err
		}
	}

	// Successful start: clear the streak so any future failure is reported
	// as fresh. This is the recovery path — it is intentionally silent.
	s.started = true
	s.startStreak.reset()
	s.recoveryStreak.reset()
	return nil
}

// Tools lists the underlying toolset's tools and tracks listing-failure
// streaks so callers can de-duplicate warnings via ShouldReportListFailure().
// A successful listing clears the streak so a future failure is reported as
// fresh. Releasing the lifecycle lock on return can settle a pending
// StopIfStarted request, so a call may invoke — and block on — the
// underlying Stop.
func (s *StartableToolSet) Tools(ctx context.Context) ([]Tool, error) {
	ta, err := s.ToolSet.Tools(ctx)

	s.mu.Lock()
	defer s.unlock()
	if err != nil {
		s.listStreak.fail()
		return nil, err
	}

	s.listStreak.reset()
	return ta, nil
}

// Stop stops the toolset if it implements Startable and resets
// the started flag so that a subsequent Start will re-initialize.
func (s *StartableToolSet) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.unlock()
	return s.stopLocked(ctx)
}

// StopIfStarted stops the toolset only when it has been started, waiting —
// bounded by ctx — for an in-flight lifecycle operation to settle first.
// It is the shutdown-path variant of Stop:
//
//   - When an in-flight Start settles before ctx is done, check-then-stop
//     runs atomically under the lifecycle lock: a start that completes in
//     time is stopped, never leaked, and a never-started toolset is left
//     untouched.
//   - When ctx ends first (a wedged Start ignoring cancellation, #4001), it
//     returns ctx.Err() without blocking further. The published request
//     stays pending and is consumed by the current lock holder's release
//     handshake (see unlock), which stops the toolset when that holder
//     leaves it started — so a start that settles after the deadline is
//     still reaped, and a later deliberate start is never affected.
func (s *StartableToolSet) StopIfStarted(ctx context.Context) error {
	// Publish the request before waiting so it cannot be lost to a holder
	// that releases the lock only after the deadline below.
	s.stopRequestMu.Lock()
	s.stopRequested = true
	s.stopRequestCtx = ctx
	s.stopRequestMu.Unlock()

	if err := s.mu.LockContext(ctx); err != nil {
		return err
	}
	defer s.unlock()

	// Claim the request — ours, or a concurrent caller's; one stop settles
	// both — so it cannot linger past this shutdown.
	s.stopRequestMu.Lock()
	s.stopRequested = false
	s.stopRequestCtx = nil
	s.stopRequestMu.Unlock()

	if !s.started {
		return nil
	}
	return s.stopLocked(ctx)
}

// stopLocked implements the stop sequence shared by Stop, StopIfStarted and
// the unlock release handshake; s.mu must be held.
func (s *StartableToolSet) stopLocked(ctx context.Context) error {
	s.started = false
	s.startStreak.reset()
	s.listStreak.reset()
	s.recoveryStreak.reset()
	if startable, ok := As[Startable](s.ToolSet); ok {
		return startable.Stop(ctx)
	}
	return nil
}

// unlock releases the lifecycle lock after a handshake that settles any
// pending StopIfStarted request — no matter whether this holder was Start,
// Tools, a reporter or Stop: while still holding mu it consumes the request
// and, when the toolset is started, stops it. The requester may have timed
// out and returned long ago, so the stop runs under context.WithoutCancel
// of the request ctx and a failure can only be logged. It reports whether
// any request was settled, so non-blocking probes (TryIsStarted) can avoid
// reporting a started toolset this very release just reaped.
//
// Each round settles one request; the lock is released by the round that
// finds none. Re-checking after a stop keeps stopRequestMu holds brief
// (never across stopLocked) while still consuming a request published
// during that stop, whose publisher may have timed out behind it.
func (s *StartableToolSet) unlock() (settled bool) {
	for !s.settleStopRequestAndRelease() {
		settled = true
	}
	return settled
}

// settleStopRequestAndRelease performs one round of the unlock handshake:
// when no stop request is pending it releases the lifecycle lock and reports
// true; otherwise it consumes the request, stops a started toolset on the
// requester's behalf, and reports false so unlock checks again.
func (s *StartableToolSet) settleStopRequestAndRelease() bool {
	s.stopRequestMu.Lock()
	if !s.stopRequested {
		// Releasing mu before stopRequestMu closes the publish/consume
		// race: a request published after this check finds mu free, so the
		// requester's own LockContext — or the next holder's unlock —
		// settles it.
		s.mu.Unlock()
		s.stopRequestMu.Unlock()
		return true
	}
	s.stopRequested = false
	ctx := context.WithoutCancel(s.stopRequestCtx)
	s.stopRequestCtx = nil
	s.stopRequestMu.Unlock()
	if s.started {
		if err := s.stopLocked(ctx); err != nil {
			slog.WarnContext(ctx, "Failed to stop toolset for a pending shutdown request", "toolset", DescribeToolSet(s), "error", err)
		}
	}
	return false
}

// ShouldReportFailure returns true exactly once per failure streak — after
// the first failed Start() and before the streak ends (a successful
// Start() or Stop()). Subsequent calls return false until a new streak
// begins. Calling it when no failure is pending always returns false.
func (s *StartableToolSet) ShouldReportFailure() bool {
	s.mu.Lock()
	defer s.unlock()
	return s.startStreak.shouldReport()
}

// ShouldReportListFailure returns true exactly once per Tools() listing-failure
// streak — after the first failed listing and before the streak ends (a
// successful Tools() or Stop()). Subsequent calls return false until a new
// streak begins. Calling it when no failure is pending always returns false.
func (s *StartableToolSet) ShouldReportListFailure() bool {
	s.mu.Lock()
	defer s.unlock()
	return s.listStreak.shouldReport()
}

// ShouldReportRecoveryFailure returns true exactly once per recovery-failure
// streak — when a toolset that was previously started and working fails to
// restart (e.g. because the server revoked the OAuth token in the background).
//
// Unlike ShouldReportFailure (which fires for both initial and recovery
// failures), this method fires only for recovery failures so callers can
// emit a targeted "needs re-authentication" notice instead of a generic
// "start failed" one. Returns false for initial-startup auth deferral
// (those are silent pending prompts and the dialog appears naturally on
// the first interactive turn).
func (s *StartableToolSet) ShouldReportRecoveryFailure() bool {
	s.mu.Lock()
	defer s.unlock()
	return s.recoveryStreak.shouldReport()
}

// Unwrap returns the underlying ToolSet.
func (s *StartableToolSet) Unwrap() ToolSet {
	return s.ToolSet
}

// Unwrapper is implemented by toolset wrappers that decorate another ToolSet.
// This allows As to walk the wrapper chain and find inner capabilities.
type Unwrapper interface {
	Unwrap() ToolSet
}

// As performs a type assertion on a ToolSet, walking the wrapper chain if needed.
// It checks the outermost toolset first, then recursively unwraps through any
// Unwrapper implementations (including StartableToolSet and decorator wrappers)
// until it finds a match or reaches the end of the chain.
//
// Example:
//
//	if pp, ok := tools.As[tools.PromptProvider](toolset); ok {
//	    prompts, _ := pp.ListPrompts(ctx)
//	}
func As[T any](ts ToolSet) (T, bool) {
	for ts != nil {
		if result, ok := ts.(T); ok {
			return result, true
		}
		if u, ok := ts.(Unwrapper); ok {
			ts = u.Unwrap()
		} else {
			break
		}
	}
	var zero T
	return zero, false
}
