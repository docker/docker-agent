package leantui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/tui/animation"
)

func TestSpinnerSamplesActiveElapsedTime(t *testing.T) {
	t.Parallel()
	for _, gaps := range [][]time.Duration{{30 * time.Millisecond, 195 * time.Millisecond}, {225 * time.Millisecond}} {
		m := &model{busy: true, screen: &ui.Screen{Transcript: ui.NewTranscript()}}
		now := time.Unix(1, 0)
		m.sampleSpinner(now)
		for _, gap := range gaps {
			now = now.Add(gap)
			m.sampleSpinner(now)
		}
		assert.Equal(t, 2, animation.Chat.FrameIndexAt(m.spinnerElapsed))
		m.busy = false
		m.sampleSpinner(now)
		now = now.Add(time.Hour)
		m.busy = true
		m.sampleSpinner(now)
		assert.Equal(t, 225*time.Millisecond, m.spinnerElapsed)
		m.sampleSpinner(now.Add(80 * time.Millisecond))
		assert.Equal(t, 3, animation.Chat.FrameIndexAt(m.spinnerElapsed))
	}
}
