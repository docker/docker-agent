package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
)

// fakeClock is a deterministic clock for tests. Calls to now() return the
// stored time without advancing it; tests advance it explicitly via Advance.
type fakeClock struct {
	t time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestWithClock_AppliedToRuntime(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := newFakeClock(fixed)

	rt, err := NewLocalRuntime(tm,
		WithClock(clock.Now),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	assert.Equal(t, fixed, rt.now(), "WithClock not wired into runtime")
}

func TestWithClock_NilLeavesDefault(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithClock(nil),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	// Default clock should be time.Now; ensure it returns a recent value.
	got := rt.now()
	assert.WithinDuration(t, time.Now(), got, time.Second)
}

func TestFallbackCooldown_UsesInjectedClock(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	rt, err := NewLocalRuntime(tm,
		WithClock(clock.Now),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	// Activate cooldown for 1 minute against a fallback at index 0.
	rt.fallback.cooldowns.Set("root", 0, time.Minute)
	require.NotNil(t, rt.fallback.cooldowns.Get("root"), "cooldown should be active")

	// Advance just under the window: still active.
	clock.Advance(59 * time.Second)
	assert.NotNil(t, rt.fallback.cooldowns.Get("root"), "cooldown expired prematurely")

	// Advance past expiry: cooldowns.Get evicts and returns nil.
	clock.Advance(2 * time.Second)
	assert.Nil(t, rt.fallback.cooldowns.Get("root"), "expired cooldown not evicted")
}
