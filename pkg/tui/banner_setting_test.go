package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// bannerVisible builds a chat page the way initSessionComponents does and
// reports whether the startup banner is drawn on the empty conversation.
func bannerVisible(t *testing.T, m *appModel) bool {
	t.Helper()
	sess := session.New()
	page := chat.New(animation.NewRuntime(), t.Context(),
		app.New(t.Context(), stubRuntime{}, sess), service.NewSessionState(sess), m.chatPageOpts()...)
	page.SetSize(160, 40)
	return strings.Contains(ansi.Strip(page.View()), tuibanner.Lines[0])
}

// TestChatPageOpts_PassesShowBanner covers pages built after startup (new
// tab, /clear, /fork, …): they must inherit the retained banner preference.
func TestChatPageOpts_PassesShowBanner(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { return nil }

	m.showBanner = true
	assert.True(t, bannerVisible(t, m))

	m.showBanner = false
	assert.False(t, bannerVisible(t, m), "a disabled banner must not be drawn")
}

// TestHandleApplySettings_AppliesShowBanner proves toggling the banner in
// /settings reaches the appModel, every existing page, and the user config.
func TestHandleApplySettings_AppliesShowBanner(t *testing.T) {
	setupSettingsConfigTest(t)

	m := newApplySettingsModel(t)
	second := &mockChatPage{showBanner: true}
	m.chatPages["second"] = second

	prefs := defaultTestPreferences()
	prefs.ShowBanner = false
	_, _ = m.handleApplySettings(messages.ApplySettingsMsg{Preferences: prefs})

	assert.False(t, m.showBanner, "the preference is retained for future pages")
	assert.False(t, m.chatPage.(*mockChatPage).showBanner, "the active page hides the banner")
	assert.False(t, second.showBanner, "background pages hide the banner")
	assert.False(t, userconfig.Get().GetShowBanner(), "the preference is persisted")
}

// TestNew_AppliesPersistedShowBannerAtStartup covers the startup path: a
// persisted show_banner: false must reach the appModel and the initial page.
// Not parallel: the config/data dir overrides are process-global.
func TestNew_AppliesPersistedShowBannerAtStartup(t *testing.T) {
	dir := t.TempDir()
	paths.SetDataDir(filepath.Join(dir, "data"))
	paths.SetConfigDir(filepath.Join(dir, "config"))
	t.Cleanup(func() {
		paths.SetDataDir("")
		paths.SetConfigDir("")
	})
	showBanner := false
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{ShowBanner: &showBanner}
		return nil
	}))

	sess := session.New()
	m := New(t.Context(), nil, app.New(t.Context(), stubRuntime{}, sess), dir, func() {}).(*appModel)
	t.Cleanup(m.cleanupManagedResources)

	assert.False(t, m.showBanner)
	m.chatPage.SetSize(160, 40)
	assert.NotContains(t, ansi.Strip(m.chatPage.View()), tuibanner.Lines[0],
		"the initial page must honor the persisted setting")
}
