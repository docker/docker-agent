package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// setupSettingsConfigTest isolates the user config in a temp dir. Tests using
// it must not be parallel: the config dir override is process-global.
func setupSettingsConfigTest(t *testing.T) {
	t.Helper()
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
}

func TestLayoutSettingsFromConfig(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact},
		layoutSettingsFromConfig(userconfig.LayoutSettings{}),
		"empty config falls back to the default position, spacing and info mode")

	assert.Equal(t,
		messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact},
		layoutSettingsFromConfig(userconfig.LayoutSettings{SidebarPosition: "bogus", SectionSpacing: "bogus", SidebarInfoMode: "bogus"}),
		"unknown values fall back to the defaults")

	got := layoutSettingsFromConfig(userconfig.LayoutSettings{
		SidebarPosition:  "bottom",
		SectionSpacing:   "compact",
		SidebarInfoMode:  "detailed",
		ActiveAgentsOnly: true,
		HideSessionPath:  true,
		HideUsage:        true,
		HideAgents:       true,
		HideTools:        true,
		HideTodos:        true,
	})
	assert.Equal(t, messages.LayoutSettings{
		SidebarPosition:  messages.SidebarBottom,
		SectionSpacing:   messages.SpacingCompact,
		SidebarInfoMode:  messages.InfoModeDetailed,
		ActiveAgentsOnly: true,
		HideSessionPath:  true,
		HideUsage:        true,
		HideAgents:       true,
		HideTools:        true,
		HideTodos:        true,
	}, got)
}

func TestSaveSettingsToUserConfig_RoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition: messages.SidebarLeft,
		SectionSpacing:  messages.SpacingRelaxed,
		SidebarInfoMode: messages.InfoModeCompact,
		HideSessionPath: true,
		HideTools:       true,
	}
	require.NoError(t, saveSettingsToUserConfig(saved, messages.SendModeQueue))

	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))
	assert.Equal(t, messages.SendModeQueue, messages.ParseSendMode(userconfig.Get().GetBusySendMode()))
}

func TestSaveSettingsToUserConfig_DefaultsClearEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, saveSettingsToUserConfig(messages.LayoutSettings{
		SidebarPosition: messages.SidebarTop,
	}, messages.SendModeQueue))
	require.NoError(t, saveSettingsToUserConfig(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
	}, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetSettings().Layout, "default layout clears the config entry")
	assert.Empty(t, cfg.GetSettings().GetBusySendMode(), "the default send mode is not written out")
}

func TestSaveSettingsToUserConfig_OmitsDefaultPosition(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, saveSettingsToUserConfig(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		HideUsage:       true,
	}, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout)
	assert.Empty(t, layout.SidebarPosition, "the default position is not written out")
	assert.Empty(t, layout.SectionSpacing, "the default spacing is not written out")
	assert.True(t, layout.HideUsage)
}

func TestSavePreferences_RoundTripAndPreservesExtra(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Extra: map[string]any{"future_setting": "kept"}}
		return nil
	}))

	preferences := messages.Preferences{
		Layout: messages.LayoutSettings{
			SidebarPosition:  messages.SidebarBottom,
			SectionSpacing:   messages.SpacingCompact,
			SidebarInfoMode:  messages.InfoModeDetailed,
			ActiveAgentsOnly: true,
			HideAgents:       true,
		},
		SendMode:           messages.SendModeQueue,
		SplitDiffView:      false,
		ExpandThinking:     true,
		HideToolResults:    true,
		ShowBanner:         false,
		YOLO:               true,
		RestoreTabs:        true,
		Snapshot:           true,
		CacheStablePrompts: true,
		WarnOnCacheMiss:    true,
		Lean:               true,
		TabTitleMaxLength:  42,
		Sound:              true,
		SoundThreshold:     17,
	}
	require.NoError(t, savePreferences(preferences))

	settings := userconfig.Get()
	assert.Equal(t, preferences.Layout, layoutSettingsFromConfig(settings.GetLayout()))
	assert.Equal(t, preferences.SendMode, messages.ParseSendMode(settings.GetBusySendMode()))
	assert.Equal(t, preferences.SplitDiffView, settings.GetSplitDiffView())
	assert.Equal(t, preferences.ExpandThinking, settings.GetExpandThinking())
	assert.Equal(t, preferences.HideToolResults, settings.HideToolResults)
	assert.Equal(t, preferences.ShowBanner, settings.GetShowBanner())
	assert.Equal(t, preferences.YOLO, settings.YOLO)
	assert.Equal(t, preferences.RestoreTabs, settings.GetRestoreTabs())
	assert.Equal(t, preferences.Snapshot, settings.SnapshotsEnabled())
	assert.Equal(t, preferences.CacheStablePrompts, settings.CacheStablePromptsEnabled())
	assert.Equal(t, preferences.WarnOnCacheMiss, settings.CacheMissWarningsEnabled())
	assert.Equal(t, preferences.Lean, settings.Lean)
	assert.Equal(t, preferences.TabTitleMaxLength, settings.GetTabTitleMaxLength())
	assert.Equal(t, preferences.Sound, settings.GetSound())
	assert.Equal(t, preferences.SoundThreshold, settings.GetSoundThreshold())
	assert.Equal(t, "kept", settings.Extra["future_setting"])
}

func TestSavePreferences_DefaultsClearEntries(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, savePreferences(messages.Preferences{
		Layout:             messages.LayoutSettings{SidebarPosition: messages.SidebarLeft},
		SendMode:           messages.SendModeQueue,
		SplitDiffView:      false,
		ExpandThinking:     true,
		RestoreTabs:        true,
		Snapshot:           true,
		CacheStablePrompts: true,
		WarnOnCacheMiss:    true,
		TabTitleMaxLength:  40,
		SoundThreshold:     40,
	}))
	require.NoError(t, savePreferences(messages.Preferences{
		Layout:            messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal},
		SendMode:          messages.SendModeSteer,
		SplitDiffView:     true,
		ShowBanner:        true,
		TabTitleMaxLength: userconfig.DefaultTabTitleMaxLength,
		SoundThreshold:    userconfig.DefaultSoundThreshold,
	}))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	settings := cfg.Settings
	require.NotNil(t, settings)
	assert.Nil(t, settings.Layout)
	assert.Empty(t, settings.BusySendMode)
	assert.Nil(t, settings.SplitDiffView)
	assert.Nil(t, settings.ShowBanner)
	assert.Nil(t, settings.ExpandThinking)
	assert.Nil(t, settings.RestoreTabs)
	assert.Nil(t, settings.Snapshot)
	assert.Nil(t, settings.CacheStablePrompts)
	assert.Nil(t, settings.WarnOnCacheMiss)
	assert.Zero(t, settings.TabTitleMaxLength)
	assert.Zero(t, settings.SoundThreshold)
}

func TestSaveSettingsToUserConfig_HideSessionPathKeepsEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeCompact,
		HideSessionPath: true,
	}
	require.NoError(t, saveSettingsToUserConfig(saved, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout, "hide_session_path alone must keep the layout entry")
	assert.True(t, layout.HideSessionPath)
	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))
}

func TestSavePreferences_DetailedInfoModeKeepsEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeDetailed,
	}
	require.NoError(t, saveSettingsToUserConfig(saved, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout, "a detailed info mode alone must keep the layout entry")
	assert.Equal(t, "detailed", layout.SidebarInfoMode)
	assert.Empty(t, layout.SidebarPosition, "the default position is not written out")
	assert.Empty(t, layout.SectionSpacing, "the default spacing is not written out")
	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))
}

func TestSavePreferences_CompactInfoModeClearsEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, saveSettingsToUserConfig(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeDetailed,
	}, messages.SendModeSteer))
	require.NoError(t, saveSettingsToUserConfig(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeCompact,
	}, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetSettings().Layout,
		"the default layout with an explicit compact info mode clears the config entry")
}

func TestSavePreferences_ActiveAgentsOnlyRoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition:  messages.SidebarRight,
		SectionSpacing:   messages.SpacingNormal,
		SidebarInfoMode:  messages.InfoModeCompact,
		ActiveAgentsOnly: true,
	}
	require.NoError(t, saveSettingsToUserConfig(saved, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout, "active_agents_only alone must keep the layout entry")
	assert.True(t, layout.ActiveAgentsOnly)
	assert.Empty(t, layout.SidebarPosition, "the default position is not written out")
	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))

	// Turning the filter back off restores the all-default layout and clears
	// the entry entirely.
	saved.ActiveAgentsOnly = false
	require.NoError(t, saveSettingsToUserConfig(saved, messages.SendModeSteer))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetSettings().Layout, "the default (off) filter is not written out")
}
