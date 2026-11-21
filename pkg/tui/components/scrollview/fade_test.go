package scrollview

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestScrollFadeEdges(t *testing.T) {
	t.Parallel()
	const height, width = 20, 12
	for _, tt := range []struct {
		name                     string
		content, slack, offset   int
		fadeTop, fadeBottom, bar bool
	}{
		{"top", 50, 0, 0, false, true, true},
		{"middle", 50, 0, 10, true, true, true},
		{"bottom", 50, 0, 30, true, false, true},
		{"fits", height, 0, 0, false, false, false},
		{"only slack below", height, 10, 0, false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New()
			m.SetSize(width, height)
			m.SetContentWithTrailingSlack(nil, tt.content, tt.slack)
			line := "\x1b[4;38;2;240;50;10m界 un\x1b[m"
			line += strings.Repeat(" ", m.ContentWidth()-ansi.StringWidth(line))
			content := slices.Repeat([]string{line}, tt.content)
			m.SetContentWithTrailingSlack(content, tt.content, tt.slack)
			m.SetScrollOffset(tt.offset)
			visible := content[tt.offset : tt.offset+height]
			view := m.View()
			assert.Equal(t, view, m.ViewWithLines(visible))
			assert.Equal(t, view, m.ViewWithRestyledLines(visible))
			assert.Equal(t, view, m.ViewWithPaddedLines(visible))
			assert.Equal(t, slices.Repeat([]string{line}, tt.content), content, "render must not modify input")
			assert.Equal(t, view, m.View(), "idle frames stay byte-identical")
			assert.Equal(t, tt.offset, m.ScrollOffset())
			rows := strings.Split(view, "\n")
			require.Len(t, rows, height)
			bar := m.sb.ViewLines()
			palette := styles.CurrentFadePalette()
			for i, row := range rows {
				want := line
				switch {
				case tt.fadeTop && i < 3:
					want = palette.FadeLine(line, 0.5+0.125*float64(i+1))
				case tt.fadeBottom && i >= height-3:
					want = palette.FadeLine(line, 0.5+0.125*float64(height-i))
				}
				if tt.bar {
					want += " " + bar[i]
				}
				assert.Equal(t, want, row)
				assert.Equal(t, width, ansi.StringWidth(row))
			}
		})
	}
}

func TestScrollFadeTracksThemeAndRestyling(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New()
	m.SetSize(10, 4)
	content := []string{"a", "b", "c", "d", "e", "f"}
	m.SetContent(content, len(content))
	m.SetScrollOffset(1)
	before := m.View()
	oldPalette := m.fade.palette
	theme := *original
	theme.Colors.Background = "#ffffff"
	theme.Colors.TextPrimary = "#000000"
	styles.ApplyTheme(&theme)
	after := m.View()
	assert.NotEqual(t, oldPalette, m.fade.palette)
	assert.NotEqual(t, before, after)
	assert.Equal(t, ansi.Strip(before), ansi.Strip(after))

	visible := slices.Clone(content[1:5])
	visible[0] = "\x1b[4;48;2;90;60;30mb\x1b[m"
	out := m.ViewWithRestyledLines(visible)
	assert.Contains(t, out, "\x1b[4;48;2;131;108;86m")
	assert.Equal(t, ansi.Strip(after), ansi.Strip(out))
	assert.Equal(t, after, m.View())
}

func TestScrollFadeDisabled(t *testing.T) { //nolint:paralleltest // This test changes the app-wide render preference.
	t.Cleanup(func() { SetEdgeFadeEnabled(true) })
	const height = 4
	content := []string{"a", "\x1b[4;38;2;240;50;10mb\x1b[m", "c", "d", "e", "f"}
	newView := func() *Model {
		m := New()
		m.SetSize(10, height)
		m.SetContent(content, len(content))
		m.SetScrollOffset(1)
		return m
	}
	m := newView()
	faded := m.View()
	require.NotNil(t, m.fade)
	SetEdgeFadeEnabled(false)
	plain := m.View()
	assert.Equal(t, ansi.Strip(faded), ansi.Strip(plain))
	assert.NotEqual(t, faded, plain)
	bar := m.sb.ViewLines()
	for i, row := range strings.Split(plain, "\n") {
		line := content[i+1]
		want := line + strings.Repeat(" ", m.ContentWidth()-ansi.StringWidth(line)) + " " + bar[i]
		assert.Equal(t, want, row)
	}
	assert.Equal(t, 1, m.ScrollOffset())
	newModel := newView()
	assert.Equal(t, plain, newModel.View())
	assert.Nil(t, newModel.fade, "disabled scrollviews do not allocate a fade cache")
	SetEdgeFadeEnabled(true)
	assert.Equal(t, faded, m.View())
	assert.Equal(t, faded, newModel.View())
}
