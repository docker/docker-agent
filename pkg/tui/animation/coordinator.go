// Package animation provides centralized animation tick management for the TUI.
// All animated components (spinners, fades, etc.) share a single tick stream
// to avoid tick storms and ensure synchronized animations.
//
// Thread safety: All exported functions are safe for concurrent use, though the
// typical usage pattern is single-threaded via Bubble Tea's Update loop.
package animation

import (
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TickMsg is broadcast to all animated components on each animation frame.
// Components should handle this message to update their animation state.
type TickMsg struct {
	// Frame is retained for compatibility with components that have not yet
	// adopted elapsed-time animation. Program runtimes use their accepted tick
	// count; legacy standalone ticks use the compatibility coordinator count.
	Frame int

	runtimeIdentity *runtimeIdentity
	generation      int // identifies the current tick chain so stale or parallel chains are rejected

	// deliveredAt is the timestamp tea.Tick supplies when this timer fires.
	deliveredAt time.Time
	// timerStartedAt records when the animation runtime created this tick's timer. It is
	// the first tick's elapsed-time baseline; later ticks use lastDeliveredAt.
	timerStartedAt    time.Time
	dirty             *atomic.Bool
	elapsedBeforeTick time.Duration
	elapsedAfterTick  time.Duration
}

// MarkDirty records that this accepted tick changed visible output. TickMsg
// copies share the marker, so root can decide after complete fanout whether a
// new view must be composed.
func (m TickMsg) MarkDirty() {
	if m.dirty != nil {
		m.dirty.Store(true)
	}
}

// Dirty reports whether any visible component marked this accepted tick dirty.
func (m TickMsg) Dirty() bool { return m.dirty != nil && m.dirty.Load() }

// ElapsedBounds returns the animation clock immediately before and after this
// accepted tick.
func (m TickMsg) ElapsedBounds() (time.Duration, time.Duration) {
	return m.elapsedBeforeTick, m.elapsedAfterTick
}

// Scheduler provides the wall clock and delayed message delivery used by a
// runtime. Embedders may supply a deterministic scheduler while preserving the
// exact production tick lease and acceptance path.
type Scheduler interface {
	Now() time.Time
	Tick(delay time.Duration, createMsg func(time.Time) tea.Msg) tea.Cmd
}

type wallScheduler struct{}

func (wallScheduler) Now() time.Time                                          { return time.Now() }
func (wallScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd { return tea.Tick(d, f) }

type Runtime struct {
	mu              sync.Mutex
	runtimeIdentity *runtimeIdentity
	elapsed         time.Duration
	active          int32
	paused          bool
	generation      int

	tickScheduled   bool
	lastDeliveredAt time.Time
	acceptedTicks   uint64
	scheduler       Scheduler
}

type runtimeIdentity struct{ _ byte }

// NewRuntime creates an isolated program-scoped animation runtime.
func NewRuntime() *Runtime { return NewRuntimeWithScheduler(wallScheduler{}) }

// NewRuntimeWithScheduler creates a runtime using the supplied production
// scheduling boundary. Tick creation, leases, and acceptance remain owned by
// Runtime; only time acquisition and delayed delivery are delegated.
func NewRuntimeWithScheduler(s Scheduler) *Runtime {
	if s == nil {
		panic("animation: nil Scheduler")
	}
	return &Runtime{runtimeIdentity: &runtimeIdentity{}, scheduler: s}
}

// NewSnapshotRuntime renders at a fixed elapsed time and is used by the lean TUI for its current frame.
func NewSnapshotRuntime(elapsed time.Duration) *Runtime {
	r := NewRuntime()
	r.elapsed = max(elapsed, 0)
	return r
}

// Register increments this animation runtime's active animation count.
func (r *Runtime) Register() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active++
}

// Unregister decrements this animation runtime's active animation count.
func (r *Runtime) Unregister() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active > 0 {
		r.active--
	}
	if r.active == 0 {
		r.abandonLeaseLocked()
	}
}

// HasActive reports whether this animation runtime has active animations.
func (r *Runtime) HasActive() bool { return r.ActiveCount() > 0 }

// ActiveCount returns this animation runtime's active animation count.
func (r *Runtime) ActiveCount() int32 {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Continue schedules the next tick when animations remain active. The root calls
// it exactly once after fanout of an accepted TickMsg.
func (r *Runtime) Continue() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == 0 {
		r.abandonLeaseLocked()
		r.lastDeliveredAt = time.Time{}
		return nil
	}
	return r.tickLocked()
}

// Accept consumes this animation runtime's current tick lease and advances its clock.
func (r *Runtime) Accept(msg TickMsg) (TickMsg, bool) {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if msg.runtimeIdentity != r.runtimeIdentity || msg.generation != r.generation || !r.tickScheduled {
		return msg, false
	}
	r.acceptedTicks++
	// Frame is the legacy int representation of the monotonically increasing tick count.
	msg.Frame = int(r.acceptedTicks) //nolint:gosec // Compatibility field is intentionally int.
	r.tickScheduled = false
	delta := msg.deliveredAt.Sub(r.lastDeliveredAt)
	if r.lastDeliveredAt.IsZero() {
		delta = msg.deliveredAt.Sub(msg.timerStartedAt)
	}
	if delta <= 0 {
		delta = TickRate
	}
	r.lastDeliveredAt = msg.deliveredAt
	msg.elapsedBeforeTick = r.elapsed
	r.elapsed += delta
	msg.elapsedAfterTick = r.elapsed
	msg.dirty = &atomic.Bool{}
	return msg, true
}

// Now returns elapsed time accepted by this animation runtime.
func (r *Runtime) Now() time.Duration {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.elapsed
}

// Pause freezes the animation clock and invalidates queued ticks without
// releasing subscriptions. New animations can register but cannot schedule work.
func (r *Runtime) Pause() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = true
	r.abandonLeaseLocked()
	r.lastDeliveredAt = time.Time{}
}

// Resume restarts at most one tick chain, excluding time spent paused.
func (r *Runtime) Resume() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		return nil
	}
	r.paused = false
	if r.active == 0 {
		return nil
	}
	return r.tickLocked()
}

// EnsureRunning replaces a potentially lost outstanding tick command.
func (r *Runtime) EnsureRunning() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == 0 {
		r.abandonLeaseLocked()
		r.lastDeliveredAt = time.Time{}
		return nil
	}
	r.generation++
	r.tickScheduled = false
	return r.tickLocked()
}

// Stop invalidates all queued ticks and releases every registration owned by
// this program. Program teardown calls it after component cleanup as a final
// ownership boundary; a late queued TickMsg can then never revive the chain.
func (r *Runtime) Stop() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = 0
	r.abandonLeaseLocked()
	r.lastDeliveredAt = time.Time{}
}

// Subscribe creates an inactive subscription owned by this animation runtime.
func (r *Runtime) Subscribe() Subscription { return NewSubscription(r) }

// Transition creates an idle transition owned by this animation runtime.
func (r *Runtime) Transition() Transition { return NewTransition(r) }

func (r *Runtime) start() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	wasEmpty := r.active == 0
	r.active++
	if wasEmpty {
		return r.tickLocked()
	}
	return nil
}

func (r *Runtime) mustExist() {
	if r == nil || r.runtimeIdentity == nil {
		panic("animation: nil or zero Runtime")
	}
}

func (r *Runtime) abandonLeaseLocked() {
	if r.tickScheduled {
		r.generation++
		r.tickScheduled = false
	}
}

func (r *Runtime) tickLocked() tea.Cmd {
	if r.paused || r.tickScheduled {
		return nil
	}
	r.tickScheduled = true
	runtimeIdentity, generation, timerStartedAt := r.runtimeIdentity, r.generation, r.scheduler.Now()
	return r.scheduler.Tick(TickRate, func(t time.Time) tea.Msg {
		return TickMsg{runtimeIdentity: runtimeIdentity, generation: generation, deliveredAt: t, timerStartedAt: timerStartedAt}
	})
}

// legacyContinue preserves the facade contract in which delivery itself
// consumes the outstanding lease; legacy callers do not call Runtime.Accept.
func (r *Runtime) legacyContinue() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == 0 {
		r.abandonLeaseLocked()
		return nil
	}
	return r.legacyTickLocked()
}

func (r *Runtime) legacyStart() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	wasEmpty := r.active == 0
	r.active++
	if wasEmpty {
		return r.legacyTickLocked()
	}
	return nil
}

func (r *Runtime) legacyTickLocked() tea.Cmd {
	if r.tickScheduled {
		return nil
	}
	r.tickScheduled = true
	runtimeIdentity, generation := r.runtimeIdentity, r.generation
	return r.scheduler.Tick(TickRate, func(time.Time) tea.Msg {
		r.mu.Lock()
		defer r.mu.Unlock()
		msg := TickMsg{runtimeIdentity: runtimeIdentity, generation: generation}
		if runtimeIdentity == r.runtimeIdentity && generation == r.generation && r.tickScheduled {
			r.acceptedTicks++
			// Frame is the legacy int representation of the monotonically increasing tick count.
			msg.Frame = int(r.acceptedTicks) //nolint:gosec // Compatibility field is intentionally int.
			r.tickScheduled = false
		}
		return msg
	})
}

func (r *Runtime) isCurrent(msg TickMsg) bool {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	return msg.runtimeIdentity == r.runtimeIdentity && msg.generation == r.generation
}

// legacyRuntime retains the pre-animation-runtime package API for incremental adoption.
// New programs should own an animation Runtime and pass it to components explicitly.
var legacyRuntime = NewRuntime()

// Coordinator is retained as a compatibility facade for callers that own an
// isolated coordinator. New code should use the animation Runtime.
type Coordinator struct{ ar *Runtime }

func (c *Coordinator) boundRuntime() *Runtime {
	if c.ar == nil {
		c.ar = NewRuntime()
	}
	return c.ar
}
func (c *Coordinator) Register()                 { c.boundRuntime().Register() }
func (c *Coordinator) Unregister()               { c.boundRuntime().Unregister() }
func (c *Coordinator) HasActive() bool           { return c.boundRuntime().HasActive() }
func (c *Coordinator) StartTick() tea.Cmd        { return c.boundRuntime().legacyContinue() }
func (c *Coordinator) StartTickIfFirst() tea.Cmd { return c.boundRuntime().legacyStart() }

// Register, Unregister, HasActive, StartTick, and StartTickIfFirst preserve the
// original package-level API while components migrate to program ownership.
func Register()                 { legacyRuntime.Register() }
func Unregister()               { legacyRuntime.Unregister() }
func HasActive() bool           { return legacyRuntime.HasActive() }
func StartTick() tea.Cmd        { return legacyRuntime.legacyContinue() }
func StartTickIfFirst() tea.Cmd { return legacyRuntime.legacyStart() }

// IsCurrentGen reports whether msg belongs to the package facade's current tick
// chain. It is a side-effect-free compatibility check; facade tick delivery,
// not this predicate, consumes the outstanding lease.
func IsCurrentGen(msg TickMsg) bool { return legacyRuntime.isCurrent(msg) }

// TickRate is the shared interval between animation ticks.
const TickRate = time.Second / 14
