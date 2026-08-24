package tui

import (
	"context"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// buildPageFromOpts constructs a real chat page the way initSessionComponents
// does — from m.chatPageOpts() — and drives it into the working state so Esc
// reaches the interrupt handling.
func buildPageFromOpts(t *testing.T, m *appModel) chat.Page {
	t.Helper()
	sess := session.New()
	page := chat.New(animation.NewRuntime(), t.Context(),
		app.New(t.Context(), stubRuntime{}, sess), service.NewSessionState(sess), m.chatPageOpts()...)
	_, _ = page.Update(runtime.StreamStarted(sess.ID, "root"))
	t.Cleanup(func() { _, _ = page.Update(messages.StreamCancelledMsg{}) })
	return page
}

// opensInterruptDialog reports whether Esc on a working page opens the
// interrupt confirmation dialog — the "always" behavior. "double-tap" and
// "none" never open it.
func opensInterruptDialog(page chat.Page) bool {
	_, cmd := page.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	return hasMsg[dialog.OpenDialogMsg](collectMsgs(cmd))
}

// TestChatPageOpts_PassesRetainedInterruptMode covers the future-page
// regression: pages built after startup (new tab, /clear, /fork, session
// load, …) must inherit the retained interrupt mode instead of falling back
// to the confirmation dialog.
func TestChatPageOpts_PassesRetainedInterruptMode(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { return nil }

	m.interruptMode = messages.InterruptModeNone
	assert.False(t, opensInterruptDialog(buildPageFromOpts(t, m)),
		`a page built from chatPageOpts must inherit the retained "none" mode`)

	m.interruptMode = messages.InterruptModeAlways
	assert.True(t, opensInterruptDialog(buildPageFromOpts(t, m)),
		`the default "always" mode keeps the confirmation dialog`)
}

// newApplySettingsModel wires the minimal appModel state handleApplySettings
// touches around the newTestModel mocks.
func newApplySettingsModel(t *testing.T) *appModel {
	t.Helper()
	m, _ := newTestModel(t)
	m.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { return nil }
	m.leanMode = true
	m.tabBar = tabbar.New(animation.NewRuntime(), 0)
	m.sessionState = service.NewSessionState(session.New())
	return m
}

// defaultTestPreferences returns a Preferences with every value at its
// default, so tests only vary the interrupt confirmation.
func defaultTestPreferences() messages.Preferences {
	return messages.Preferences{
		Layout: messages.LayoutSettings{
			SidebarPosition: messages.SidebarRight,
			SectionSpacing:  messages.SpacingNormal,
			SidebarInfoMode: messages.InfoModeCompact,
		},
		SendMode:              messages.SendModeSteer,
		SplitDiffView:         true,
		RenderImages:          true,
		ShowBanner:            true,
		TabTitleMaxLength:     userconfig.DefaultTabTitleMaxLength,
		SoundThreshold:        userconfig.DefaultSoundThreshold,
		InterruptConfirmation: messages.InterruptModeAlways,
	}
}

// TestHandleApplySettings_RetainsInterruptMode proves applying /settings
// records the interrupt preference on the appModel and pushes it to every
// existing page, so a page created afterwards receives it too.
func TestHandleApplySettings_RetainsInterruptMode(t *testing.T) {
	setupSettingsConfigTest(t)

	m := newApplySettingsModel(t)
	second := &mockChatPage{}
	m.chatPages["second"] = second

	prefs := defaultTestPreferences()
	prefs.InterruptConfirmation = messages.InterruptModeNone
	_, _ = m.handleApplySettings(messages.ApplySettingsMsg{Preferences: prefs})

	assert.Equal(t, messages.InterruptModeNone, m.interruptMode,
		"the preference is retained for future pages")
	assert.Equal(t, messages.InterruptModeNone, m.chatPage.(*mockChatPage).interruptMode,
		"the active page receives the new mode")
	assert.Equal(t, messages.InterruptModeNone, second.interruptMode,
		"background pages receive the new mode")
	assert.False(t, opensInterruptDialog(buildPageFromOpts(t, m)),
		"a page created after applying settings receives the new mode")
}

// TestHandleApplySettings_NormalizesInvalidInterruptMode proves an invalid
// submitted value is normalized before being retained and applied, so the
// default "always" behavior is preserved.
func TestHandleApplySettings_NormalizesInvalidInterruptMode(t *testing.T) {
	setupSettingsConfigTest(t)

	m := newApplySettingsModel(t)

	prefs := defaultTestPreferences()
	prefs.InterruptConfirmation = messages.InterruptMode("bogus")
	_, _ = m.handleApplySettings(messages.ApplySettingsMsg{Preferences: prefs})

	assert.Equal(t, messages.InterruptModeAlways, m.interruptMode)
	assert.Equal(t, messages.InterruptModeAlways, m.chatPage.(*mockChatPage).interruptMode,
		"pages receive the normalized mode, not the raw value")
}

// TestNew_AppliesPersistedInterruptModeAtStartup covers the startup path:
// the persisted interrupt confirmation setting must reach the appModel and
// the initial chat page built through chatPageOpts. Not parallel: the
// config/data dir overrides are process-global.
func TestNew_AppliesPersistedInterruptModeAtStartup(t *testing.T) {
	tests := []struct {
		name      string
		persisted string
		want      messages.InterruptMode
	}{
		{name: "persisted double-tap applies", persisted: "double-tap", want: messages.InterruptModeDoubleTap},
		{name: "invalid value falls back to always", persisted: "bogus", want: messages.InterruptModeAlways},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			paths.SetDataDir(filepath.Join(dir, "data"))
			paths.SetConfigDir(filepath.Join(dir, "config"))
			t.Cleanup(func() {
				paths.SetDataDir("")
				paths.SetConfigDir("")
			})
			require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
				cfg.Settings = &userconfig.Settings{InterruptConfirmation: tt.persisted}
				return nil
			}))

			sess := session.New()
			m := New(t.Context(), nil, app.New(t.Context(), stubRuntime{}, sess), dir, func() {}).(*appModel)
			t.Cleanup(m.cleanupManagedResources)

			assert.Equal(t, tt.want, m.interruptMode)

			_, _ = m.chatPage.Update(runtime.StreamStarted(sess.ID, "root"))
			t.Cleanup(func() { _, _ = m.chatPage.Update(messages.StreamCancelledMsg{}) })
			assert.Equal(t, tt.want == messages.InterruptModeAlways, opensInterruptDialog(m.chatPage),
				"the initial page must honor the persisted mode")
		})
	}
}
