package scrollview

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
)

// composeReference reproduces the previous compose implementation (measure,
// pad, lipgloss.JoinHorizontal) as an oracle for the optimized version.
func composeReference(m *Model, lines []string) string {
	contentWidth := m.ContentWidth()
	for i, line := range lines {
		w := ansi.StringWidth(line)
		switch {
		case w > contentWidth:
			lines[i] = ansi.Truncate(line, contentWidth, "")
		case w < contentWidth:
			lines[i] = line + strings.Repeat(" ", contentWidth-w)
		}
	}
	contentView := strings.Join(lines, "\n")
	if m.NeedsScrollbar() {
		col := strings.Repeat(" ", m.gapWidth)
		gapLines := make([]string, m.height)
		for i := range gapLines {
			gapLines[i] = col
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, contentView, strings.Join(gapLines, "\n"), m.sb.View())
	}
	if m.reserveScrollbarSpace {
		col := strings.Repeat(" ", m.gapWidth+scrollbar.Width)
		blankLines := make([]string, len(lines))
		for i := range blankLines {
			blankLines[i] = col
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, contentView, strings.Join(blankLines, "\n"))
	}
	return contentView
}

func TestComposeMatchesJoinHorizontal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reserveSpace bool
		totalHeight  int // > height forces a scrollbar
	}{
		{name: "with scrollbar", totalHeight: 100},
		{name: "with scrollbar and reserved space", reserveSpace: true, totalHeight: 100},
		{name: "no scrollbar reserved space", reserveSpace: true, totalHeight: 5},
		{name: "no scrollbar no reserve", totalHeight: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New(WithReserveScrollbarSpace(tt.reserveSpace))
			m.SetSize(40, 10)

			content := make([]string, tt.totalHeight)
			for i := range content {
				content[i] = strings.Repeat("x", (i%7)+1)
			}
			m.SetContent(content, tt.totalHeight)
			m.SetScrollOffset(3)
			m.syncScrollbar()

			nLines := min(m.height, len(content)-m.scrollOffset)
			lines := append([]string(nil), content[m.scrollOffset:m.scrollOffset+nLines]...)
			if m.NeedsScrollbar() {
				for len(lines) < m.height {
					lines = append(lines, "")
				}
			}

			want := composeReference(m, append([]string(nil), lines...))
			got := m.compose(append([]string(nil), lines...), m.scrollOffset)
			assert.Equal(t, want, got)
		})
	}
}

func TestViewIsStableAcrossFrames(t *testing.T) {
	t.Parallel()

	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(30, 6)

	content := make([]string, 50)
	for i := range content {
		content[i] = strings.Repeat("line", (i%3)+1)
	}
	m.SetContent(content, len(content))
	m.SetScrollOffset(10)

	first := m.View()
	require.NotEmpty(t, first)
	// Repeated frames with unchanged state must render identically (widths
	// are memoized after the first frame).
	assert.Equal(t, first, m.View())

	// Scrolling changes the window but keeps the memoized widths valid.
	m.SetScrollOffset(11)
	shifted := m.View()
	assert.NotEqual(t, first, shifted)

	m.SetScrollOffset(10)
	assert.Equal(t, first, m.View())
}

func TestSetContentInvalidatesWidthCache(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetSize(20, 4)

	m.SetContent([]string{"aa", "bb", "cc", "dd", "ee"}, 5)
	before := m.View()

	// New slice with wider lines: cached widths must not leak through.
	m.SetContent([]string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd", "eeeeeeee"}, 5)
	after := m.View()

	assert.NotEqual(t, before, after)
	assert.Contains(t, after, "aaaaaaaa")
}

func TestComposeRemeasuresRestyledLines(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetSize(20, 4)
	content := []string{"aa", "bb", "cc", "dd", "ee", "ff"}
	m.SetContent(content, len(content))
	m.SetScrollOffset(0)
	m.View() // warm the width cache

	// A restyled line whose display width differs from the cached original
	// (e.g. width drift on complex grapheme clusters) must be re-measured,
	// not padded using the stale cached width.
	restyled := []string{"aaaa", "bb", "cc", "dd"}
	out := m.ViewWithRestyledLines(restyled)
	for line := range strings.SplitSeq(out, "\n") {
		// Full row = content + gap + scrollbar column.
		assert.Equal(t, m.width, ansi.StringWidth(line))
	}
}

func TestViewWithPaddedLinesReservesScrollbarSpaceForShortContent(t *testing.T) {
	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(20, 5)
	m.SetContent([]string{"short"}, 1)
	line := "short" + strings.Repeat(" ", m.ContentWidth()-len("short"))

	got := m.ViewWithPaddedLines([]string{line})
	lines := strings.Split(got, "\n")
	require.NotEmpty(t, lines)
	require.Equal(t, 20, ansi.StringWidth(lines[0]))
	require.True(t, strings.HasSuffix(lines[0], strings.Repeat(" ", m.ReservedCols())))
}
