package tui

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// setupAutoThemeTest isolates the user config in a temp dir and restores the
// process-global theme state afterwards. Tests using it must not be parallel:
// they mutate the applied theme and the auto-theme flags shared by the styles
// package.
func setupAutoThemeTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	paths.SetDataDir(dir)
	prevDark := styles.TerminalIsDark()
	prevEnabled := styles.AutoThemeEnabled()
	t.Cleanup(func() {
		paths.SetConfigDir("")
		paths.SetDataDir("")
		styles.SetTerminalDark(prevDark)
		styles.SetAutoThemeEnabled(prevEnabled)
		styles.ApplyTheme(styles.DefaultTheme())
	})
}

func rawMsgs(msgs []tea.Msg) []string {
	var raws []string
	for _, msg := range msgs {
		if raw, ok := msg.(tea.RawMsg); ok {
			raws = append(raws, raw.Msg.(string))
		}
	}
	return raws
}

func TestAutoThemeInitCmd(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(false)
	assert.Nil(t, m.autoThemeInitCmd(), "no sequence should be emitted when auto theme is off")
	assert.False(t, m.lightDarkModeSet)

	styles.SetAutoThemeEnabled(true)
	msgs := collectMsgs(m.autoThemeInitCmd())
	assert.Contains(t, rawMsgs(msgs), ansi.SetModeLightDark)
	assert.True(t, m.lightDarkModeSet)
}

func TestQuitCmdResetsLightDarkMode(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	msgs := collectMsgs(m.quitCmd())
	assert.True(t, hasMsg[tea.QuitMsg](msgs))
	assert.Empty(t, rawMsgs(msgs), "no mode reset expected when 2031 was never set")

	m.lightDarkModeSet = true
	msgs = collectMsgs(m.quitCmd())
	assert.True(t, hasMsg[tea.QuitMsg](msgs))
	assert.Contains(t, rawMsgs(msgs), ansi.ResetModeLightDark)
	assert.False(t, m.lightDarkModeSet)
}

func TestColorSchemeEventSwitchesAutoTheme(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(true)
	styles.SetTerminalDark(true)
	styles.ApplyTheme(styles.DefaultTheme())

	_, cmd := m.Update(uv.LightColorSchemeEvent{})
	assert.False(t, styles.TerminalIsDark())
	assert.Equal(t, styles.DefaultLightThemeRef, styles.CurrentTheme().Ref)
	assert.True(t, hasMsg[messages.ThemeChangedMsg](collectMsgs(cmd)))

	_, cmd = m.Update(uv.DarkColorSchemeEvent{})
	assert.True(t, styles.TerminalIsDark())
	assert.Equal(t, styles.DefaultThemeRef, styles.CurrentTheme().Ref)
	assert.True(t, hasMsg[messages.ThemeChangedMsg](collectMsgs(cmd)))

	// Same polarity again: no redundant theme application.
	_, cmd = m.Update(uv.DarkColorSchemeEvent{})
	assert.False(t, hasMsg[messages.ThemeChangedMsg](collectMsgs(cmd)))
}

func TestColorSchemeEventIgnoredWithoutAutoTheme(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(false)
	styles.SetTerminalDark(true)
	styles.ApplyTheme(styles.DefaultTheme())

	_, cmd := m.Update(uv.LightColorSchemeEvent{})
	assert.Equal(t, styles.DefaultThemeRef, styles.CurrentTheme().Ref, "theme must not change when auto is off")
	assert.False(t, hasMsg[messages.ThemeChangedMsg](collectMsgs(cmd)))
	assert.False(t, styles.TerminalIsDark(), "polarity should still be recorded")
}

func TestBackgroundColorMsgDrivesAutoTheme(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(true)
	styles.SetTerminalDark(true)
	styles.ApplyTheme(styles.DefaultTheme())

	_, _ = m.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, styles.DefaultLightThemeRef, styles.CurrentTheme().Ref)

	_, _ = m.Update(tea.BackgroundColorMsg{Color: color.Black})
	assert.Equal(t, styles.DefaultThemeRef, styles.CurrentTheme().Ref)
}

func TestHandleChangeThemeAutoSelection(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(false)
	styles.SetTerminalDark(false)
	styles.ApplyTheme(styles.DefaultTheme())

	_, cmd := m.handleChangeTheme(styles.AutoThemeRef)
	require.NotNil(t, cmd)

	assert.True(t, styles.AutoThemeEnabled())
	assert.Equal(t, styles.DefaultLightThemeRef, styles.CurrentTheme().Ref)
	assert.Equal(t, styles.AutoThemeRef, styles.GetPersistedThemeRef())
	assert.True(t, m.lightDarkModeSet)
	assert.Contains(t, rawMsgs(collectMsgs(cmd)), ansi.SetModeLightDark)
}

func TestHandleChangeThemeAwayFromAutoResetsMode(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(true)
	styles.SetTerminalDark(true)
	styles.ApplyTheme(styles.DefaultTheme())
	m.lightDarkModeSet = true

	_, cmd := m.handleChangeTheme("nord")
	require.NotNil(t, cmd)

	assert.False(t, styles.AutoThemeEnabled())
	assert.Equal(t, "nord", styles.CurrentTheme().Ref)
	assert.Equal(t, "nord", styles.GetPersistedThemeRef())
	assert.False(t, m.lightDarkModeSet)
	assert.Contains(t, rawMsgs(collectMsgs(cmd)), ansi.ResetModeLightDark)
}

func TestHandleChangeThemeAutoNoOpWhenAlreadyActive(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(true)
	styles.SetTerminalDark(true)
	require.NoError(t, styles.SaveThemeToUserConfig(styles.AutoThemeRef))

	_, cmd := m.handleChangeTheme(styles.AutoThemeRef)
	assert.Nil(t, cmd, "re-selecting the active auto theme should be a no-op")
}

func TestHandleThemePreviewResolvesAuto(t *testing.T) {
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	styles.SetAutoThemeEnabled(false)
	styles.SetTerminalDark(false)
	styles.ApplyTheme(styles.DefaultTheme())

	_, _ = m.handleThemePreview(styles.AutoThemeRef)
	assert.Equal(t, styles.DefaultLightThemeRef, styles.CurrentTheme().Ref)
	assert.False(t, styles.AutoThemeEnabled(), "previewing auto must not enable it")
}

func TestThemeSwitchRecolorsDrafts(t *testing.T) {
	setupAutoThemeTest(t)
	styles.ApplyTheme(styles.DefaultTheme())
	m := newTabLifecycleModel(t)
	first := m.activeTab.editor
	first.SetValue("Q1")
	_, _ = m.handleSpawnSession("/second")
	_, _ = m.Update(messages.RequestFocusMsg{Target: messages.PanelEditor})
	for _, r := range "Q2" {
		_, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})

	checkDrafts := func() {
		t.Helper()
		want := color.RGBAModel.Convert(styles.TextPrimary)
		assert.Equal(t, want, themeTestForeground(t, m.activeTab.editor.View(), "Q"), "active draft")
		assert.Equal(t, want, themeTestForeground(t, first.View(), "Q"), "background draft")
	}
	checkDrafts()
	_, cmd := m.Update(messages.ChangeThemeMsg{ThemeRef: styles.DefaultLightThemeRef})
	for _, msg := range collectMsgs(cmd) {
		_, _ = m.Update(msg)
	}
	require.Equal(t, styles.DefaultLightThemeRef, styles.CurrentTheme().Ref)
	checkDrafts()
	_, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.Equal(t, "Qx2", m.activeTab.editor.Value())
	require.Equal(t, "Q1", first.Value())
}

func TestThemeSwitchRecolorsInactiveTab(t *testing.T) {
	setupAutoThemeTest(t)
	styles.ApplyTheme(styles.DefaultTheme())
	m := newTabLifecycleModel(t)
	firstID := m.supervisor.ActiveID()
	page := m.activeTab.chatPage
	_, _ = m.Update(messages.RoutedMsg{SessionID: firstID, Inner: runtime.UserMessage("Ω first prompt", m.application.Session().ID, nil)})
	_ = m.resizeAll()
	beforePrompt := themeTestForeground(t, page.View(), "Ω")

	event := &runtime.ElicitationRequestEvent{ElicitationID: "first"}
	_, _ = m.Update(queuedRuntimeDelivery(m, firstID, event))
	prompt := m.dialogMgr.TopDialog()
	require.NotNil(t, prompt)
	_, _ = m.Update(tea.PasteMsg{Content: "Ψ unfinished answer"})
	beforeInput := themeTestForeground(t, prompt.View(), "Ψ")

	_, _ = m.handleSpawnSession("/second")
	require.NotSame(t, page, m.activeTab.chatPage)
	require.Same(t, prompt, m.tabs[firstID].attentionDialogs[event])
	_, _ = m.Update(messages.ThemePreviewMsg{ThemeRef: styles.DefaultLightThemeRef})
	require.Equal(t, styles.DefaultLightThemeRef, styles.CurrentTheme().Ref)

	want := color.RGBAModel.Convert(styles.TextPrimary)
	require.NotEqual(t, want, beforePrompt)
	require.NotEqual(t, want, beforeInput)
	assert.Equal(t, want, themeTestForeground(t, page.View(), "Ω"), "inactive transcript updates without a resize")
	_, _ = m.handleSwitchTab(firstID)
	require.Same(t, prompt, m.dialogMgr.TopDialog())
	assert.Equal(t, want, themeTestForeground(t, prompt.View(), "Ψ"), "parked input keeps its draft and uses the new color")
}

func TestThemeSwitchRecolorsLeanWorkingIndicator(t *testing.T) {
	setupAutoThemeTest(t)
	styles.ApplyTheme(styles.DefaultTheme())
	m := newSpawnTestModel(t, &spySpawner{}, WithLeanMode())
	m.initSessionComponents(m.supervisor.ActiveID(), m.application, m.application.Session())
	t.Cleanup(func() {
		m.cleanupAll()
		m.cleanupManagedResources()
	})
	_, _ = m.handleWorkingStateChanged(messages.WorkingStateChangedMsg{Working: true})
	spinner := m.workingSpinner
	dots := m.workingSpinner.RawFrame()
	before := m.renderLeanWorkingIndicator()
	oldColor := themeTestForeground(t, before, dots)
	assert.Equal(t, oldColor, themeTestForeground(t, before, "W"))

	_, _ = m.Update(messages.ThemePreviewMsg{ThemeRef: styles.DefaultLightThemeRef})
	want := color.RGBAModel.Convert(styles.TabAccentFg)
	require.NotEqual(t, want, oldColor)
	require.Same(t, spinner, m.workingSpinner)
	after := m.renderLeanWorkingIndicator()
	assert.Equal(t, ansi.Strip(before), ansi.Strip(after), "theme switch should not advance the animation")
	assert.Equal(t, want, themeTestForeground(t, after, dots))
	assert.Equal(t, want, themeTestForeground(t, after, "W"))
}

func themeTestForeground(t *testing.T, view, content string) color.Color {
	t.Helper()
	for _, line := range uv.NewStyledString(view).Lines(ansi.GraphemeWidth) {
		for _, cell := range line {
			if cell.Content == content {
				require.NotNil(t, cell.Style.Fg, "text has no foreground color")
				return color.RGBAModel.Convert(cell.Style.Fg)
			}
		}
	}
	t.Fatalf("%q missing from rendered view", content)
	return nil
}
