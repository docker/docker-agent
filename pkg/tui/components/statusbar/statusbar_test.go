package statusbar

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestNewTabHitboxMatchesRenderedAction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		width int
		label string
	}{{80, "+ new tab"}, {20, "+ new tab"}, {10, "+"}, {3, "+"}, {2, "+"}, {1, "+"}, {0, ""}} {
		t.Run(strconv.Itoa(tc.width), func(t *testing.T) {
			bar := New(nil, WithTitle("docker agent 界"))
			bar.SetWidth(tc.width)
			bar.SetShowNewTab(true)
			row := ansi.Strip(bar.View())
			require.Equal(t, tc.width, ansi.StringWidth(row))
			if tc.label != "" {
				require.Contains(t, row, tc.label)
			}
			before, _, found := strings.Cut(row, tc.label)
			start := ansi.StringWidth(before)
			for x := -1; x <= tc.width; x++ {
				require.Equal(t, found && x >= start && x < start+len(tc.label), bar.ClickedNewTab(x), "cell %d of %q", x, row)
			}
			bar.SetShowNewTab(false)
			require.False(t, bar.ClickedNewTab(start), "disabling takes effect before repaint")
			hidden := ansi.Strip(bar.View())
			require.LessOrEqual(t, ansi.StringWidth(hidden), tc.width)
			require.NotContains(t, hidden, "+")
			bar.SetShowNewTab(true)
			require.Equal(t, row, ansi.Strip(bar.View()))
		})
	}
}

func TestNewTabHitboxAfterResizeBeforeRepaint(t *testing.T) {
	t.Parallel()
	bar := New(nil)
	bar.SetWidth(80)
	bar.SetShowNewTab(true)
	before, _, ok := strings.Cut(ansi.Strip(bar.View()), "+")
	require.True(t, ok)
	oldX := ansi.StringWidth(before)
	bar.SetWidth(100)
	require.False(t, bar.ClickedNewTab(oldX))
	require.True(t, bar.ClickedNewTab(oldX+20))
}

func TestPauseNoticeDoesNotLeaveHiddenNewTabHitbox(t *testing.T) {
	t.Parallel()
	bar := New(nil)
	bar.SetWidth(20)
	bar.SetShowNewTab(true)
	before, _, ok := strings.Cut(ansi.Strip(bar.View()), "+")
	require.True(t, ok)
	plus := ansi.StringWidth(before)
	require.True(t, bar.ClickedNewTab(plus))
	bar.SetStatus("Pausing… (/pause to cancel)")
	row := ansi.Strip(bar.View())
	require.Contains(t, row, "Pausing…")
	require.NotContains(t, row, "+")
	require.False(t, bar.ClickedNewTab(plus))
}
