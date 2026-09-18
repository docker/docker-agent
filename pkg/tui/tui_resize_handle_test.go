package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestRenderResizeHandle_TinyWidths guards against a panic (negative
// strings.Repeat count) when the terminal reports degenerate sizes such as
// 0x0 or 1x1: the padded inner width goes negative for widths smaller than
// the app's horizontal padding.
func TestRenderResizeHandle_TinyWidths(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)

	for _, width := range []int{-1, 0, 1, appPaddingHorizontal, appPaddingHorizontal + 1, 80} {
		out := m.renderResizeHandle(width)
		if width <= appPaddingHorizontal {
			assert.Empty(t, out, "width %d", width)
		} else {
			assert.NotEmpty(t, out, "width %d", width)
		}
	}
}

// TestRenderResizeHandle_SuffixNeverOverflows pins that a status suffix wider
// than the handle line is truncated instead of overflowing the row on narrow
// terminals.
func TestRenderResizeHandle_SuffixNeverOverflows(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.activeTab.sessionState = &service.SessionState{}
	m.activeTab.sessionState.SetPauseState(service.PausePaused)

	for _, width := range []int{5, 10, 20, 80, 200} {
		out := m.renderResizeHandle(width)
		assert.LessOrEqual(t, lipgloss.Width(out), width, "width %d", width)
	}
}

func TestLineWithSuffix(t *testing.T) {
	t.Parallel()

	// The suffix fits: the line is truncated to make room.
	out := lineWithSuffix("──────────", " ok", 8)
	assert.Equal(t, "───── ok", out)
	assert.Equal(t, 8, lipgloss.Width(out))

	// The suffix alone is too wide: it is truncated and the line dropped.
	out = lineWithSuffix("──────────", " a very long status", 8)
	assert.Equal(t, 8, lipgloss.Width(out))
}
