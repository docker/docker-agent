package dialog

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	settingsWidthPercent = 60
	settingsMinWidth     = 52
	settingsMaxWidth     = 72
	previewMaxWidth      = 44
	previewMinWidth      = 24
)

const (
	tabAppearance = iota
	tabBehavior
	tabNotifications
	tabCount
)

var settingsTabLabels = [tabCount]string{"Appearance", "Behavior", "Notifications"}

const (
	rowTheme = iota
	rowPosition
	rowSpacing
	rowInfoMode
	rowSessionPath
	rowUsage
	rowAgents
	rowActiveAgents
	rowTools
	rowTodos
	rowSplitDiff
	rowExpandThinking
	rowHideToolResults
	rowRenderImages
	rowShowBanner
	appearanceRowCount
)

const (
	rowSendMode = iota
	rowYOLO
	rowRestoreTabs
	rowSnapshot
	rowCacheStablePrompts
	rowLean
	rowTabTitleLength
	rowInterruptConfirmation
	behaviorRowCount
)

const (
	rowSound = iota
	rowSoundThreshold
	rowWarnOnCacheMiss
	notificationsRowCount
)

var sidebarPositions = []messages.SidebarPosition{
	messages.SidebarRight, messages.SidebarLeft, messages.SidebarTop, messages.SidebarBottom,
}

var positionLabels = map[messages.SidebarPosition]string{
	messages.SidebarRight: "Right", messages.SidebarLeft: "Left",
	messages.SidebarTop: "Top", messages.SidebarBottom: "Bottom",
}

var sectionSpacings = []messages.SectionSpacing{
	messages.SpacingCompact, messages.SpacingNormal, messages.SpacingRelaxed,
}

var spacingLabels = map[messages.SectionSpacing]string{
	messages.SpacingCompact: "Compact", messages.SpacingNormal: "Normal", messages.SpacingRelaxed: "Relaxed",
}

var sidebarInfoModes = []messages.SidebarInfoMode{
	messages.InfoModeCompact, messages.InfoModeDetailed,
}

var infoModeLabels = map[messages.SidebarInfoMode]string{
	messages.InfoModeCompact: "Compact", messages.InfoModeDetailed: "Detailed",
}

var sendModes = []messages.SendMode{messages.SendModeSteer, messages.SendModeQueue}

type sendModeOption struct {
	mode  messages.SendMode
	label string
	desc  string
}

var sendModeOptions = []sendModeOption{
	{messages.SendModeSteer, "Steer", "send to the working agent mid-turn"},
	{messages.SendModeQueue, "Queue", "hold until the current turn ends"},
}

var interruptConfirmationModes = []messages.InterruptMode{
	messages.InterruptModeAlways, messages.InterruptModeDoubleTap, messages.InterruptModeNone,
}

var interruptConfirmationLabels = map[messages.InterruptMode]string{
	messages.InterruptModeAlways:    "Always (confirm dialog)",
	messages.InterruptModeDoubleTap: "Double-tap (press Esc twice)",
	messages.InterruptModeNone:      "None (immediate)",
}

type settingsDialog struct {
	BaseDialog

	original    messages.Preferences
	current     messages.Preferences
	showVisuals bool
	tab         int
	selected    [tabCount]int
	confirmYOLO bool
}

func NewSettingsDialog(preferences messages.Preferences, showVisuals bool) Dialog {
	preferences.Layout.SidebarPosition = messages.ParseSidebarPosition(string(preferences.Layout.SidebarPosition))
	preferences.Layout.SectionSpacing = messages.ParseSectionSpacing(string(preferences.Layout.SectionSpacing))
	preferences.Layout.SidebarInfoMode = messages.ParseSidebarInfoMode(string(preferences.Layout.SidebarInfoMode))
	preferences.SendMode = messages.ParseSendMode(string(preferences.SendMode))
	if preferences.TabTitleMaxLength <= 0 {
		preferences.TabTitleMaxLength = 20
	}
	if preferences.SoundThreshold <= 0 {
		preferences.SoundThreshold = 10
	}
	if preferences.InterruptConfirmation == "" {
		preferences.InterruptConfirmation = messages.InterruptModeAlways
	}
	return &settingsDialog{original: preferences, current: preferences, showVisuals: showVisuals}
}

func (d *settingsDialog) Init() tea.Cmd { return nil }

func (d *settingsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		cmd := d.handleKey(msg)
		return d, cmd
	}
	return d, nil
}

func (d *settingsDialog) rowCount() int {
	switch d.tab {
	case tabBehavior:
		return behaviorRowCount
	case tabNotifications:
		return notificationsRowCount
	default:
		return appearanceRowCount
	}
}

func (d *settingsDialog) selectable(tab, row int) bool {
	if tab == tabAppearance && !d.showVisuals && row >= rowPosition && row <= rowTodos {
		return false
	}
	if tab == tabAppearance && row == rowActiveAgents && d.current.Layout.HideAgents {
		return false
	}
	if tab == tabNotifications && row == rowSoundThreshold && !d.current.Sound {
		return false
	}
	return true
}

func (d *settingsDialog) moveSelection(delta int) {
	for next := d.selected[d.tab] + delta; next >= 0 && next < d.rowCount(); next += delta {
		if d.selectable(d.tab, next) {
			d.selected[d.tab] = next
			return
		}
	}
}

func (d *settingsDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q":
		return d.cancel()
	case "tab":
		d.confirmYOLO = false
		d.tab = (d.tab + 1) % tabCount
	case "shift+tab":
		d.confirmYOLO = false
		d.tab = (d.tab + tabCount - 1) % tabCount
	case "up", "k", "ctrl+k":
		d.confirmYOLO = false
		d.moveSelection(-1)
	case "down", "j", "ctrl+j":
		d.confirmYOLO = false
		d.moveSelection(1)
	case "home", "g":
		d.selected[d.tab] = 0
	case "end", "G":
		d.selected[d.tab] = d.rowCount() - 1
		if !d.selectable(d.tab, d.selected[d.tab]) {
			d.moveSelection(-1)
		}
	case "left", "h":
		return d.changeValue(-1)
	case "right", "l", "space":
		return d.changeValue(1)
	case "enter":
		if d.tab == tabAppearance && d.selected[d.tab] == rowTheme {
			return core.CmdHandler(messages.OpenThemePickerMsg{})
		}
		if d.tab == tabBehavior && d.selected[d.tab] == rowYOLO && !d.current.YOLO {
			return d.changeValue(1)
		}
		return d.apply()
	}
	return nil
}

func (d *settingsDialog) changeValue(delta int) tea.Cmd {
	switch d.tab {
	case tabAppearance:
		switch d.selected[d.tab] {
		case rowTheme:
			if delta > 0 {
				return core.CmdHandler(messages.OpenThemePickerMsg{})
			}
		case rowPosition:
			d.current.Layout.SidebarPosition = cycleValue(sidebarPositions, d.current.Layout.SidebarPosition, delta)
		case rowSpacing:
			d.current.Layout.SectionSpacing = cycleValue(sectionSpacings, d.current.Layout.SectionSpacing, delta)
		case rowInfoMode:
			d.current.Layout.SidebarInfoMode = cycleValue(sidebarInfoModes, d.current.Layout.SidebarInfoMode, delta)
		case rowSessionPath:
			d.current.Layout.HideSessionPath = !d.current.Layout.HideSessionPath
		case rowUsage:
			d.current.Layout.HideUsage = !d.current.Layout.HideUsage
		case rowAgents:
			d.current.Layout.HideAgents = !d.current.Layout.HideAgents
		case rowActiveAgents:
			if !d.current.Layout.HideAgents {
				d.current.Layout.ActiveAgentsOnly = !d.current.Layout.ActiveAgentsOnly
			}
		case rowTools:
			d.current.Layout.HideTools = !d.current.Layout.HideTools
		case rowTodos:
			d.current.Layout.HideTodos = !d.current.Layout.HideTodos
		case rowSplitDiff:
			d.current.SplitDiffView = !d.current.SplitDiffView
		case rowExpandThinking:
			d.current.ExpandThinking = !d.current.ExpandThinking
		case rowHideToolResults:
			d.current.HideToolResults = !d.current.HideToolResults
		case rowRenderImages:
			d.current.RenderImages = !d.current.RenderImages
		case rowShowBanner:
			d.current.ShowBanner = !d.current.ShowBanner
		}
		if d.selected[d.tab] >= rowPosition && d.selected[d.tab] <= rowTodos {
			return core.CmdHandler(messages.PreviewLayoutMsg{Layout: d.current.Layout})
		}
	case tabBehavior:
		switch d.selected[d.tab] {
		case rowSendMode:
			d.current.SendMode = cycleValue(sendModes, d.current.SendMode, delta)
		case rowYOLO:
			switch {
			case d.current.YOLO:
				d.current.YOLO = false
				d.confirmYOLO = false
			case d.confirmYOLO:
				d.current.YOLO = true
				d.confirmYOLO = false
			default:
				d.confirmYOLO = true
			}
		case rowRestoreTabs:
			d.current.RestoreTabs = !d.current.RestoreTabs
		case rowSnapshot:
			d.current.Snapshot = !d.current.Snapshot
		case rowCacheStablePrompts:
			d.current.CacheStablePrompts = !d.current.CacheStablePrompts
		case rowLean:
			d.current.Lean = !d.current.Lean
		case rowTabTitleLength:
			d.current.TabTitleMaxLength = stepValue(d.current.TabTitleMaxLength, delta, 1, 5, 100)
		case rowInterruptConfirmation:
			d.current.InterruptConfirmation = cycleValue(interruptConfirmationModes, d.current.InterruptConfirmation, delta)
		}
	case tabNotifications:
		switch d.selected[d.tab] {
		case rowSound:
			d.current.Sound = !d.current.Sound
			if !d.current.Sound && d.selected[d.tab] == rowSoundThreshold {
				d.selected[d.tab] = rowSound
			}
		case rowSoundThreshold:
			if d.current.Sound {
				d.current.SoundThreshold = stepValue(d.current.SoundThreshold, delta, 1, 1, 300)
			}
		case rowWarnOnCacheMiss:
			d.current.WarnOnCacheMiss = !d.current.WarnOnCacheMiss
		}
	}
	return nil
}

func cycleValue[T comparable](values []T, current T, delta int) T {
	idx := 0
	for i, v := range values {
		if v == current {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(values)) % len(values)
	return values[idx]
}

func stepValue(current, delta, step, minimum, maximum int) int {
	return max(minimum, min(maximum, current+delta*step))
}

func (d *settingsDialog) apply() tea.Cmd {
	if d.current == d.original {
		return closeDialogCmd()
	}
	return tea.Sequence(closeDialogCmd(), core.CmdHandler(messages.ApplySettingsMsg{Preferences: d.current}))
}

func (d *settingsDialog) cancel() tea.Cmd {
	if d.current.Layout == d.original.Layout {
		return closeDialogCmd()
	}
	return tea.Sequence(closeDialogCmd(), core.CmdHandler(messages.CancelLayoutPreviewMsg{Original: d.original.Layout}))
}

func (d *settingsDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *settingsDialog) View() string {
	width := d.ComputeDialogWidth(settingsWidthPercent, settingsMinWidth, settingsMaxWidth)
	inner := d.ContentWidth(width, 2)
	content := NewContent(inner).AddTitle("Settings").AddSeparator().AddSpace().AddContent(d.renderTabBar(inner))
	switch d.tab {
	case tabBehavior:
		d.renderBehaviorTab(content, inner)
	case tabNotifications:
		d.renderNotificationsTab(content, inner)
	default:
		d.renderAppearanceTab(content, inner)
	}
	content.AddSpace().AddHelpKeys("↑/↓", "navigate", "←/→", "change", "tab", "switch tab", "enter", "apply", "esc", "cancel")
	return styles.DialogStyle.Width(width).Render(content.Build())
}

func (d *settingsDialog) renderTabBar(width int) string {
	tabs := make([]string, 0, tabCount)
	for i, label := range settingsTabLabels {
		style := styles.MutedStyle
		if i == d.tab {
			style = styles.HighlightWhiteStyle.Underline(true)
		}
		tabs = append(tabs, style.Render(label))
	}
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, strings.Join(tabs, "    "))
}

func (d *settingsDialog) renderAppearanceTab(content *Content, inner int) {
	theme := styles.GetPersistedThemeRef()
	if theme == "" {
		theme = styles.DefaultThemeRef
	}
	content.AddSpace().AddContent(d.renderSelectorRow(rowTheme, "Theme", theme, inner))
	if d.showVisuals {
		preview := lipgloss.NewStyle().Width(inner).Align(lipgloss.Center).Render(renderLayoutPreview(d.current.Layout, inner))
		content.AddSpace().AddContent(preview).AddSpace().
			AddContent(d.renderSelectorRow(rowPosition, "Sidebar position", positionLabels[d.current.Layout.SidebarPosition], inner)).
			AddContent(d.renderSelectorRow(rowSpacing, "Section spacing", spacingLabels[d.current.Layout.SectionSpacing], inner)).
			AddContent(d.renderSelectorRow(rowInfoMode, "Sidebar info mode", infoModeLabels[d.current.Layout.SidebarInfoMode], inner)).
			AddContent(styles.MutedStyle.Render("Sidebar sections")).
			AddContent(d.renderToggleRow(rowSessionPath, "Session path", !d.current.Layout.HideSessionPath)).
			AddContent(d.renderToggleRow(rowUsage, "Token usage", !d.current.Layout.HideUsage)).
			AddContent(d.renderToggleRow(rowAgents, "Agents", !d.current.Layout.HideAgents)).
			AddContent(d.renderNestedToggleRow(rowActiveAgents, "Active agents only", d.current.Layout.ActiveAgentsOnly, d.current.Layout.HideAgents)).
			AddContent(d.renderToggleRow(rowTools, "Tools", !d.current.Layout.HideTools)).
			AddContent(d.renderToggleRow(rowTodos, "Todos", !d.current.Layout.HideTodos))
	}
	content.AddSpace().
		AddContent(d.renderToggleRow(rowSplitDiff, "Split diff view", d.current.SplitDiffView)).
		AddContent(d.renderToggleRow(rowExpandThinking, "Expand thinking by default", d.current.ExpandThinking)).
		AddContent(d.renderToggleRow(rowHideToolResults, "Hide tool results by default", d.current.HideToolResults)).
		AddContent(d.renderToggleRow(rowRenderImages, "Render images", d.current.RenderImages)).
		AddContent(d.renderToggleRow(rowShowBanner, "Show startup banner", d.current.ShowBanner))
}

func (d *settingsDialog) renderBehaviorTab(content *Content, inner int) {
	content.AddSpace().AddContent(styles.MutedStyle.Render("While agent is working")).AddSpace()
	for _, opt := range sendModeOptions {
		content.AddContent(d.renderSendModeOption(opt))
	}
	content.AddSpace().
		AddContent(d.renderToggleRow(rowYOLO, "Auto-approve tools by default", d.current.YOLO)).
		AddContent(d.renderToggleRow(rowRestoreTabs, "Restore tabs on launch", d.current.RestoreTabs)).
		AddContent(d.renderToggleRow(rowSnapshot, "Automatic snapshots", d.current.Snapshot)).
		AddContent(d.renderToggleRow(rowCacheStablePrompts, "Cache-stable dynamic prompts", d.current.CacheStablePrompts)).
		AddContent(d.renderToggleRow(rowLean, "Lean UI by default", d.current.Lean)).
		AddContent(d.renderStepperRow(rowTabTitleLength, "Tab title max length", d.current.TabTitleMaxLength, "chars", inner, false))
	content.AddContent(d.renderSelectorRow(rowInterruptConfirmation, "Interrupt confirmation", interruptConfirmationLabels[d.current.InterruptConfirmation], inner))
	if d.confirmYOLO {
		content.AddSpace().AddContent(styles.MutedStyle.Render("Auto-approve can run tools without confirmation. Press again to enable."))
	}
	content.AddContent(styles.MutedStyle.Render("Restore tabs and lean UI apply on next launch."))
}

func (d *settingsDialog) renderNotificationsTab(content *Content, inner int) {
	content.AddSpace().
		AddContent(d.renderToggleRow(rowSound, "Sound on task completion", d.current.Sound)).
		AddContent(d.renderStepperRow(rowSoundThreshold, "Sound threshold", d.current.SoundThreshold, "seconds", inner, !d.current.Sound)).
		AddContent(d.renderToggleRow(rowWarnOnCacheMiss, "Warn when a turn misses the cache", d.current.WarnOnCacheMiss))
}

func (d *settingsDialog) renderSendModeOption(opt sendModeOption) string {
	glyph, prefix := "○", "  "
	labelStyle, glyphStyle := styles.PaletteUnselectedActionStyle, styles.SecondaryStyle
	if d.current.SendMode == opt.mode {
		glyph, prefix = "●", styles.HighlightWhiteStyle.Render("› ")
		labelStyle = styles.PaletteSelectedActionStyle
		glyphStyle = styles.SecondaryStyle.Foreground(styles.Success)
	}
	return prefix + glyphStyle.Render(glyph) + " " + labelStyle.Render(opt.label) + "   " + styles.MutedStyle.Render(opt.desc)
}

func (d *settingsDialog) renderSelectorRow(row int, label, valueLabel string, width int) string {
	value := "‹ " + valueLabel + " ›"
	labelStyle, valueStyle, prefix := styles.PaletteUnselectedActionStyle, styles.SecondaryStyle, "  "
	if d.selected[d.tab] == row {
		labelStyle, valueStyle, prefix = styles.PaletteSelectedActionStyle, styles.HighlightWhiteStyle, styles.HighlightWhiteStyle.Render("› ")
	}
	left := prefix + labelStyle.Render(label)
	return left + strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(value))) + valueStyle.Render(value)
}

func (d *settingsDialog) renderStepperRow(row int, label string, value int, unit string, width int, disabled bool) string {
	if disabled {
		left, right := "  "+styles.MutedStyle.Render(label), styles.MutedStyle.Render(fmt.Sprintf("‹ %d %s ›", value, unit))
		return left + strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(right))) + right
	}
	return d.renderSelectorRow(row, label, fmt.Sprintf("%d %s", value, unit), width)
}

func (d *settingsDialog) renderToggleRow(row int, label string, enabled bool) string {
	check := "[ ]"
	if enabled {
		check = "[x]"
	}
	labelStyle, checkStyle, prefix := styles.PaletteUnselectedActionStyle, styles.SecondaryStyle, "  "
	if d.selected[d.tab] == row {
		labelStyle, checkStyle, prefix = styles.PaletteSelectedActionStyle, styles.HighlightWhiteStyle, styles.HighlightWhiteStyle.Render("› ")
	}
	if enabled {
		checkStyle = checkStyle.Foreground(styles.Success)
	}
	return prefix + checkStyle.Render(check) + " " + labelStyle.Render(label)
}

// renderNestedToggleRow renders a toggle row indented under its parent row.
// A disabled row (its parent section is off) renders fully muted and is
// skipped by navigation (see selectable).
func (d *settingsDialog) renderNestedToggleRow(row int, label string, enabled, disabled bool) string {
	if disabled {
		check := "[ ]"
		if enabled {
			check = "[x]"
		}
		return "    " + styles.MutedStyle.Render(check+" "+label)
	}
	return "  " + d.renderToggleRow(row, label, enabled)
}

// visibleSectionLabels returns the sidebar section labels that are visible
// under the given settings. The session block is always shown; its label
// reads "session/path" while the session path is visible and "session" once
// it is hidden.
func visibleSectionLabels(s messages.LayoutSettings) []string {
	sessionLabel := "session/path"
	if s.HideSessionPath {
		sessionLabel = "session"
	}
	labels := []string{sessionLabel}
	if !s.HideUsage {
		labels = append(labels, "usage")
	}
	if !s.HideAgents {
		labels = append(labels, "agents")
	}
	if !s.HideTools {
		labels = append(labels, "tools")
	}
	if !s.HideTodos {
		labels = append(labels, "todos")
	}
	return labels
}

// renderLayoutPreview draws a small schematic of the resulting layout: the
// chat box, the input box, and the sidebar (side column or horizontal band)
// listing only the visible sections. maxWidth caps the schematic width.
func renderLayoutPreview(s messages.LayoutSettings, maxWidth int) string {
	width := max(previewMinWidth, min(previewMaxWidth, maxWidth))
	switch messages.ParseSidebarPosition(string(s.SidebarPosition)) {
	case messages.SidebarLeft:
		return renderSidePreview(s, width, true)
	case messages.SidebarTop:
		return renderBandPreview(s, width, false)
	case messages.SidebarBottom:
		return renderBandPreview(s, width, true)
	default:
		return renderSidePreview(s, width, false)
	}
}

// borderStyle renders preview borders.
var previewBorder = func(text string) string { return styles.FadingStyle.Render(text) }

// previewLabelCell renders a label padded to width inside a preview box cell.
func previewLabelCell(label string, width int, style lipgloss.Style) string {
	label = toolcommon.TruncateText(label, max(0, width-1))
	cell := " " + style.Render(label)
	return cell + strings.Repeat(" ", max(0, width-lipgloss.Width(cell)))
}

// previewCenteredCell renders a label centered within width.
func previewCenteredCell(label string, width int, style lipgloss.Style) string {
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, style.Render(label))
}

// renderSidePreview draws the vertical layout with the sidebar on the left or right.
func renderSidePreview(s messages.LayoutSettings, width int, onLeft bool) string {
	inner := width - 2
	sideW := max(9, inner/3)
	chatW := inner - sideW - 1
	const contentRows = 5

	labels := visibleSectionLabels(s)
	sectionStyle := styles.TabAccentStyle

	var lines []string
	hbarChat := strings.Repeat("─", chatW)
	hbarSide := strings.Repeat("─", sideW)
	hbarFull := strings.Repeat("─", inner)

	if onLeft {
		lines = append(lines, previewBorder("╭"+hbarSide+"┬"+hbarChat+"╮"))
	} else {
		lines = append(lines, previewBorder("╭"+hbarChat+"┬"+hbarSide+"╮"))
	}

	for i := range contentRows {
		var side, chat string
		if i < len(labels) {
			side = previewLabelCell(labels[i], sideW, sectionStyle)
		} else {
			side = strings.Repeat(" ", sideW)
		}
		if i == contentRows/2 {
			chat = previewCenteredCell("chat", chatW, styles.SecondaryStyle)
		} else {
			chat = strings.Repeat(" ", chatW)
		}

		v := previewBorder("│")
		if onLeft {
			lines = append(lines, v+side+v+chat+v)
		} else {
			lines = append(lines, v+chat+v+side+v)
		}
	}

	if onLeft {
		lines = append(lines, previewBorder("├"+hbarSide+"┴"+hbarChat+"┤"))
	} else {
		lines = append(lines, previewBorder("├"+hbarChat+"┴"+hbarSide+"┤"))
	}
	lines = append(lines,
		previewBorder("│")+previewLabelCell("input", inner, styles.SecondaryStyle)+previewBorder("│"),
		previewBorder("╰"+hbarFull+"╯"),
	)

	return strings.Join(lines, "\n")
}

// renderBandPreview draws the layout with the sidebar as a horizontal band
// above (bottom=false) or below (bottom=true) the chat.
func renderBandPreview(s messages.LayoutSettings, width int, bottom bool) string {
	inner := width - 2
	const chatRows = 3

	band := previewLabelCell(strings.Join(visibleSectionLabels(s), " · "), inner, styles.TabAccentStyle)
	hbarFull := strings.Repeat("─", inner)
	v := previewBorder("│")

	chatLines := make([]string, 0, chatRows)
	for i := range chatRows {
		if i == chatRows/2 {
			chatLines = append(chatLines, v+previewCenteredCell("chat", inner, styles.SecondaryStyle)+v)
		} else {
			chatLines = append(chatLines, v+strings.Repeat(" ", inner)+v)
		}
	}

	lines := []string{previewBorder("╭" + hbarFull + "╮")}
	if bottom {
		lines = append(lines, chatLines...)
		lines = append(lines, previewBorder("├"+hbarFull+"┤"), v+band+v)
	} else {
		lines = append(lines, v+band+v, previewBorder("├"+hbarFull+"┤"))
		lines = append(lines, chatLines...)
	}
	lines = append(lines,
		previewBorder("├"+hbarFull+"┤"),
		v+previewLabelCell("input", inner, styles.SecondaryStyle)+v,
		previewBorder("╰"+hbarFull+"╯"),
	)

	return strings.Join(lines, "\n")
}
