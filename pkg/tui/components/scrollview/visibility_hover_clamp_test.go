package scrollview

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScrollbarHoverClampsToPane pins the scrollbar hover band's right edge:
// the pane's own columns and the divider column just outside it (the
// crossfade partner) count as hover, while the neighbouring pane's first
// column does not.
func TestScrollbarHoverClampsToPane(t *testing.T) {
	t.Parallel()

	m, _, _ := animationFixture(t)
	row := m.yPos

	m.SetScrollbarPointer(m.ScrollbarX(), row)
	require.True(t, m.pointerOverScrollbar(), "the scrollbar column itself hovers")

	m.SetScrollbarPointer(m.ScrollbarX()+1, row)
	require.True(t, m.pointerOverScrollbar(),
		"the divider column just past the pane crossfades with the scrollbar and must hover")

	m.SetScrollbarPointer(m.ScrollbarX()+2, row)
	require.False(t, m.pointerOverScrollbar(),
		"the neighbouring pane's first column must not summon this scrollbar")
}
