package tui

import (
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
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestFadeScrollEdgesSetting(t *testing.T) {
	setupSettingsConfigTest(t)
	paths.SetDataDir(filepath.Join(t.TempDir(), "data"))
	t.Cleanup(func() {
		paths.SetDataDir("")
		scrollview.SetEdgeFadeEnabled(true)
	})
	// An existing config with no scroll fade preference must still start on.
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{ShowBanner: new(false)}
		return nil
	}))
	start := func() *appModel {
		m := New(t.Context(), nil, app.New(t.Context(), stubRuntime{}, session.New()), t.TempDir(), func() {}).(*appModel)
		t.Cleanup(m.cleanupManagedResources)
		m.width, m.height = 100, 40
		return m
	}
	m := start()
	view := scrollview.New()
	view.SetSize(25, 3)
	lines := []string{"alpha", "beta", "gamma", "delta"}
	view.SetContent(lines, len(lines))
	faded := view.View()
	require.True(t, strings.HasPrefix(strings.Split(faded, "\n")[2], "\x1b[38;2;"),
		"an unset preference leaves plain text edge fading enabled")

	open := func() dialog.Dialog {
		_, cmd := m.handleOpenSettingsDialog()
		opened, ok := cmd().(dialog.OpenDialogMsg)
		require.True(t, ok)
		d := opened.Model
		d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		return d
	}
	toggle := func(d dialog.Dialog) {
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
		require.Nil(t, cmd, "the edited value must not preview or save")
	}
	apply := func(d dialog.Dialog) {
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		msgs := collectMsgs(cmd)
		require.Len(t, msgs, 2)
		msg, ok := msgs[1].(messages.ApplySettingsMsg)
		require.True(t, ok)
		_, _ = m.handleApplySettings(msg)
	}

	d := open()
	assert.Contains(t, ansi.Strip(d.View()), "› [x] Fade scroll edges")
	toggle(d)
	assert.Contains(t, ansi.Strip(d.View()), "› [ ] Fade scroll edges")
	assert.Equal(t, faded, view.View())
	_, cancel := d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, []tea.Msg{dialog.CloseDialogMsg{}}, collectMsgs(cancel))
	assert.True(t, userconfig.Get().GetFadeScrollEdges())
	assert.Equal(t, faded, view.View())

	d = open()
	toggle(d)
	apply(d)
	plain := view.View()
	assert.Equal(t, ansi.Strip(faded), ansi.Strip(plain))
	assert.Contains(t, plain, "\ngamma", "scroll edge text has no fade style")
	saved, err := userconfig.Load()
	require.NoError(t, err)
	require.NotNil(t, saved.Settings.FadeScrollEdges, "off must be saved explicitly")
	assert.False(t, saved.Settings.GetFadeScrollEdges())

	// Reset the renderer to establish that startup really reads the persisted value.
	scrollview.SetEdgeFadeEnabled(true)
	m = start()
	assert.Equal(t, plain, view.View())
	d = open()
	assert.Contains(t, ansi.Strip(d.View()), "› [ ] Fade scroll edges")
	toggle(d)
	apply(d)
	assert.Equal(t, faded, view.View())
	saved, err = userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, saved.GetSettings().FadeScrollEdges, "restoring the default removes the saved override")
}
