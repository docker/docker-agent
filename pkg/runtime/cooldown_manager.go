package runtime

import (
	"log/slog"
	"sync"
	"time"
)

// fallbackCooldownState tracks when we should stick with a fallback model
// instead of retrying the primary after a non-retryable error (e.g., 429).
type fallbackCooldownState struct {
	// fallbackIndex is the index in the fallback chain to start from
	// (0 = first fallback, -1 = primary).
	fallbackIndex int
	// until is when the cooldown expires and we should retry the primary.
	until time.Time
}

// cooldownManager owns the per-agent fallback-cooldown map. The runtime
// activates a cooldown when the primary model fails with a non-retryable
// error and a fallback succeeds; subsequent calls for that agent skip the
// primary until the window expires.
//
// Pulled out of fallback.go so the eviction-on-read invariant, the slog
// emissions, and the mutex contract live in one small, race-tested place
// instead of being threaded through three methods on *LocalRuntime.
//
// All methods are safe for concurrent use.
type cooldownManager struct {
	mu  sync.RWMutex
	now func() time.Time
	by  map[string]*fallbackCooldownState
}

// newCooldownManager constructs a cooldownManager that reads time via now.
// Tests pass a fake clock so they can verify the eviction window without
// real wall-clock advancement.
func newCooldownManager(now func() time.Time) *cooldownManager {
	if now == nil {
		now = time.Now
	}
	return &cooldownManager{
		now: now,
		by:  make(map[string]*fallbackCooldownState),
	}
}

// Get returns the active cooldown state for agentName, or nil when no
// cooldown is active. Expired entries are evicted as a side-effect of the
// read so a long-running runtime serving many agents stays bounded.
func (c *cooldownManager) Get(agentName string) *fallbackCooldownState {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.by[agentName]
	if state == nil {
		return nil
	}
	if c.now().After(state.until) {
		delete(c.by, agentName)
		return nil
	}
	return state
}

// Set activates a cooldown for agentName, pinning the run loop to the
// fallback at fallbackIndex for duration. Replaces any existing entry.
func (c *cooldownManager) Set(agentName string, fallbackIndex int, duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := &fallbackCooldownState{
		fallbackIndex: fallbackIndex,
		until:         c.now().Add(duration),
	}
	c.by[agentName] = state

	slog.Info("Fallback cooldown activated",
		"agent", agentName,
		"fallback_index", fallbackIndex,
		"cooldown", duration,
		"until", state.until.Format(time.RFC3339))
}

// Clear evicts any cooldown entry for agentName. No-op if none is active.
func (c *cooldownManager) Clear(agentName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.by[agentName]; exists {
		delete(c.by, agentName)
		slog.Debug("Fallback cooldown cleared", "agent", agentName)
	}
}
