package root

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTags(t *testing.T) {
	t.Parallel()

	// No tags renders nothing.
	assert.Empty(t, renderTags(nil, 40))
	assert.Empty(t, renderTags([]string{"go"}, 0))

	// Tags render as "#tag" chips joined by spaces (ANSI stripped).
	assert.Equal(t, "#go #cli", ansi.Strip(renderTags([]string{"go", "cli"}, 40)))

	// Blank/whitespace tags are skipped.
	assert.Equal(t, "#go", ansi.Strip(renderTags([]string{" ", "go"}, 40)))

	// Chips that don't fit the width are dropped instead of overflowing.
	assert.Equal(t, "#go", ansi.Strip(renderTags([]string{"go", "verylongtag"}, 4)))
}

func TestParseAgentPickerRefs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single ref", "coder", []string{"coder"}},
		{"multiple refs", "default,coder", []string{"default", "coder"}},
		{"trims whitespace", " default , coder ", []string{"default", "coder"}},
		{"drops empty entries", "default,,coder,", []string{"default", "coder"}},
		{"external refs", "default,myorg/agent:tag", []string{"default", "myorg/agent:tag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseAgentPickerRefs(tt.raw))
		})
	}
}

func TestParseAgentPickerRefsDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	// Without ~/.agents, only the built-in agents are offered.
	for _, raw := range []string{"", "   ", ",,,"} {
		assert.Equal(t, []string{"default", "minimal"}, parseAgentPickerRefs(raw))
	}

	// Config files in ~/.agents are appended to the built-ins; directories
	// (e.g. skills) and non-config files are ignored.
	agentsDir := filepath.Join(home, ".agents")
	require.NoError(t, os.MkdirAll(filepath.Join(agentsDir, "skills"), 0o755))
	for _, name := range []string{"assistant.yaml", "gopher.yml", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(agentsDir, name), nil, 0o644))
	}

	want := []string{
		"default",
		"minimal",
		filepath.Join(agentsDir, "assistant.yaml"),
		filepath.Join(agentsDir, "gopher.yml"),
	}
	assert.Equal(t, want, parseAgentPickerRefs(""))

	// The NoOptDefVal sentinel behaves like an empty spec.
	assert.Equal(t, want, parseAgentPickerRefs(agentPickerDefaultsSpec))

	// An explicit list bypasses the defaults entirely.
	assert.Equal(t, []string{"coder"}, parseAgentPickerRefs("coder"))
}

func TestLoadBuiltinAgentChoices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	choices := loadAgentChoices(t.Context(), defaultAgentPickerRefs(), nil, nil)
	require.Len(t, choices, 2)
	for i, name := range []string{"default", "minimal"} {
		assert.Equal(t, name, choices[i].ref)
		require.NoError(t, choices[i].err)
		assert.NotEmpty(t, choices[i].description)
		assert.NotEmpty(t, choices[i].yaml)
	}
}

func TestPrependAgentRef(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"coder"}, prependAgentRef("coder", nil))
	assert.Equal(t, []string{"coder", "hello"}, prependAgentRef("coder", []string{"hello"}))
	assert.Equal(t, []string{"coder", "a", "b"}, prependAgentRef("coder", []string{"a", "b"}))
}

func TestIsLocalConfigRef(t *testing.T) {
	t.Parallel()

	assert.True(t, isLocalConfigRef("/home/user/.agents/assistant.yaml"))
	assert.True(t, isLocalConfigRef("./agent.yml"))
	assert.True(t, isLocalConfigRef("agent.hcl"))
	assert.False(t, isLocalConfigRef("default"))
	assert.False(t, isLocalConfigRef("myorg/agent:tag"))
	assert.False(t, isLocalConfigRef("https://example.com/agent.yaml"))
}

func TestAgentPickerCardShowsDescriptionForLocalConfigs(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel(nil)

	// Local config files show the description as the title, with the path
	// demoted to the detail line.
	card := ansi.Strip(m.renderCard(agentChoice{ref: filepath.FromSlash("/tmp/agents/gopher.yaml"), description: "Golang expert"}, 70, false))
	assert.Less(t, strings.Index(card, "Golang expert"), strings.Index(card, filepath.FromSlash("/tmp/agents/gopher.yaml")))

	// Without a description the path remains the title; same for descriptions
	// that sanitize to nothing.
	for _, desc := range []string{"", "  \n\t ", "\x1b\x07"} {
		card = ansi.Strip(m.renderCard(agentChoice{ref: filepath.FromSlash("/tmp/agents/gopher.yaml"), description: desc}, 70, false))
		assert.Contains(t, card, filepath.FromSlash("/tmp/agents/gopher.yaml"))
	}

	// Non-path refs keep the ref as title with the description below.
	card = ansi.Strip(m.renderCard(agentChoice{ref: "default", description: "A helpful assistant"}, 70, false))
	assert.Less(t, strings.Index(card, "default"), strings.Index(card, "A helpful assistant"))
}

func TestAgentPickerCardShortensHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	ref := filepath.Join(home, ".agents", "gopher.yaml")
	card := ansi.Strip(newAgentPickerModel(nil).renderCard(agentChoice{ref: ref, description: "Golang expert"}, 70, false))
	assert.Contains(t, card, filepath.Join("~", ".agents", "gopher.yaml"))
	assert.NotContains(t, card, home)
}

func TestTruncateDetail(t *testing.T) {
	t.Parallel()

	// Collapses newlines and runs of whitespace into single spaces.
	assert.Equal(t, "a b c", truncateDetail("a\nb\t  c", 80))
	// Truncates to width with an ellipsis.
	assert.Equal(t, "hel…", truncateDetail("hello world", 4))
	// Empty / whitespace-only input collapses to empty.
	assert.Empty(t, truncateDetail("   \n\t ", 80))
}

func TestAgentPickerRenderNoPanic(t *testing.T) {
	t.Parallel()

	choices := []agentChoice{
		{ref: "default", description: "A helpful AI assistant", tags: []string{"general", "assistant"}, yaml: "agents:\n  root:\n    model: auto\n"},
		{ref: "myorg/some-really-long-agent-reference-name", description: strings.Repeat("very long description ", 20)},
		{ref: "broken", err: errors.New("multi\nline\nerror that is also quite long and should be truncated cleanly")},
	}
	m := newAgentPickerModel(choices)

	// Render across a range of widths, including degenerate ones, to make
	// sure width math never produces a panic or a negative truncation width.
	for _, w := range []int{0, 1, 10, 30, 80, 200} {
		m.width = w
		m.height = 24
		assert.NotPanics(t, func() { _ = m.render() })
		m.openDetails()
		assert.NotPanics(t, func() { _ = m.renderDetails() })
		m.showDetails = false
	}
}

func TestAgentPickerDetailsToggle(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{
		{ref: "default", yaml: "agents:\n  root:\n    model: auto\n"},
	})
	m.width = 80
	m.height = 24

	assert.False(t, m.showDetails)
	m.openDetails()
	assert.True(t, m.showDetails)
	assert.Contains(t, ansi.Strip(m.details.GetContent()), "model: auto")
}

func TestDetailsContent(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel(nil)
	// YAML is syntax-highlighted, so compare with ANSI stripped.
	assert.Equal(t, "a: b", ansi.Strip(m.detailsContent(agentChoice{yaml: "a: b\n\n"})))
	assert.Contains(t, m.detailsContent(agentChoice{err: errors.New("boom")}), "boom")
	assert.Equal(t, "No configuration available.", m.detailsContent(agentChoice{}))
}

func TestHighlightYAML(t *testing.T) {
	t.Parallel()

	src := "agents:\n  root:\n    model: auto"
	out := highlightYAML(src)
	// Colorized output differs from the input but preserves the text
	// (ignoring any insignificant trailing whitespace per line).
	assert.NotEqual(t, src, out)
	assert.Equal(t, src, trimTrailingPerLine(ansi.Strip(out)))
}

func trimTrailingPerLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

func TestAgentPickerDetailsFixedSize(t *testing.T) {
	t.Parallel()

	// A long YAML so the viewport is scrollable.
	var sb strings.Builder
	for i := range 200 {
		sb.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	m := newAgentPickerModel([]agentChoice{{ref: "default", yaml: sb.String()}})
	m.width = 120
	m.height = 40
	m.openDetails()

	top := m.renderDetails()
	topW, topH := lipgloss.Size(top)

	// Scroll down a few lines and to the bottom; dimensions must not change.
	for range 5 {
		m.details.ScrollDown(1)
		m.syncDetailsBar()
		w, h := lipgloss.Size(m.renderDetails())
		assert.Equal(t, topW, w, "width changed while scrolling")
		assert.Equal(t, topH, h, "height changed while scrolling")
	}

	m.details.GotoBottom()
	m.syncDetailsBar()
	w, h := lipgloss.Size(m.renderDetails())
	assert.Equal(t, topW, w, "width changed at bottom")
	assert.Equal(t, topH, h, "height changed at bottom")
}

func TestAgentPickerDetailsHelpNeverWraps(t *testing.T) {
	t.Parallel()

	// On very narrow terminals the details help must drop bindings instead of
	// soft-wrapping, which would add a row and overflow the dialog height.
	m := newAgentPickerModel([]agentChoice{{ref: "default", yaml: strings.Repeat("a: b\n", 50)}})
	for _, w := range []int{20, 24, 28, 32, 40, 120} {
		m.width = w
		m.height = 40
		m.openDetails()
		_, dh := m.detailsDialogSize()
		assert.Equal(t, dh, lipgloss.Height(m.renderDetails()), "dialog height mismatch at width %d", w)
		m.showDetails = false
	}
}

func TestStripControl(t *testing.T) {
	t.Parallel()

	// The ESC byte is removed, neutralizing the escape sequence (the
	// remaining "[31m" is harmless literal text). Other control chars go too;
	// newlines are preserved.
	assert.Equal(t, "[31mredtext[0m", stripControl("\x1b[31mredtext\x1b[0m"))
	assert.NotContains(t, stripControl("\x1b[31mredtext\x1b[0m"), "\x1b")
	assert.Equal(t, "ab", stripControl("a\x07b"))
	assert.Equal(t, "line1\nline2", stripControl("line1\nline2"))
	assert.Equal(t, "ab", stripControl("a\x7fb"))
}

func TestSanitizeYAML(t *testing.T) {
	t.Parallel()

	// CRLF/CR normalized to LF, tabs expanded, ESC/control chars stripped.
	assert.Equal(t, "a\nb", sanitizeYAML("a\r\nb"))
	assert.Equal(t, "a\nb", sanitizeYAML("a\rb"))
	assert.Equal(t, "    x", sanitizeYAML("\tx"))
	assert.NotContains(t, sanitizeYAML("key: \x1b[31mvalue\x1b[0m"), "\x1b")
}

func TestHighlightYAMLStripsInjectedEscapes(t *testing.T) {
	t.Parallel()

	// A malicious config can't smuggle its own escape sequences through.
	out := highlightYAML("key: \x1b[31mvalue\x1b[0m\x07")
	plain := ansi.Strip(out)
	assert.NotContains(t, plain, "\x1b")
	assert.NotContains(t, plain, "\x07")
	assert.Contains(t, plain, "value")
}

func TestAgentPickerWindowing(t *testing.T) {
	t.Parallel()

	choices := make([]agentChoice, 10)
	for i := range choices {
		choices[i] = agentChoice{ref: "agent-" + strconv.Itoa(i)}
	}
	m := newAgentPickerModel(choices)
	m.width = 120
	m.height = 22 // fits (22-12)/5 = 2 cards

	assert.Equal(t, 2, m.visibleCount())

	// The rendered panel never exceeds the terminal height, and panelSize
	// still matches the actual render for hit-testing.
	gotW, gotH := m.panelSize()
	wantW, wantH := lipgloss.Size(m.render())
	assert.LessOrEqual(t, wantH, m.height)
	assert.Equal(t, wantW, gotW)
	assert.Equal(t, wantH, gotH)

	// Moving down past the window scrolls it, keeping the cursor visible.
	// The panel geometry must not shift while scrolling, or mouse
	// hit-testing would mis-track the cards.
	w0, h0 := m.panelSize()
	for range 5 {
		m.moveDown()
		w, h := m.panelSize()
		assert.Equal(t, w0, w, "panel width changed while scrolling")
		assert.Equal(t, h0, h, "panel height changed while scrolling")
	}
	assert.Equal(t, 5, m.cursor)
	assert.Equal(t, 4, m.offset)
	out := ansi.Strip(m.render())
	assert.Contains(t, out, "agent-4")
	assert.Contains(t, out, "agent-5")
	assert.NotContains(t, out, "agent-0")

	// Hit-testing maps points to absolute indices within the window.
	x, y := renderedCardRefPoint(t, m, 4)
	i, ok := m.cardAt(x, y)
	assert.True(t, ok)
	assert.Equal(t, 4, i)

	// Moving back up scrolls the window up again.
	for range 5 {
		m.moveUp()
	}
	assert.Equal(t, 0, m.cursor)
	assert.Equal(t, 0, m.offset)

	// Growing the terminal reveals every card and clamps the offset.
	m.cursor, m.offset = 9, 8
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 200})
	assert.Equal(t, 10, m.visibleCount())
	assert.Equal(t, 0, m.offset)
}

func TestAgentPickerDetailsTitleStripsEscapes(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "evil\x1b[31m\nagent.yaml", yaml: "a: b\n"}})
	m.width = 120
	m.height = 40
	m.openDetails()

	out := m.renderDetails()
	assert.NotContains(t, ansi.Strip(out), "\x1b")
	assert.Contains(t, ansi.Strip(out), "evil[31m agent.yaml")
}

func TestAgentPickerModelNavigation(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{
		{ref: "default"},
		{ref: "coder"},
	})

	// Up at the top is a no-op.
	m.moveUp()
	assert.Equal(t, 0, m.cursor)

	m.moveDown()
	assert.Equal(t, 1, m.cursor)

	// Down at the bottom is a no-op.
	m.moveDown()
	assert.Equal(t, 1, m.cursor)

	m.moveUp()
	assert.Equal(t, 0, m.cursor)
}

func TestAgentPickerCardAt(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{
		{ref: "default", description: "first"},
		{ref: "coder", description: "second"},
	})
	m.width = 120
	m.height = 40

	// A point far outside the panel hits nothing.
	_, ok := m.cardAt(0, 0)
	assert.False(t, ok)

	// Find the coordinates of each card by scanning the whole grid and
	// checking the reported index is stable and in range.
	seen := map[int]bool{}
	for y := range m.height {
		for x := range m.width {
			if i, ok := m.cardAt(x, y); ok {
				assert.GreaterOrEqual(t, i, 0)
				assert.Less(t, i, len(m.choices))
				seen[i] = true
			}
		}
	}
	// Both cards must be reachable.
	assert.True(t, seen[0])
	assert.True(t, seen[1])
}

func TestAgentPickerMouseHoverSelects(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{
		{ref: "default"},
		{ref: "coder"},
	})
	m.width = 120
	m.height = 40

	x, y := renderedCardRefPoint(t, m, 1)
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
	assert.Equal(t, 1, m.cursor, "hover should move the cursor to the hovered card")
}

func TestAgentPickerDoubleClickSelects(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{
		{ref: "default"},
		{ref: "coder"},
	})
	m.width = 120
	m.height = 40

	x, y := renderedCardRefPoint(t, m, 1)
	click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}

	// First click selects (moves cursor) but does not quit.
	_, cmd := m.Update(click)
	assert.Equal(t, 1, m.cursor)
	assert.Nil(t, cmd, "single click must not quit")

	// Second click on the same card within the threshold quits (selects).
	_, cmd = m.Update(click)
	assert.NotNil(t, cmd, "double click must quit")
	assert.IsType(t, tea.QuitMsg{}, cmd())
}

func TestAgentPickerDoubleClickResetsAfterTimeout(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	x, y := renderedCardRefPoint(t, m, 0)
	click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}

	_, cmd := m.Update(click)
	assert.Nil(t, cmd)

	// Simulate the threshold elapsing: a stale first click can't complete a
	// double-click, so the next click is treated as a fresh first click.
	m.lastClickTime = time.Now().Add(-2 * time.Second)
	_, cmd = m.Update(click)
	assert.Nil(t, cmd, "click after the threshold must not quit")
}

func TestAgentPickerClickOutsideDoesNothing(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	_, cmd := m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	assert.Nil(t, cmd)
	assert.Equal(t, -1, m.lastClickIndex, "a miss resets double-click tracking")
}

func TestAgentPickerPanelSizeMatchesRender(t *testing.T) {
	t.Parallel()

	// panelSize must agree with the actual rendered panel; otherwise cardAt's
	// hit zones drift away from what the user sees.
	cases := [][]agentChoice{
		{{ref: "default"}, {ref: "coder"}},
		{{ref: "a", description: "short"}},
		{
			{ref: "default", description: strings.Repeat("long description ", 10)},
			{ref: "myorg/some-really-long-agent-reference-name"},
			{ref: "broken", err: errors.New("boom")},
		},
	}
	for _, choices := range cases {
		m := newAgentPickerModel(choices)
		m.width = 120
		m.height = 40
		gotW, gotH := m.panelSize()
		wantW, wantH := lipgloss.Size(m.render())
		assert.Equal(t, wantW, gotW, "panel width mismatch")
		assert.Equal(t, wantH, gotH, "panel height mismatch")
	}
}

func TestAgentPickerCardAtMatchesRenderedText(t *testing.T) {
	t.Parallel()

	// Independently relate hit-testing to the rendered output: the row where a
	// card's ref text appears (as centered on screen) must hit that card, and
	// the title/help rows must miss.
	m := newAgentPickerModel([]agentChoice{
		{ref: "alpha-agent"},
		{ref: "beta-agent"},
	})
	m.width = 120
	m.height = 40

	screen := ansi.Strip(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.render()))
	lines := strings.Split(screen, "\n")

	findRow := func(substr string) int {
		for y, line := range lines {
			if strings.Contains(line, substr) {
				return y
			}
		}
		t.Fatalf("%q not found on screen", substr)
		return -1
	}

	for idx, ref := range []string{"alpha-agent", "beta-agent"} {
		y := findRow(ref)
		x := strings.Index(lines[y], ref)
		i, ok := m.cardAt(x, y)
		assert.True(t, ok, "ref row for %q should hit a card", ref)
		assert.Equal(t, idx, i, "ref row for %q should hit card %d", ref, idx)
	}

	// The title, subtitle, and status-bar rows must not resolve to any card.
	titleY := findRow("Choose an agent to run")
	_, ok := m.cardAt(m.width/2, titleY)
	assert.False(t, ok, "title row must not hit a card")

	subtitleY := findRow("double-click a card")
	_, ok = m.cardAt(m.width/2, subtitleY)
	assert.False(t, ok, "subtitle row must not hit a card")

	helpY := findRow("view yaml")
	_, ok = m.cardAt(m.width/2, helpY)
	assert.False(t, ok, "help row must not hit a card")
}

func TestAgentPickerDetailsResetsClickTracking(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{
		{ref: "default", yaml: "a: b\n"},
		{ref: "coder", yaml: "c: d\n"},
	})
	m.width = 120
	m.height = 40

	x, y := renderedCardRefPoint(t, m, 0)
	click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}

	// First click primes double-click tracking.
	_, cmd := m.Update(click)
	assert.Nil(t, cmd)

	// Opening then closing the details dialog must clear that state, so the
	// next click can't be paired with the pre-dialog one into a double-click.
	m.openDetails()
	assert.Equal(t, -1, m.lastClickIndex)

	_, _ = m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	assert.False(t, m.showDetails)
	assert.Equal(t, -1, m.lastClickIndex)

	_, cmd = m.Update(click)
	assert.Nil(t, cmd, "click after closing details must not complete a double-click")
}

func TestAgentPickerWheelIgnoredWithoutDetails(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Nil(t, cmd, "wheel does nothing while the card list is shown")
	assert.Equal(t, 0, m.cursor)
}

func TestAgentPickerLeanCheckboxDefaultsUnticked(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	assert.False(t, m.leanMode, "lean mode must be off by default")
	assert.Contains(t, ansi.Strip(m.render()), "[ ] Lean Mode", "checkbox renders unticked by default")
}

func TestAgentPickerLeanCheckboxSeeded(t *testing.T) {
	t.Parallel()

	// When the run would already be lean (--lean or user config), the
	// checkbox must reflect it instead of lying about the run mode.
	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.leanMode = true
	m.width = 120
	m.height = 40

	assert.Contains(t, ansi.Strip(m.render()), "[x] Lean Mode")
}

func TestAgentPickerLeanCheckboxKeyToggle(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	assert.Nil(t, cmd)
	assert.True(t, m.leanMode)
	assert.Contains(t, ansi.Strip(m.render()), "[x] Lean Mode")

	_, _ = m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	assert.False(t, m.leanMode)
}

func TestAgentPickerLeanCheckboxClickToggle(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	// Locate the checkbox on the rendered screen and click it. Note:
	// strings.Index returns a byte offset; convert the prefix to display
	// columns because border runes (│) are multi-byte.
	screen := ansi.Strip(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.render()))
	lines := strings.Split(screen, "\n")
	var x, y int
	found := false
	for row, line := range lines {
		if prefix, _, ok := strings.Cut(line, "[ ] Lean Mode"); ok {
			x, y, found = lipgloss.Width(prefix), row, true
			break
		}
	}
	assert.True(t, found, "checkbox not found on screen")
	assert.True(t, m.leanCheckboxAt(x, y), "hit zone must match the rendered checkbox")

	// Hit-zone boundaries: both edges are inside; one cell beyond each edge
	// and the adjacent rows are outside.
	checkboxWidth := len("[ ] Lean Mode")
	assert.True(t, m.leanCheckboxAt(x+checkboxWidth-1, y), "right edge must be inside")
	assert.False(t, m.leanCheckboxAt(x-1, y), "left of the checkbox must miss")
	assert.False(t, m.leanCheckboxAt(x+checkboxWidth, y), "right of the checkbox must miss")
	assert.False(t, m.leanCheckboxAt(x, y-1), "row above must miss")
	assert.False(t, m.leanCheckboxAt(x, y+1), "row below must miss")

	click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
	_, cmd := m.Update(click)
	assert.Nil(t, cmd, "clicking the checkbox must not quit")
	assert.True(t, m.leanMode)

	// A second click within the double-click threshold toggles back rather
	// than being treated as a card double-click.
	_, cmd = m.Update(click)
	assert.Nil(t, cmd)
	assert.False(t, m.leanMode)

	// The checkbox row must not hit any card.
	_, ok := m.cardAt(x, y)
	assert.False(t, ok, "checkbox row must not resolve to a card")
}

func TestAgentPickerBoardKey(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	assert.Contains(t, ansi.Strip(m.render()), "[ Open Board ]", "board button must be rendered")

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	require.NotNil(t, cmd, "pressing b must quit the picker")
	assert.True(t, m.startBoard)
}

func TestAgentPickerBoardButtonClick(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
	m.width = 120
	m.height = 40

	// Locate the button on the rendered screen and click it; convert the
	// byte-offset prefix to display columns (border runes are multi-byte).
	screen := ansi.Strip(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.render()))
	var x, y int
	found := false
	for row, line := range strings.Split(screen, "\n") {
		if prefix, _, ok := strings.Cut(line, "[ Open Board ]"); ok {
			x, y, found = lipgloss.Width(prefix), row, true
			break
		}
	}
	require.True(t, found, "board button not found on screen")
	assert.True(t, m.boardButtonAt(x, y), "hit zone must match the rendered button")

	// Hit-zone boundaries: both edges inside; one cell beyond each edge and
	// the adjacent rows outside. The cell left of the button belongs to the
	// gap after the lean checkbox, so it must hit neither.
	buttonWidth := len("[ Open Board ]")
	assert.True(t, m.boardButtonAt(x+buttonWidth-1, y), "right edge must be inside")
	assert.False(t, m.boardButtonAt(x-1, y), "left of the button must miss")
	assert.False(t, m.leanCheckboxAt(x-1, y), "gap must not hit the checkbox either")
	assert.False(t, m.boardButtonAt(x+buttonWidth, y), "right of the button must miss")
	assert.False(t, m.boardButtonAt(x, y-1), "row above must miss")
	assert.False(t, m.boardButtonAt(x, y+1), "row below must miss")

	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "clicking the button must quit the picker")
	assert.True(t, m.startBoard)
	assert.False(t, m.leanMode, "board click must not toggle lean mode")
}

func TestAgentPickerBoardKeyIgnoredInDetails(t *testing.T) {
	t.Parallel()

	m := newAgentPickerModel([]agentChoice{{ref: "default", yaml: "a: b\n"}, {ref: "coder"}})
	m.width = 120
	m.height = 40
	m.openDetails()

	// While the YAML dialog is open, b belongs to the viewport (page up) and
	// must not start the board.
	_, _ = m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	assert.False(t, m.startBoard, "b in the details dialog must not start the board")
	assert.True(t, m.showDetails, "the dialog must stay open")
}

func TestAgentPickerBoardButtonWindowed(t *testing.T) {
	t.Parallel()

	// A windowed list with a scrolled offset exercises the cardRows-based row
	// math the button's hit zone shares with the checkbox.
	choices := make([]agentChoice, 10)
	for i := range choices {
		choices[i] = agentChoice{ref: "agent-" + strconv.Itoa(i)}
	}
	m := newAgentPickerModel(choices)
	m.width = 120
	m.height = 22 // fits 2 cards
	for range 5 {
		m.moveDown()
	}
	require.Positive(t, m.offset, "list must be scrolled")

	screen := ansi.Strip(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.render()))
	var x, y int
	found := false
	for row, line := range strings.Split(screen, "\n") {
		if prefix, _, ok := strings.Cut(line, "[ Open Board ]"); ok {
			x, y, found = lipgloss.Width(prefix), row, true
			break
		}
	}
	require.True(t, found, "board button not found on screen")
	assert.True(t, m.boardButtonAt(x, y), "hit zone must match the rendered button while windowed")
}

func TestAgentPickerPanelSizeMatchesRenderNarrow(t *testing.T) {
	t.Parallel()

	// On narrow terminals the header lines are truncated so nothing wraps;
	// panelSize must still match the render exactly or hit-testing drifts.
	for _, width := range []int{40, 60, 80, 100} {
		m := newAgentPickerModel([]agentChoice{{ref: "default"}, {ref: "coder"}})
		m.width = width
		m.height = 40
		gotW, gotH := m.panelSize()
		wantW, wantH := lipgloss.Size(m.render())
		assert.Equal(t, wantW, gotW, "panel width mismatch at width %d", width)
		assert.Equal(t, wantH, gotH, "panel height mismatch at width %d", width)
		assert.LessOrEqual(t, wantW, width, "panel must fit the terminal at width %d", width)
	}
}

// renderedCardRefPoint locates fixture text on the rendered screen, independently of hit testing.
func renderedCardRefPoint(t *testing.T, m *agentPickerModel, want int) (int, int) {
	t.Helper()
	require.GreaterOrEqual(t, want, 0)
	require.Less(t, want, len(m.choices))
	ref := displayRef(m.choices[want].ref)
	require.NotEmpty(t, ref)
	screen := ansi.Strip(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.render()))
	for y, line := range strings.Split(screen, "\n") {
		offset := strings.Index(line, ref)
		if offset < 0 {
			continue
		}
		x := lipgloss.Width(line[:offset])
		index, ok := m.cardAt(x, y)
		require.True(t, ok, "rendered card %d must be clickable", want)
		require.Equal(t, want, index)
		return x, y
	}
	t.Fatalf("card %d (%q) is not visible", want, ref)
	return 0, 0
}
