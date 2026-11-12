package leantui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func setupLeanSettingsTest(t *testing.T) {
	t.Helper()
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
}

func TestSettingsCommand(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	require.True(t, m.handleSlash(t.Context(), "/settings", busySubmitSteer))
	require.NotNil(t, m.screen.Settings)
	lines, _, _ := m.buildLines()
	view := strings.Join(lines, "\n")
	assert.Contains(t, view, "Settings")
	assert.Contains(t, view, "Cache-stable dynamic prompts")
	for _, excluded := range []string{"Appearance", "Theme", "Sidebar", "Restore tabs", "Interrupt confirmation"} {
		assert.NotContains(t, view, excluded)
	}
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRight})
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Nil(t, m.screen.Settings)
	assert.Empty(t, m.sendMode)
	_, err := os.Stat(userconfig.Path())
	assert.True(t, os.IsNotExist(err))
}

func TestSettingsSavePreservesOtherPreferences(t *testing.T) {
	setupLeanSettingsTest(t)
	cfg := &userconfig.Config{Settings: &userconfig.Settings{
		Theme: "dracula", Sound: true, RestoreTabs: new(true),
		Layout: &userconfig.LayoutSettings{SidebarPosition: "left"},
	}}
	require.NoError(t, cfg.Save())
	m := bareModel(80)
	m.openSettings()
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	m.screen.Settings.Selected = settingWarnOnCacheMiss
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRune, Runes: []rune{' '}})
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.Nil(t, m.screen.Settings)
	assert.Equal(t, messages.SendModeQueue, m.sendMode)
	s := userconfig.Get()
	assert.Equal(t, "queue", s.GetBusySendMode())
	assert.True(t, s.CacheMissWarningsEnabled())
	assert.Equal(t, cfg.Settings.Theme, s.Theme)
	assert.Equal(t, cfg.Settings.Sound, s.Sound)
	assert.Equal(t, cfg.Settings.RestoreTabs, s.RestoreTabs)
	assert.Equal(t, cfg.Settings.Layout, s.Layout)
}

func TestSettingsKeepAwake(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	assert.False(t, m.settings.values[settingKeepAwake])
	m.screen.Settings.Selected = settingKeepAwake
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.True(t, userconfig.Get().GetKeepAwake())
	m.openSettings()
	assert.True(t, m.settings.values[settingKeepAwake])
	m.screen.Settings.Selected = settingKeepAwake
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.False(t, userconfig.Get().GetKeepAwake())
}

func TestSettingsYOLORequiresConfirmation(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	m.screen.Settings.Selected = settingYOLO
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.True(t, m.screen.Settings.ConfirmYOLO)
	assert.NotContains(t, strings.Join(m.screen.Settings.Render(120), "\n"), "Auto-approve can run tools without confirmation")
	assert.False(t, m.settings.values[settingYOLO])
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.False(t, m.screen.Settings.ConfirmYOLO)
	assert.True(t, m.settings.values[settingYOLO])
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.True(t, userconfig.Get().YOLO)
}

func TestSettingsSaveFailureKeepsPanelOpen(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	require.NoError(t, os.WriteFile(userconfig.Path(), []byte("settings: ["), 0o600))
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.NotNil(t, m.screen.Settings)
	assert.Empty(t, m.sendMode)
	lines, _, _ := m.buildLines()
	assert.Contains(t, strings.Join(lines, "\n"), "Failed to save settings")
}

func TestSettingsBusySendModeQueuesMessages(t *testing.T) {
	rt := &cycleThinkingRuntime{}
	m := bareModel(80)
	m.app = app.New(t.Context(), rt, session.New())
	m.busy = true
	m.sendMode = messages.SendModeQueue
	m.submitEditor(t.Context(), "next task")
	assert.Empty(t, rt.steered)
	require.Len(t, m.queue, 1)
	assert.Equal(t, "next task", m.queue[0].Content)
}

func TestSettingsOnlySavesChangedValues(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Lean: true}
		return nil
	}))
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.True(t, userconfig.Get().Lean)
}

func TestSettingsSoundThreshold(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	m.screen.Settings.Selected = settingSoundThreshold
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.Equal(t, userconfig.DefaultSoundThreshold, m.settings.soundThreshold)
	m.screen.Settings.Selected = settingSound
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	m.screen.Settings.Selected = settingSoundThreshold
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.Equal(t, userconfig.DefaultSoundThreshold+1, m.settings.soundThreshold)
	m.settings.soundThreshold = 300
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.Equal(t, 300, m.settings.soundThreshold)
	m.settings.soundThreshold = 1
	m.handleSettingsKey(ui.Key{Typ: ui.KeyLeft})
	assert.Equal(t, 1, m.settings.soundThreshold)
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.True(t, userconfig.Get().GetSound())
	assert.Equal(t, 1, userconfig.Get().GetSoundThreshold())
}

func TestSettingsBackgroundCompaction(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	assert.False(t, m.settings.values[settingBackgroundCompaction])
	assert.Equal(t, 70, m.settings.backgroundThreshold)
	assert.Contains(t, m.screen.Settings.Rows[settingBackgroundCompaction].Label, "Background compaction")
	assert.Equal(t, "70%", m.screen.Settings.Rows[settingBackgroundCompactionThreshold].Value)
	m.screen.Settings.Selected = settingBackgroundCompaction
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	m.screen.Settings.Selected = settingBackgroundCompactionThreshold
	m.handleSettingsKey(ui.Key{Typ: ui.KeyLeft})
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.True(t, userconfig.Get().BackgroundCompaction)
	assert.Equal(t, 69, userconfig.Get().GetBackgroundCompactionThreshold())

	m.openSettings()
	assert.True(t, m.settings.values[settingBackgroundCompaction])
	assert.Equal(t, 69, m.settings.backgroundThreshold)
	m.screen.Settings.Selected = settingBackgroundCompactionThreshold
	m.settings.backgroundThreshold = 99
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.Equal(t, 99, m.settings.backgroundThreshold)
	m.settings.backgroundThreshold = 1
	m.handleSettingsKey(ui.Key{Typ: ui.KeyLeft})
	assert.Equal(t, 1, m.settings.backgroundThreshold)
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEsc})
	assert.Equal(t, 69, userconfig.Get().GetBackgroundCompactionThreshold())

	m.openSettings()
	m.screen.Settings.Selected = settingBackgroundCompactionThreshold
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Zero(t, cfg.Settings.BackgroundCompactionThreshold, "default is omitted from config")
	assert.True(t, cfg.Settings.BackgroundCompaction)
}

func TestSettingsRenderingPreferences(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.renderImages = true
	m.openSettings()
	for _, row := range []int{settingRenderImages, settingShowBanner, settingSplitDiff} {
		m.screen.Settings.Selected = row
		m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	}
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.False(t, m.renderImages)
	assert.True(t, m.hideBanner)
	assert.False(t, m.sessionState.SplitDiffView())
	s := userconfig.Get()
	assert.False(t, s.GetRenderImages())
	assert.False(t, s.GetShowBanner())
	assert.False(t, s.GetSplitDiffView())
}

func TestCompletionSound(t *testing.T) {
	setupLeanSettingsTest(t)
	require.NoError(t, (&userconfig.Config{Settings: &userconfig.Settings{Sound: true, SoundThreshold: 10}}).Save())
	for _, tc := range []struct {
		name     string
		duration time.Duration
		reason   string
		want     bool
	}{
		{"completed", 11 * time.Second, "normal", true},
		{"too short", time.Second, "normal", false},
		{"cancelled", 11 * time.Second, "cancelled", false},
		{"failed", 11 * time.Second, "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bareModel(80)
			var played []sound.Event
			m.playSound = func(_ context.Context, event sound.Event) { played = append(played, event) }
			m.handleEvent(t.Context(), runtime.StreamStarted("root", "root"))
			m.streamStartTime = time.Now().Add(-tc.duration)
			m.handleEvent(t.Context(), runtime.StreamStarted("child", "child"))
			m.handleEvent(t.Context(), runtime.StreamStopped("child", "child", "normal"))
			assert.Empty(t, played)
			assert.True(t, m.busy)
			m.handleEvent(t.Context(), runtime.StreamStopped("root", "root", tc.reason))
			assert.Equal(t, tc.want, len(played) == 1)
			m.handleEvent(t.Context(), runtime.StreamStopped("root", "root", tc.reason))
			assert.LessOrEqual(t, len(played), 1)
		})
	}
}

func TestFailureSound(t *testing.T) {
	setupLeanSettingsTest(t)
	require.NoError(t, (&userconfig.Config{Settings: &userconfig.Settings{Sound: true}}).Save())
	m := bareModel(80)
	var played []sound.Event
	m.playSound = func(_ context.Context, event sound.Event) { played = append(played, event) }
	m.handleEvent(t.Context(), &runtime.ErrorEvent{Error: "failed"})
	assert.Equal(t, []sound.Event{sound.Failure}, played)
	require.NoError(t, (&userconfig.Config{}).Save())
	m.handleEvent(t.Context(), &runtime.ErrorEvent{Error: "failed"})
	assert.Len(t, played, 1)
}

func TestSettingsImageToggleNeverReadsLiveInput(t *testing.T) {
	setupLeanSettingsTest(t)
	for _, supported := range []bool{false, true} {
		m := bareModel(80)
		m.imageSupport = supported
		require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
			cfg.Settings = &userconfig.Settings{RenderImages: new(false)}
			return nil
		}))
		m.openSettings()
		m.screen.Settings.Selected = settingRenderImages
		m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
		m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
		assert.Equal(t, supported, m.renderImages, "only confirmed startup support can enable graphics")
		assert.True(t, userconfig.Get().GetRenderImages())
		m.openSettings()
		assert.True(t, m.settings.values[settingRenderImages], "the panel must show the persisted preference")
	}
}

func TestSettingsWrittenFilePreview(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.openSettings()
	assert.Equal(t, "Last 10 lines", m.screen.Settings.Rows[settingFullFilePreview].Value)
	m.screen.Settings.Selected = settingFullFilePreview
	m.handleSettingsKey(ui.Key{Typ: ui.KeyRight})
	assert.Equal(t, "Full", m.screen.Settings.Rows[settingFullFilePreview].Value)
	m.handleSettingsKey(ui.Key{Typ: ui.KeyEnter})
	assert.True(t, userconfig.Get().LeanFullFilePreview)
	m.openSettings()
	assert.True(t, m.settings.values[settingFullFilePreview])
}

func TestCtrlOTogglesFilePreview(t *testing.T) {
	setupLeanSettingsTest(t)
	m := bareModel(80)
	m.screen.Editor.SetText("keep my input")
	assert.False(t, m.screen.Transcript.FullFilePreview())
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyCtrlO})
	assert.True(t, m.screen.Transcript.FullFilePreview())
	assert.Equal(t, "keep my input", m.screen.Editor.Text())
	assert.False(t, userconfig.Get().LeanFullFilePreview, "shortcut only changes the current session")
	m.openSettings()
	assert.Equal(t, "Full", m.screen.Settings.Rows[settingFullFilePreview].Value)
	m.closeSettings()
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyCtrlO})
	assert.False(t, m.screen.Transcript.FullFilePreview())
}
