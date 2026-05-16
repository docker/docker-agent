package runtime

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCooldownManager_GetMissingReturnsNil(t *testing.T) {
	t.Parallel()

	m := newCooldownManager(time.Now)
	assert.Nil(t, m.Get("nobody"))
}

func TestCooldownManager_SetAndGet(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newCooldownManager(clock.Now)

	m.Set("agent-A", 2, time.Minute)

	state := m.Get("agent-A")
	require.NotNil(t, state)
	assert.Equal(t, 2, state.fallbackIndex)
	assert.True(t, state.until.After(clock.Now()), "until must be in the future")
}

func TestCooldownManager_GetEvictsExpired(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newCooldownManager(clock.Now)

	m.Set("agent-A", 0, time.Minute)
	require.NotNil(t, m.Get("agent-A"), "cooldown should be active immediately after Set")

	clock.Advance(59 * time.Second)
	assert.NotNil(t, m.Get("agent-A"), "cooldown should still be active just under the window")

	clock.Advance(2 * time.Second)
	assert.Nil(t, m.Get("agent-A"), "expired cooldown should be evicted")

	// Eviction is observable: a second Get also returns nil.
	assert.Nil(t, m.Get("agent-A"), "expired cooldown should stay evicted")
}

func TestCooldownManager_SetReplacesExistingEntry(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newCooldownManager(clock.Now)

	m.Set("agent-A", 0, time.Minute)
	m.Set("agent-A", 3, 5*time.Minute)

	state := m.Get("agent-A")
	require.NotNil(t, state)
	assert.Equal(t, 3, state.fallbackIndex, "second Set must replace fallback index")

	expectedUntil := clock.Now().Add(5 * time.Minute)
	assert.Equal(t, expectedUntil, state.until, "second Set must reset the until window")
}

func TestCooldownManager_Clear(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newCooldownManager(clock.Now)

	m.Set("agent-A", 0, time.Minute)
	m.Clear("agent-A")
	assert.Nil(t, m.Get("agent-A"))

	// Clearing a non-existent entry is a no-op (no panic).
	assert.NotPanics(t, func() { m.Clear("never-set") })
}

func TestCooldownManager_NilClockDefaultsToTimeNow(t *testing.T) {
	t.Parallel()

	m := newCooldownManager(nil)
	require.NotNil(t, m.now, "nil clock should be replaced by time.Now")

	got := m.now()
	assert.WithinDuration(t, time.Now(), got, time.Second)
}

func TestCooldownManager_PerAgentIsolation(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	m := newCooldownManager(clock.Now)

	m.Set("agent-A", 0, time.Minute)
	m.Set("agent-B", 1, 2*time.Minute)

	stateA := m.Get("agent-A")
	stateB := m.Get("agent-B")
	require.NotNil(t, stateA)
	require.NotNil(t, stateB)
	assert.Equal(t, 0, stateA.fallbackIndex)
	assert.Equal(t, 1, stateB.fallbackIndex)

	// Expiring A leaves B intact.
	clock.Advance(90 * time.Second)
	assert.Nil(t, m.Get("agent-A"), "A should have expired")
	assert.NotNil(t, m.Get("agent-B"), "B must not be affected by A's expiry")
}

// TestCooldownManager_ConcurrentSafety drives many concurrent Set/Get/Clear
// to confirm the mutex contract under -race.
func TestCooldownManager_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	m := newCooldownManager(time.Now)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(3)
		go func(id int) {
			defer wg.Done()
			for j := range 100 {
				m.Set("agent", j%5, time.Hour)
				_ = id
			}
		}(i)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = m.Get("agent")
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				m.Clear("agent")
			}
		}()
	}
	wg.Wait()
}
