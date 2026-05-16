package dialog

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ThemeChoice represents a selectable theme option.
type ThemeChoice struct {
	Ref       string // Theme reference ("default" for built-in default)
	Name      string // Display name
	IsCurrent bool   // Currently active theme
	IsDefault bool   // Built-in default theme ("default")
	IsBuiltin bool   // Built-in theme shipped with docker agent
}

// themePickerDialog is a dialog for selecting a theme.
type themePickerDialog struct {
	pickerCore

	themes   []ThemeChoice
	filtered []ThemeChoice

	// originalThemeRef is the theme ref active when the dialog opened. It is
	// used to restore on cancel.
	originalThemeRef string
	// lastPreviewRef avoids re-applying the same preview repeatedly (e.g.,
	// during filtering).
	lastPreviewRef string
}

// customThemesSeparatorLabel labels the separator above the custom themes group.
const customThemesSeparatorLabel = "Custom themes"

// themePickerLayout is the layout used by the theme picker. It uses the
// shared sectioned-picker overhead so it can host the same group separators
// as the model picker.
var themePickerLayout = pickerLayout{
	WidthPercent:    pickerWidthPercent,
	MinWidth:        pickerMinWidth,
	MaxWidth:        pickerMaxWidth,
	HeightPercent:   pickerHeightPercent,
	MaxHeight:       pickerMaxHeight,
	ListOverhead:    pickerListVerticalOverhead,
	ListStartOffset: pickerListStartOffset,
}

// NewThemePickerDialog creates a new theme picker dialog.
// originalThemeRef is the currently active theme ref (for restoration on cancel).
func NewThemePickerDialog(themes []ThemeChoice, originalThemeRef string) Dialog {
	d := &themePickerDialog{
		pickerCore:       newPickerCore(themePickerLayout, "Type to search themes…"),
		originalThemeRef: originalThemeRef,
	}
	d.textInput.CharLimit = 100

	// Sort themes: built-in first, then custom. Within each section: current
	// first, then default, then alphabetically.
	sortedThemes := slices.Clone(themes)
	slices.SortFunc(sortedThemes, func(a, b ThemeChoice) int {
		return comparePickerSortKeys(themeSortKeys(a), themeSortKeys(b))
	})
	d.themes = sortedThemes
	d.filterThemes()

	// Find current theme and select it (if multiple are marked current, pick first).
	for i, t := range d.filtered {
		if t.IsCurrent {
			d.selected = i
			d.scrollview.EnsureLineVisible(d.findSelectedLine())
			break
		}
	}
	// The current theme is already applied; avoid emitting a duplicate preview
	// for it on the first navigation.
	if d.selected >= 0 && d.selected < len(d.filtered) {
		d.lastPreviewRef = d.filtered[d.selected].Ref
	}

	return d
}

// themeSortKeys derives the sort key tuple from a ThemeChoice.
func themeSortKeys(t ThemeChoice) pickerSortKeys {
	section := 1
	if t.IsBuiltin {
		section = 0
	}
	return pickerSortKeys{
		Section:   section,
		IsCurrent: t.IsCurrent,
		IsDefault: t.IsDefault,
		Name:      t.Name,
		Tiebreak:  t.Ref,
	}
}

func (d *themePickerDialog) Init() tea.Cmd { return textinput.Blink }

func (d *themePickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Scrollview handles mouse scrollbar, wheel, and pgup/pgdn/home/end.
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case messages.ThemeChangedMsg:
		// Refresh input styling when a preview swaps the active theme.
		d.textInput.SetStyles(styles.DialogInputStyle)
		return d, nil

	case tea.PasteMsg:
		cmd := d.handleInputChange(msg)
		return d, cmd

	case tea.MouseClickMsg:
		dbl, changed := d.handleListClick(msg, d.lineToThemeIndex)
		switch {
		case dbl:
			cmd := d.handleSelection()
			return d, cmd
		case changed:
			cmd := d.emitPreview()
			return d, cmd
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, tea.Sequence(
				closeDialogCmd(),
				core.CmdHandler(messages.ThemeCancelPreviewMsg{OriginalRef: d.originalThemeRef}),
			)
		case key.Matches(msg, d.keyMap.Up):
			cmd := d.navigateAndPreview(-1)
			return d, cmd
		case key.Matches(msg, d.keyMap.Down):
			cmd := d.navigateAndPreview(+1)
			return d, cmd
		case key.Matches(msg, d.keyMap.Enter):
			cmd := d.handleSelection()
			return d, cmd
		default:
			cmd := d.handleInputChange(msg)
			return d, cmd
		}
	}

	return d, nil
}

// navigateAndPreview moves the selection by delta and emits a preview when
// the selection actually moved.
func (d *themePickerDialog) navigateAndPreview(delta int) tea.Cmd {
	if d.navigate(delta, len(d.filtered), d.findSelectedLine) {
		return d.emitPreview()
	}
	return nil
}

// handleInputChange forwards msg to the text input, re-runs the filter, and
// emits a preview when filtering moved the selection.
func (d *themePickerDialog) handleInputChange(msg tea.Msg) tea.Cmd {
	cmd := d.updateInput(msg, nil)
	if d.filterThemes() {
		d.scrollview.EnsureLineVisible(d.findSelectedLine())
		return tea.Batch(cmd, d.emitPreview())
	}
	return cmd
}

func (d *themePickerDialog) handleSelection() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return nil
	}
	return tea.Sequence(
		closeDialogCmd(),
		core.CmdHandler(messages.ChangeThemeMsg{ThemeRef: d.filtered[d.selected].Ref}),
	)
}

// emitPreview requests a theme preview via an app-level message, skipping
// re-emission for the same theme.
func (d *themePickerDialog) emitPreview() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return nil
	}
	selected := d.filtered[d.selected]
	if selected.Ref == d.lastPreviewRef {
		return nil
	}
	d.lastPreviewRef = selected.Ref
	return core.CmdHandler(messages.ThemePreviewMsg{
		ThemeRef:    selected.Ref,
		OriginalRef: d.originalThemeRef,
	})
}

// buildList constructs the list of themes with a "Custom themes" separator
// before the first custom entry (when built-in themes precede it). Pass
// contentWidth=0 to compute the layout without rendering items (used by
// mouse hit-testing and findSelectedLine).
func (d *themePickerDialog) buildList(contentWidth int) *groupedList {
	gl := newGroupedList()
	hasBuiltin := slices.ContainsFunc(d.filtered, func(t ThemeChoice) bool { return t.IsBuiltin })

	customSepShown := false
	for i, theme := range d.filtered {
		if !theme.IsBuiltin && !customSepShown {
			if hasBuiltin {
				gl.AddNonItem(RenderGroupSeparator(customThemesSeparatorLabel, contentWidth))
			}
			customSepShown = true
		}
		gl.AddItem(d.renderTheme(theme, i == d.selected, contentWidth))
	}
	return gl
}

func (d *themePickerDialog) lineToThemeIndex(line int) int {
	return d.buildList(0).ItemForLine(line)
}

func (d *themePickerDialog) findSelectedLine() int {
	return d.buildList(0).LineForItem(d.selected)
}

func (d *themePickerDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)

	gl := d.buildList(contentWidth)
	d.updateScrollviewPosition()
	d.scrollview.SetContent(gl.Lines(), len(gl.Lines()))

	scrollableContent := d.scrollview.View()
	if len(d.filtered) == 0 {
		scrollableContent = d.renderEmptyState("No themes found", contentWidth)
	}

	content := NewContent(d.regionWidth(contentWidth)).
		AddTitle("Select Theme").
		AddSpace().
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(scrollableContent).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "select", "esc", "cancel").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

func (d *themePickerDialog) renderTheme(theme ThemeChoice, selected bool, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}

	nameStyle, descStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	defaultBadgeStyle := styles.BadgeDefaultStyle
	currentBadgeStyle := styles.BadgeCurrentStyle
	if selected {
		nameStyle, descStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
		defaultBadgeStyle = defaultBadgeStyle.Background(styles.MobyBlue)
		currentBadgeStyle = currentBadgeStyle.Background(styles.MobyBlue)
	}

	// For custom themes, show the filename as a description. Built-in themes
	// don't need one.
	var desc string
	if !theme.IsBuiltin {
		desc = strings.TrimPrefix(theme.Ref, styles.UserThemePrefix)
	}

	// Reserve space for badges and the description so the name truncation
	// keeps everything visible.
	var badgeWidth int
	if theme.IsCurrent {
		badgeWidth += lipgloss.Width(" (current)")
	}
	if theme.IsDefault {
		badgeWidth += lipgloss.Width(" (default)")
	}

	separatorWidth := 0
	if desc != "" {
		separatorWidth = lipgloss.Width(" • ")
	}

	maxNameWidth := maxWidth - badgeWidth
	if desc != "" {
		minDescWidth := min(10, lipgloss.Width(desc))
		maxNameWidth = maxWidth - badgeWidth - separatorWidth - minDescWidth
	}

	displayName := theme.Name
	if lipgloss.Width(displayName) > maxNameWidth {
		displayName = toolcommon.TruncateText(displayName, maxNameWidth)
	}

	// Build the name with coloured badges in order: name (current) (default).
	name := nameStyle.Render(displayName)
	if theme.IsCurrent {
		name += currentBadgeStyle.Render(" (current)")
	}
	if theme.IsDefault {
		name += defaultBadgeStyle.Render(" (default)")
	}

	if desc != "" {
		remainingWidth := maxWidth - lipgloss.Width(name) - separatorWidth
		if remainingWidth > 0 {
			return name + descStyle.Render(" • "+toolcommon.TruncateText(desc, remainingWidth))
		}
	}
	return name
}

// filterThemes re-applies the search filter. It preserves the current
// selection when possible. Returns true when the selected theme changed.
func (d *themePickerDialog) filterThemes() (selectionChanged bool) {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))

	prevRef := ""
	if d.selected >= 0 && d.selected < len(d.filtered) {
		prevRef = d.filtered[d.selected].Ref
	}

	d.filtered = d.filtered[:0]
	for _, theme := range d.themes {
		if query == "" || strings.Contains(strings.ToLower(theme.Name+" "+theme.Ref), query) {
			d.filtered = append(d.filtered, theme)
		}
	}

	d.selected = 0
	if prevRef != "" {
		for i, t := range d.filtered {
			if t.Ref == prevRef {
				d.selected = i
				break
			}
		}
	}
	d.scrollview.SetScrollOffset(0)

	newRef := ""
	if d.selected >= 0 && d.selected < len(d.filtered) {
		newRef = d.filtered[d.selected].Ref
	}
	return newRef != prevRef
}
