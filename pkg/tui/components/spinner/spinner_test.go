package spinner

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func TestSpinnerCopyDoesNotLeakAnimationSubscription(t *testing.T) {
	s1 := New(ModeSpinnerOnly, lipgloss.NewStyle())
	cmd := s1.Init()
	require.NotNil(t, cmd)
	require.True(t, animation.HasActive())

	// Copy the spinner value and stop via the copy; should still stop the shared subscription.
	s2 := s1
	s2.Stop()
	require.False(t, animation.HasActive())
}

func TestFrameWrapsAround(t *testing.T) {
	n := len(spinnerFrames)
	require.Equal(t, spinnerFrames[0], Frame(0))
	require.Equal(t, spinnerFrames[1], Frame(1))
	require.Equal(t, spinnerFrames[0], Frame(n), "Frame should wrap around")
}

func BenchmarkSpinner_ModeSpinnerOnly(b *testing.B) {
	s := New(ModeSpinnerOnly, lipgloss.NewStyle())
	for b.Loop() {
		_ = s.View()
	}
}

func BenchmarkSpinner_ModeBoth(b *testing.B) {
	s := New(ModeBoth, lipgloss.NewStyle())
	for b.Loop() {
		_ = s.View()
	}
}
