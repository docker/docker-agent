package animation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/animation/animationtest"
)

func TestHoverFadeLifecycle(t *testing.T) {
	t.Parallel()
	clock, ar := animationtest.NewClock(time.Unix(1, 0))
	h := animation.NewHoverFade[string](ar, 100*time.Millisecond, animation.Linear)

	// Hover two targets simultaneously, then advance halfway.
	require.NotNil(t, h.Set("a", "b"))
	clock.Advance(50*time.Millisecond - animation.TickRate)
	tick, ok := ar.Accept(clock.Step(t).(animation.TickMsg))
	require.True(t, ok)
	require.True(t, h.Tick(tick))
	require.InDelta(t, 0.5, h.Fraction("a"), 0.01)
	require.InDelta(t, 0.5, h.Fraction("b"), 0.01)

	// Dropping "b" fades it back to 0 and prunes it once settled.
	h.Set("a")
	_ = ar.Continue()
	clock.Advance(100 * time.Millisecond)
	tick, ok = ar.Accept(clock.Step(t).(animation.TickMsg))
	require.True(t, ok)
	require.True(t, h.Tick(tick))
	require.InDelta(t, 1.0, h.Fraction("a"), 0.01)
	require.Zero(t, h.Fraction("b"))
	require.Equal(t, 1, h.Len())

	// Clear cancels remaining state and unregisters from runtime.
	require.True(t, h.Clear())
	require.Zero(t, h.Len())
	require.Zero(t, ar.ActiveCount())
}
