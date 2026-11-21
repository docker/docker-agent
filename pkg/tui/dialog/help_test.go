package dialog

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestHelpSectionSpacing(t *testing.T) {
	t.Parallel()

	up := key.NewBinding(key.WithKeys("up"), key.WithHelp("", "Move up"))
	down := key.NewBinding(key.WithKeys("down"), key.WithHelp("", "Move down"))
	d := NewHelpDialog([]HelpSection{
		{Title: "Picker", Note: "Type to filter.", Bindings: []key.Binding{up, down}},
		{Title: "Dialog", Note: "No shortcuts here."},
		{Title: "Navigation", Bindings: []key.Binding{up}},
	}).(*helpDialog)
	var rows []string
	for _, line := range d.renderContent(40, 25) {
		rows = append(rows, strings.Join(strings.Fields(ansi.Strip(line)), " "))
	}
	require.Equal(t, []string{
		"Help", "", "> Search...", strings.Repeat("─", 40),
		"Picker", "", "Type to filter.", "", "up Move up", "down Move down",
		"", "", "Dialog", "", "No shortcuts here.",
		"", "", "Navigation", "", "up Move up",
	}, rows)
}

func TestHelpMatchesVisibleText(t *testing.T) {
	t.Parallel()

	binding := func(desc string, keys ...string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp("invisible", desc))
	}
	weak := binding("Annex the draft", "a")
	strong := binding("Next tab", "ctrl+n", "alt+right")
	exactDesc := binding("alt+right", "b")
	previous := binding("Previous tab", "ctrl+p")
	disabled := binding("Next tab", "f24")
	disabled.SetEnabled(false)
	d := NewHelpDialog([]HelpSection{
		{Title: "Composer", Bindings: []key.Binding{weak, exactDesc, disabled}},
		{Title: "Tabs", Note: "After leaving history search.", Bindings: []key.Binding{strong, previous}},
		{Title: "Open dialog", Note: "Use the underlying controls."},
	}).(*helpDialog)

	for _, tc := range []struct {
		query    string
		sections []int
		bindings []key.Binding
	}{
		{"nex", []int{1, 0}, []key.Binding{strong, weak}},
		{"alt+right", []int{1, 0}, []key.Binding{strong, exactDesc}},
		{"ALT+R", []int{0, 1}, []key.Binding{exactDesc, strong}},
		{"ctrln", []int{1}, []key.Binding{strong}},
		{"tabs next", []int{1}, []key.Binding{strong}},
		{"history ctrl+p", []int{1}, []key.Binding{previous}},
		{"tabs history", []int{1}, []key.Binding{strong, previous}},
		{"underlying", []int{2}, nil},
		{"invisible", nil, nil},
		{"ntb", nil, nil},
		{"tabs backups", nil, nil},
		{"f24", nil, nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			var sections []int
			var bindings []key.Binding
			for _, match := range d.matchShortcuts(strings.Fields(tc.query)) {
				sections = append(sections, match.section)
				for _, row := range match.rows {
					bindings = append(bindings, row.binding)
				}
			}
			require.Equal(t, tc.sections, sections)
			require.Equal(t, tc.bindings, bindings)
		})
	}
}

func TestHelpHighlightsMatchSources(t *testing.T) {
	t.Parallel()

	d := NewHelpDialog([]HelpSection{{
		Title: "Tabs", Note: "After closing the dialog.",
		Bindings: []key.Binding{
			key.NewBinding(key.WithKeys("ctrl+n", "alt+right"), key.WithHelp("", "Next tab")),
			key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("", "Previous tab")),
		},
	}}).(*helpDialog)
	view := func(query string) string {
		d.input.SetValue(query)
		return strings.Join(d.renderContent(90, 15), "\n")
	}
	highlight := func(style lipgloss.Style, text string) string {
		return style.Foreground(styles.Highlight).Underline(true).Render(text)
	}
	heading := styles.BoldStyle.Foreground(styles.Accent).Italic(true)
	catalog := view("")
	require.Contains(t, catalog, heading.Render("Tabs"))
	require.Contains(t, catalog, styles.HighlightWhiteStyle.Render("ctrl+n, alt+right"))
	mixed := view("tabs dialog alt+right next")
	require.Contains(t, mixed, highlight(heading, "Tabs"))
	require.Contains(t, mixed, highlight(styles.DialogHelpStyle, "dialog"))
	require.Contains(t, mixed, highlight(styles.HighlightWhiteStyle, "alt+right"))
	require.Contains(t, mixed, highlight(styles.SecondaryStyle, "Next"))
	require.NotContains(t, ansi.Strip(mixed), "Previous tab")
	wholeSection := view("closing")
	require.Contains(t, wholeSection, highlight(styles.DialogHelpStyle, "closing"))
	require.Contains(t, ansi.Strip(wholeSection), "Previous tab")

	// Scrolling to the second half of a wrapped match still shows its highlight.
	lines := renderHelpText("Go to CAFÉ", 3, styles.DialogContentStyle, []helpSpan{{6, 11}})
	require.Equal(t, []string{highlight(styles.DialogContentStyle, "CAF"), highlight(styles.DialogContentStyle, "É")}, lines[len(lines)-2:])
}

func TestHelpFilterLeavesResultsVisibleInShortTerminals(t *testing.T) {
	t.Parallel()

	next := key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("", "Next tab"))
	bindings := make([]key.Binding, 30)
	for i := range bindings {
		bindings[i] = next
	}
	d := NewHelpDialog([]HelpSection{{Title: "Tabs", Note: "After closing the dialog.", Bindings: bindings}}).(*helpDialog)
	_, _ = d.Update(tea.PasteMsg{Content: "tabs"})
	for _, size := range []tea.WindowSizeMsg{{Width: 42, Height: 15}, {Width: 80, Height: 16}, {Width: 120, Height: 24}} {
		_, _ = d.Update(size)
		frame := d.View()
		plain := ansi.Strip(frame)
		require.GreaterOrEqual(t, lipgloss.Width(frame), size.Width*9/10)
		require.LessOrEqual(t, lipgloss.Width(frame), size.Width)
		require.Equal(t, size.Height*95/100, lipgloss.Height(frame))
		require.Contains(t, plain, "> tabs")
		require.Contains(t, plain, "After closing the dialog.")
		require.Contains(t, plain, "Next tab")
		require.Contains(t, plain, "↑↓ scroll  Esc close")
	}
}

func TestHelpFilterKeepsScrollingAndRestoresCatalog(t *testing.T) {
	t.Parallel()

	d := NewHelpDialog([]HelpSection{
		{Title: "Composer", Bindings: []key.Binding{
			key.NewBinding(key.WithKeys("j"), key.WithHelp("", "Jump to match")),
			key.NewBinding(key.WithKeys("k"), key.WithHelp("", "Keep changes")),
			key.NewBinding(key.WithKeys("q"), key.WithHelp("", "Queue a message")),
		}},
		{Title: "Tabs", Note: "Use these to switch sessions.", Bindings: []key.Binding{
			key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("", "Next tab")),
		}},
	}).(*helpDialog)
	_, _ = d.Update(tea.WindowSizeMsg{Width: 120, Height: 18})
	view := func() string { return ansi.Strip(d.View()) }
	press := func(k tea.KeyPressMsg) { _, _ = d.Update(k) }
	catalog := view()
	require.Contains(t, catalog, "Search...")
	press(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.Positive(t, d.scrollview.ScrollOffset())
	require.Contains(t, view(), "Next tab")
	for _, tc := range []struct {
		letter rune
		want   string
	}{{'j', "Jump to match"}, {'k', "Keep changes"}, {'q', "Queue a message"}} {
		press(tea.KeyPressMsg{Code: tc.letter, Text: string(tc.letter)})
		require.Zero(t, d.scrollview.ScrollOffset())
		require.Contains(t, view(), tc.want)
		require.NotContains(t, view(), "Next tab")
		press(tea.KeyPressMsg{Code: tea.KeyBackspace})
		require.Equal(t, catalog, view())
	}
	_, _ = d.Update(tea.PasteMsg{Content: "ZZZ"})
	require.Contains(t, view(), "No matching shortcuts.")
	press(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Equal(t, "ZZ", d.input.Value())
	press(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	require.Equal(t, catalog, view())
}
