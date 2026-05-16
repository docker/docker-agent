package lifecycle

import (
	"fmt"
	"sync"
	"time"
)

// State is the high-level lifecycle state of a toolset, surfaced in logs,
// the TUI, and OTel attributes.
//
// State machine:
//
//	Stopped ──Start()──▶ Starting ──ok──▶ Ready
//	   ▲                    │ err          │ Wait()/Close()
//	   │                    ▼              ▼
//	   └─────── Stop() ── Failed ◀──── Restarting ──ok──▶ Ready
//	                       ▲              │
//	                       └── budget ────┘
//
// Degraded is a transient state used when a Ready toolset starts failing
// health checks but has not yet been demoted by the supervisor.
type State int32

const (
	// StateStopped is the initial state and the post-Stop state.
	StateStopped State = iota
	// StateStarting is set during the first connect/initialize handshake.
	StateStarting
	// StateReady means the toolset is connected, initialized, and serving.
	StateReady
	// StateDegraded means usable but the last health check or call failed.
	StateDegraded
	// StateRestarting means the supervisor is reconnecting after a failure.
	StateRestarting
	// StateFailed means the supervisor has given up restarting.
	StateFailed
)

var stateNames = [...]string{
	StateStopped:    "stopped",
	StateStarting:   "starting",
	StateReady:      "ready",
	StateDegraded:   "degraded",
	StateRestarting: "restarting",
	StateFailed:     "failed",
}

// String returns a short, lowercase human-readable name. Out-of-range or
// negative State values fall back to "state(N)" instead of panicking on
// the lookup array.
func (s State) String() string {
	if s >= 0 && int(s) < len(stateNames) && stateNames[s] != "" {
		return stateNames[s]
	}
	return fmt.Sprintf("state(%d)", s)
}

// IsTerminal reports whether s requires external action (Start/Restart/Stop)
// to leave.
func (s State) IsTerminal() bool { return s == StateStopped || s == StateFailed }

// IsUsable reports whether the toolset can serve requests in this state
// (Ready or Degraded).
func (s State) IsUsable() bool { return s == StateReady || s == StateDegraded }

// StateInfo is a copyable snapshot of a Tracker.
type StateInfo struct {
	State        State
	Since        time.Time
	LastError    error
	RestartCount int
}

// Tracker is a small thread-safe state holder shared by all transports.
// It records the current state, transition time, last error, and a
// restart counter. Transition validity is the supervisor's job.
//
// The zero value is valid and starts in StateStopped.
type Tracker struct {
	mu           sync.RWMutex
	state        State
	since        time.Time
	lastErr      error
	restartCount int
}

// NewTracker returns a Tracker initialised in StateStopped.
func NewTracker() *Tracker {
	return &Tracker{state: StateStopped, since: time.Now()}
}

// Set transitions to s, recording the transition time and clearing the
// last error. Same-state Set is a no-op (preserves Since/LastError).
func (t *Tracker) Set(s State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == s {
		return
	}
	t.state = s
	t.since = time.Now()
	t.lastErr = nil
}

// Fail transitions to s and records err as the last error. Use this when
// entering Failed/Restarting after a failure so the supervisor's snapshot
// surfaces a useful message.
func (t *Tracker) Fail(s State, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
	t.since = time.Now()
	t.lastErr = err
}

// IncRestarts increments the restart counter and returns the new value.
func (t *Tracker) IncRestarts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restartCount++
	return t.restartCount
}

// ResetRestarts zeroes the restart counter. Called after a sustained
// Ready period to forget transient incidents.
func (t *Tracker) ResetRestarts() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restartCount = 0
}

// Snapshot returns a point-in-time copy of the tracker.
func (t *Tracker) Snapshot() StateInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return StateInfo{
		State:        t.state,
		Since:        t.since,
		LastError:    t.lastErr,
		RestartCount: t.restartCount,
	}
}

// State returns the current state.
func (t *Tracker) State() State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// LastError returns the most recent error recorded by Fail, or nil if a
// clean Set has happened since.
func (t *Tracker) LastError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastErr
}
