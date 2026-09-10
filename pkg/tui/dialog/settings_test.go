package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func newTestSettingsDialog(t *testing.T, layout messages.LayoutSettings) *settingsDialog {
	t.Helper()
	d, ok := NewSettingsDialog(messages.Preferences{Layout: layout, SendMode: messages.SendModeSteer, SplitDiffView: true, RenderImages: true, ShowBanner: true, TabTitleMaxLength: 20, SoundThreshold: 10}, true).(*settingsDialog)
	require.True(t, ok)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	return d
}

func TestSettingsDialogNormalizesValues(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	assert.Equal(t, messages.SidebarRight, d.current.Layout.SidebarPosition)
	assert.Equal(t, messages.InfoModeCompact, d.current.Layout.SidebarInfoMode,
		"empty info mode normalizes to compact")

	raw, ok := NewSettingsDialog(messages.Preferences{
		SendMode: messages.SendMode("bogus"),
		Layout:   messages.LayoutSettings{SidebarInfoMode: messages.SidebarInfoMode("bogus")},
	}, true).(*settingsDialog)
	require.True(t, ok)
	assert.Equal(t, messages.SendModeSteer, raw.current.SendMode, "unknown send mode normalizes to steer")
	assert.Equal(t, messages.InfoModeCompact, raw.current.Layout.SidebarInfoMode,
		"unknown info mode normalizes to compact")
}

func TestSettingsDialogNavigation(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	up := tea.KeyPressMsg{Code: tea.KeyUp}

	require.Equal(t, rowTheme, d.selected[tabAppearance])

	d.Update(down)
	require.Equal(t, rowPosition, d.selected[tabAppearance])

	d.Update(down)
	require.Equal(t, rowSpacing, d.selected[tabAppearance])

	d.Update(down)
	require.Equal(t, rowInfoMode, d.selected[tabAppearance])

	d.Update(down)
	require.Equal(t, rowSessionPath, d.selected[tabAppearance])

	for range 20 {
		d.Update(down)
	}
	require.Equal(t, rowShowBanner, d.selected[tabAppearance], "down must stop at the last row")

	d.Update(up)
	require.Equal(t, rowRenderImages, d.selected[tabAppearance])
}

func TestSettingsDialogTabSwitching(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	require.Equal(t, tabAppearance, d.tab)

	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, tabBehavior, d.tab)

	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, tabNotifications, d.tab)

	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, tabAppearance, d.tab, "tab wraps around")

	d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.Equal(t, tabNotifications, d.tab, "shift+tab cycles backwards")
}

func TestSettingsDialogWithoutVisualsTab(t *testing.T) {
	t.Parallel()

	d, ok := NewSettingsDialog(messages.Preferences{SendMode: messages.SendModeSteer}, false).(*settingsDialog)
	require.True(t, ok)
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	require.Equal(t, tabAppearance, d.tab)

	view := ansi.Strip(d.View())
	assert.Contains(t, view, "Appearance")
	assert.NotContains(t, view, "Sidebar position")
	assert.NotContains(t, view, "Sidebar info mode")
	assert.NotContains(t, view, "Active agents only")
	assert.Contains(t, view, "Split diff view")

	assert.False(t, d.selectable(tabAppearance, rowInfoMode),
		"the info mode row is not selectable without a sidebar")
	assert.False(t, d.selectable(tabAppearance, rowActiveAgents),
		"the agent filter row is not selectable without a sidebar")
}

func TestSettingsDialogCyclesPositionAndPreviews(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowPosition

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "changing a value must emit a live preview")
	assert.Equal(t, messages.SidebarLeft, preview.Layout.SidebarPosition)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.SidebarTop, d.current.Layout.SidebarPosition)

	// Cycling backwards from the start wraps around.
	d.current.Layout.SidebarPosition = messages.SidebarRight
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, messages.SidebarBottom, d.current.Layout.SidebarPosition)
}

func TestSettingsDialogCyclesSpacingAndPreviews(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	require.Equal(t, messages.SpacingNormal, d.current.Layout.SectionSpacing,
		"empty spacing normalizes to normal")
	d.selected[tabAppearance] = rowSpacing

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "changing the spacing must emit a live preview")
	assert.Equal(t, messages.SpacingRelaxed, preview.Layout.SectionSpacing)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.SpacingCompact, d.current.Layout.SectionSpacing, "cycling wraps around")

	d.current.Layout.SectionSpacing = messages.SpacingNormal
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, messages.SpacingCompact, d.current.Layout.SectionSpacing)
}

func TestSettingsDialogCyclesInfoModeAndPreviews(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	require.Equal(t, messages.InfoModeCompact, d.current.Layout.SidebarInfoMode,
		"the info mode starts at the compact default")
	d.selected[tabAppearance] = rowInfoMode

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "changing the info mode must emit a live preview")
	assert.Equal(t, messages.InfoModeDetailed, preview.Layout.SidebarInfoMode)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.InfoModeCompact, d.current.Layout.SidebarInfoMode, "cycling wraps around")

	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, messages.InfoModeDetailed, d.current.Layout.SidebarInfoMode, "cycling backwards wraps around")
	msgs = collectMsgs(cmd)
	require.Len(t, msgs, 1)
	_, ok = msgs[0].(messages.PreviewLayoutMsg)
	assert.True(t, ok, "cycling backwards must also emit a live preview")
}

func TestSettingsDialogAppliesInfoMode(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowInfoMode
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2, "apply must close the dialog and emit the settings")
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok)
	assert.Equal(t, messages.InfoModeDetailed, applied.Preferences.Layout.SidebarInfoMode)
}

func TestSettingsDialogEscapeRestoresInfoMode(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowInfoMode
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, messages.InfoModeDetailed, d.current.Layout.SidebarInfoMode)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	cancel, ok := msgs[1].(messages.CancelLayoutPreviewMsg)
	require.True(t, ok, "esc after an info mode change must restore the original layout")
	assert.Equal(t, messages.InfoModeCompact, cancel.Original.SidebarInfoMode)
}

func TestSettingsDialogTogglesSection(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowUsage

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok)
	assert.True(t, preview.Layout.HideUsage, "space must hide the usage section")

	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.Layout.HideUsage, "space must toggle back")
}

func TestSettingsDialogTogglesActiveAgentsOnly(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowActiveAgents

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "toggling the agent filter must emit a live preview")
	assert.True(t, preview.Layout.ActiveAgentsOnly, "space must enable the filter")

	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.Layout.ActiveAgentsOnly, "space must toggle back")

	assert.Contains(t, ansi.Strip(d.View()), "Active agents only")
}

func TestSettingsDialogActiveAgentsRowDisabledWhenAgentsHidden(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{HideAgents: true, ActiveAgentsOnly: true})
	assert.False(t, d.selectable(tabAppearance, rowActiveAgents),
		"the nested row is not selectable while Agents is hidden")

	d.selected[tabAppearance] = rowAgents
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, rowTools, d.selected[tabAppearance], "navigation skips the disabled nested row")

	assert.Contains(t, ansi.Strip(d.View()), "Active agents only",
		"the disabled row still renders (muted)")

	// Re-showing Agents makes the nested row selectable again.
	d.selected[tabAppearance] = rowAgents
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	require.False(t, d.current.Layout.HideAgents)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, rowActiveAgents, d.selected[tabAppearance])
}

func TestSettingsDialogTogglesSessionPath(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowSessionPath

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "toggling the session path must emit a live preview")
	assert.True(t, preview.Layout.HideSessionPath, "space must hide the session path")

	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.Layout.HideSessionPath, "space must toggle back")
}

func TestSettingsDialogTogglesSendModeWithoutPreview(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, tabBehavior, d.tab)
	require.Equal(t, messages.SendModeSteer, d.current.SendMode)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Nil(t, cmd, "the send mode has no live preview")
	assert.Equal(t, messages.SendModeQueue, d.current.SendMode)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.SendModeSteer, d.current.SendMode, "cycling wraps around")
}

func TestSettingsDialogTogglesCacheStablePrompts(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabBehavior
	d.selected[tabBehavior] = rowCacheStablePrompts

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.Nil(t, cmd)
	assert.True(t, d.current.CacheStablePrompts)
	assert.Contains(t, ansi.Strip(d.View()), "Cache-stable dynamic prompts")
}

func TestSettingsDialogTogglesShowBanner(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowShowBanner
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	require.False(t, d.current.ShowBanner)
	assert.Contains(t, ansi.Strip(d.View()), "Show startup banner")

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2, "apply must close the dialog and emit the settings")
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok)
	assert.False(t, applied.Preferences.ShowBanner)
}

func TestSettingsDialogApplyEmitsApplySettings(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowTools
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2, "apply must close the dialog and emit the settings")
	_, ok := msgs[0].(CloseDialogMsg)
	require.True(t, ok)
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok)
	assert.True(t, applied.Preferences.Layout.HideTools)
	assert.Equal(t, messages.SendModeQueue, applied.Preferences.SendMode)
}

func TestSettingsDialogApplySendModeChangeOnly(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok, "a send-mode-only change must still be applied")
	assert.Equal(t, messages.SendModeQueue, applied.Preferences.SendMode)
}

func TestSettingsDialogApplyWithoutChangesOnlyCloses(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowPosition

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	_, ok := msgs[0].(CloseDialogMsg)
	assert.True(t, ok, "no changes: apply just closes")
}

func TestSettingsDialogEscapeRestoresOriginal(t *testing.T) {
	t.Parallel()

	original := messages.LayoutSettings{SidebarPosition: messages.SidebarLeft, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact}
	d := newTestSettingsDialog(t, original)
	d.selected[tabAppearance] = rowPosition
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	_, ok := msgs[0].(CloseDialogMsg)
	require.True(t, ok)
	cancel, ok := msgs[1].(messages.CancelLayoutPreviewMsg)
	require.True(t, ok, "esc after a change must restore the original layout")
	assert.Equal(t, original, cancel.Original)
}

func TestSettingsDialogEscapeWithSendModeChangeOnlyCloses(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1, "the send mode never previews, so there is nothing to roll back")
	_, ok := msgs[0].(CloseDialogMsg)
	assert.True(t, ok)
}

func TestSettingsDialogViewShowsVisualsRows(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	view := ansi.Strip(d.View())

	assert.Contains(t, view, "Settings")
	assert.Contains(t, view, "Appearance")
	assert.Contains(t, view, "Behavior")
	assert.Contains(t, view, "Sidebar position")
	assert.Contains(t, view, "Right")
	assert.Contains(t, view, "Section spacing")
	assert.Contains(t, view, "Normal")
	assert.Contains(t, view, "Sidebar info mode")
	assert.Contains(t, view, "‹ Compact ›", "the info mode selector shows the compact default")
	assert.Contains(t, view, "Session path")
	assert.Contains(t, view, "Token usage")
	assert.Contains(t, view, "Agents")
	assert.Contains(t, view, "Active agents only")
	assert.Contains(t, view, "Tools")
	assert.Contains(t, view, "Todos")
}

func TestSettingsDialogViewShowsBehaviorRows(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	view := ansi.Strip(d.View())

	assert.Contains(t, view, "While agent is working")
	assert.Contains(t, view, "● Steer", "steer starts selected")
	assert.Contains(t, view, "○ Queue")
	assert.Contains(t, view, "mid-turn")
	assert.Contains(t, view, "hold until the current turn ends")
	assert.NotContains(t, view, "Sidebar position", "behavior tab must not render visuals rows")

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	view = ansi.Strip(d.View())
	assert.Contains(t, view, "● Queue", "the mark moves to the chosen mode")
	assert.Contains(t, view, "○ Steer")
}

func TestStepValueClamps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                     string
		current, delta, expected int
	}{
		{name: "increments", current: 10, delta: 1, expected: 12},
		{name: "decrements", current: 10, delta: -1, expected: 8},
		{name: "minimum", current: 1, delta: -1, expected: 1},
		{name: "maximum", current: 20, delta: 1, expected: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, stepValue(tt.current, tt.delta, 2, 1, 20))
		})
	}
}

func TestSettingsDialogThemeRowOpensPicker(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	_, ok := cmd().(messages.OpenThemePickerMsg)
	assert.True(t, ok)
}

func TestSettingsDialogConfirmsYOLOBeforeEnabling(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabBehavior
	d.selected[tabBehavior] = rowYOLO
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.YOLO)
	assert.True(t, d.confirmYOLO)
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.True(t, d.current.YOLO)
	assert.False(t, d.confirmYOLO)
}

func TestSettingsDialogSoundThresholdDisabledWhenSoundOff(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabNotifications
	d.moveSelection(1)
	assert.Equal(t, rowWarnOnCacheMiss, d.selected[tabNotifications], "disabled threshold is skipped")
	d.selected[tabNotifications] = rowSound
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	d.moveSelection(1)
	assert.Equal(t, rowSoundThreshold, d.selected[tabNotifications])
}

func TestSettingsDialogTogglesCacheMissWarning(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabNotifications
	d.selected[tabNotifications] = rowWarnOnCacheMiss
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})

	assert.True(t, d.current.WarnOnCacheMiss)
	assert.Contains(t, ansi.Strip(d.View()), "Warn when a turn misses the cache")
}

func TestRenderLayoutPreviewReflectsSections(t *testing.T) {
	t.Parallel()

	full := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{}, previewMaxWidth))
	assert.Contains(t, full, "chat")
	assert.Contains(t, full, "input")
	assert.Contains(t, full, "session/path", "a visible session path shows in the session label")
	assert.Contains(t, full, "usage")
	assert.Contains(t, full, "todos")

	trimmed := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{
		HideSessionPath: true,
		HideUsage:       true,
		HideTodos:       true,
	}, previewMaxWidth))
	assert.NotContains(t, trimmed, "session/path", "a hidden session path shortens the session label")
	assert.Contains(t, trimmed, "session")
	assert.NotContains(t, trimmed, "usage")
	assert.NotContains(t, trimmed, "todos")
	assert.Contains(t, trimmed, "agents")
}

func TestRenderLayoutPreviewPositions(t *testing.T) {
	t.Parallel()

	for _, position := range sidebarPositions {
		preview := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{SidebarPosition: position}, previewMaxWidth))
		assert.Contains(t, preview, "chat", "position %s", position)
		assert.Contains(t, preview, "session", "position %s", position)
	}

	// Band layouts list the sections on a single line; the full label is
	// wider than the band and gets truncated at its tail.
	band := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{SidebarPosition: messages.SidebarTop}, previewMaxWidth))
	assert.Contains(t, band, "session/path · usage · agents · tools")

	hidden := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{
		SidebarPosition: messages.SidebarTop,
		HideSessionPath: true,
	}, previewMaxWidth))
	assert.Contains(t, hidden, "session · usage · agents · tools · todos")

	// Narrow widths truncate the band list instead of overflowing.
	narrow := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{SidebarPosition: messages.SidebarTop}, previewMinWidth))
	assert.Contains(t, narrow, "session")
}
