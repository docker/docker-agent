package editor

import (
	"image"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestTextOccupancyUsesVisibleWrappedGlyphs(t *testing.T) {
	t.Parallel()
	e := New(nil).(*editor)
	e.SetSize(13, 2)
	origin := image.Pt(7, 10)
	cells := func() []image.Rectangle { return e.AppendOccupiedTextCells(nil, origin) }
	require.Empty(t, cells(), "placeholder is not input")
	e.SetValue("   ")
	require.Empty(t, cells(), "the cursor over a blank cell is not input")

	// Scrolling to the end leaves only the final two rows visible. The
	// penultimate row wraps, and each CJK glyph occupies two columns.
	e.SetValue("earlier\n" + strings.Repeat("界", 12) + "\n字  語")
	frame := styles.EditorStyle
	x := origin.X + frame.GetMarginLeft() + frame.GetPaddingLeft()
	y := origin.Y + frame.GetPaddingTop()
	want := []image.Rectangle{
		image.Rect(x, y, x+12, y+1),
		image.Rect(x, y+1, x+2, y+2),
		image.Rect(x+4, y+1, x+6, y+2),
	}
	got := cells()
	require.Equal(t, want, got)
	got[0] = image.Rectangle{}
	require.Equal(t, want, cells(), "callers cannot overwrite cached viewport cells")
	e.Blur()
	require.Empty(t, cells())
}

func TestTextOccupancyUsesFocusedHistorySearchViewport(t *testing.T) {
	t.Parallel()
	e := New(nil).(*editor)
	e.banner.SetItems([]bannerItem{{label: "attached.txt"}})
	e.EnterHistorySearch()
	e.SetSize(20, 5)
	origin := image.Pt(7, 10)
	cells := func() []image.Rectangle { return e.AppendOccupiedTextCells(nil, origin) }
	require.Empty(t, cells(), "search placeholder is not input")

	e.searchInput.SetValue(strings.Repeat("a", 20) + "界  語")
	e.textarea.SetValue("unfocused history preview")
	for _, size := range []image.Point{{X: 20, Y: 5}, {X: 10, Y: 3}} {
		e.SetSize(size.X, size.Y)
		e.searchInput.CursorEnd()
		got := cells()
		require.Len(t, got, 2, "only the visible non-space query runs are occupied")
		frame := styles.EditorStyle
		x := origin.X + frame.GetMarginLeft() + frame.GetBorderLeftSize() + frame.GetPaddingLeft()
		y := origin.Y + frame.GetMarginTop() + frame.GetBorderTopSize() + frame.GetPaddingTop() + size.Y - 1
		visibleQuery := strings.Split(stripANSI(e.View()), "\n")[y-origin.Y]
		visibleQuery = strings.TrimSpace(visibleQuery)
		require.NotContains(t, visibleQuery, strings.Repeat("a", 20), "left of query scrolled out of view")
		before, _, ok := strings.Cut(visibleQuery, "  ")
		require.True(t, ok)
		end := x + lipgloss.Width(before)
		require.Equal(t, []image.Rectangle{image.Rect(x, y, end, y+1), image.Rect(end+2, y, end+4, y+1)}, got)
	}
	e.searchInput.Blur()
	require.Empty(t, cells(), "blurred query is not input")
}
