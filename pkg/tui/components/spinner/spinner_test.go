package spinner

import (
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func TestSpinnerCopyDoesNotLeakAnimationSubscription(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntime()
	s1 := New(ar, ModeSpinnerOnly, lipgloss.NewStyle())
	cmd := s1.Init()
	require.NotNil(t, cmd)
	require.True(t, ar.HasActive())

	// Copy the spinner value and stop via the copy; should still stop the shared subscription.
	s2 := s1
	s2.Stop()
	require.False(t, ar.HasActive())
}

func BenchmarkSpinner_ModeSpinnerOnly(b *testing.B) {
	s := New(animation.NewRuntime(), ModeSpinnerOnly, lipgloss.NewStyle())
	for b.Loop() {
		_ = s.View()
	}
}

func BenchmarkSpinner_ModeBoth(b *testing.B) {
	s := New(animation.NewRuntime(), ModeBoth, lipgloss.NewStyle())
	for b.Loop() {
		_ = s.View()
	}
}

func TestLightSweepSamplesElapsedPositionAndDwell(t *testing.T) {
	t.Parallel()
	s := New(animation.NewRuntime(), ModeBoth, lipgloss.NewStyle()).(*spinner)
	s.SetMessage("test")
	const right = 7
	const outbound = 10 * lightStepDuration
	const reverse = outbound + lightPauseDuration
	const cycle = reverse + 12*lightStepDuration
	for _, tc := range []struct {
		elapsed time.Duration
		want    int
	}{
		{0, -3},
		{lightStepDuration + 17*time.Millisecond, -2},
		{outbound - time.Nanosecond, right - 1},
		{outbound, right},
		{reverse - time.Nanosecond, right},
		{reverse + lightStepDuration, right - 1},
		{cycle - time.Nanosecond, -4},
		{cycle, -3},
		{1_000_000*cycle + reverse + 2*lightStepDuration, right - 2},
	} {
		require.Equal(t, tc.want, s.lightPositionAt(tc.elapsed), "elapsed %v", tc.elapsed)
	}
}
