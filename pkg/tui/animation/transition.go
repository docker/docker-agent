package animation

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Transition animates a value from 0 to 1 over an animation-clock duration
// using an easing function. It integrates with the owning animation Runtime: starting
// a transition registers an active animation, and finishing or cancelling it
// unregisters.
//
// Usage:
//
//	var t animation.Transition
//	cmd := t.Start(140*time.Millisecond, animation.EaseOutCubic)
//
//	// On each TickMsg:
//	if t.Running() {
//	    t.Tick()
//	    v := t.Value()           // 0.0 → 1.0, eased
//	    current := from + (to-from) * v
//	}

type Transition struct {
	ar        *Runtime
	running   bool
	startedAt time.Duration
	elapsed   time.Duration
	duration  time.Duration
	easingFn  EasingFunc
}

// NewTransition returns an idle transition owned by an animation runtime.
func NewTransition(ar *Runtime) Transition {
	if ar == nil {
		panic("animation: nil runtime")
	}
	return Transition{ar: ar}
}

// SetRuntime binds an idle transition to a program's animation runtime.
func (tr *Transition) SetRuntime(ar *Runtime) {
	if ar == nil {
		panic("animation: nil runtime")
	}
	if tr.running {
		panic("animation: cannot rebind running transition")
	}
	tr.ar = ar
}

func (tr *Transition) boundRuntime() *Runtime {
	if tr.ar == nil {
		panic("animation: unbound Transition")
	}
	return tr.ar
}

// EasingFunc maps a linear progress value in [0,1] to an eased value in [0,1].
type EasingFunc func(t float64) float64

// EaseOutCubic decelerates toward the end — fast start, slow finish.
func EaseOutCubic(t float64) float64 {
	t = 1 - t
	return 1 - t*t*t
}

// EaseOutQuint decelerates more aggressively than cubic.
func EaseOutQuint(t float64) float64 {
	t = 1 - t
	return 1 - t*t*t*t*t
}

// EaseInOutCubic accelerates then decelerates symmetrically.
func EaseInOutCubic(t float64) float64 {
	if t < 0.5 {
		return 4 * t * t * t
	}
	t = -2*t + 2
	return 1 - t*t*t/2
}

// Linear performs no easing — constant speed.
func Linear(t float64) float64 {
	return t
}

// Start begins the transition over the given duration using the provided easing
// function. If a transition is already running it is replaced without
// re-registering. Returns a command to start the tick chain when this is the
// first registration.
func (tr *Transition) Start(duration time.Duration, fn EasingFunc) tea.Cmd {
	if duration <= 0 {
		duration = time.Nanosecond
	}
	if fn == nil {
		fn = Linear
	}
	wasRunning := tr.running
	tr.running = true
	tr.startedAt = tr.boundRuntime().Now()
	tr.elapsed = 0
	tr.duration = duration
	tr.easingFn = fn
	if !wasRunning {
		return tr.boundRuntime().start()
	}
	return nil
}

// Tick samples the animation-runtime-owned clock. When the transition completes
// it stops automatically and unregisters from the coordinator.
func (tr *Transition) Tick() {
	if !tr.running {
		return
	}
	tr.elapsed = tr.currentElapsed()
	if tr.elapsed >= tr.duration {
		tr.elapsed = tr.duration
		tr.running = false
		tr.boundRuntime().Unregister()
	}
}

// Cancel stops the transition immediately and unregisters from the
// coordinator. Safe to call when not running.
func (tr *Transition) Cancel() {
	if !tr.running {
		return
	}
	tr.running = false
	tr.boundRuntime().Unregister()
}

// Running reports whether the transition is in progress.
func (tr *Transition) Running() bool { return tr.running }

// Elapsed returns the elapsed time within the transition.
func (tr *Transition) Elapsed() time.Duration {
	if tr.running {
		return tr.currentElapsed()
	}
	return tr.elapsed
}

// Duration returns the configured transition duration.
func (tr *Transition) Duration() time.Duration { return tr.duration }

// Progress returns the linear progress in [0, 1].
func (tr *Transition) Progress() float64 {
	return ProgressAt(tr.Elapsed(), tr.duration, nil)
}

// ProgressAt maps elapsed time to clamped progress in [0, 1], optionally
// applying easing. A nonpositive duration returns 0 (an unconfigured effect).
func ProgressAt(elapsed, duration time.Duration, ease EasingFunc) float64 {
	if duration <= 0 || elapsed <= 0 {
		return 0
	}
	if elapsed >= duration {
		return 1
	}
	progress := float64(elapsed) / float64(duration)
	if ease != nil {
		return ease(progress)
	}
	return progress
}

func (tr *Transition) currentElapsed() time.Duration {
	elapsed := tr.boundRuntime().Now() - tr.startedAt
	if elapsed < 0 {
		return 0
	}
	if tr.duration > 0 && elapsed > tr.duration {
		return tr.duration
	}
	return elapsed
}

// Value returns the eased progress in [0, 1]. Returns 0 before Start,
// and 1 after the transition completes.
func (tr *Transition) Value() float64 {
	return ProgressAt(tr.Elapsed(), tr.duration, tr.easingFn)
}

// Lerp returns the linearly-interpolated value between from and to at
// the current eased progress: from + (to-from) * Value().
func (tr *Transition) Lerp(from, to int) int {
	v := tr.Value()
	return from + round(float64(to-from)*v)
}

func round(f float64) int {
	if f >= 0 {
		return int(f + 0.5)
	}
	return -int(-f + 0.5)
}
