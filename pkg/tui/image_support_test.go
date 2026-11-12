package tui

import (
	"io"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type imageSupportPage struct {
	mockChatPage

	changes int
}

func (p *imageSupportPage) UpdateEffects(msg tea.Msg) (chat.Page, chat.Effects) {
	if _, ok := msg.(messages.ImageRenderingChangedMsg); ok {
		p.changes++
	}
	return p, chat.Effects{}
}

func TestImagePreferenceInvalidatesEveryTab(t *testing.T) {
	m, _ := newTestModel(t)
	m.imageWriter = tuiimage.NewWriter(io.Discard)
	active, background := &imageSupportPage{}, &imageSupportPage{}
	m.activeTab.chatPage = active
	m.ensureTab("background").chatPage = background
	m.ensureTab("pending")
	t.Cleanup(func() { tuiimage.SetRenderingEnabled(true) })

	for _, enabled := range []bool{false, true} {
		m.viewCacheValid = true
		before := m.imageWriter.RenderingEnabled()
		m.imageWriter.SetEnabled(enabled)
		_ = m.syncImageRendering(before)
		assert.Equal(t, enabled, m.imageWriter.RenderingEnabled())
		assert.False(t, m.viewCacheValid)
	}
	assert.Equal(t, 2, active.changes)
	assert.Equal(t, 2, background.changes)

	_ = m.syncImageRendering(m.imageWriter.RenderingEnabled())
	assert.Equal(t, 2, active.changes, "unchanged preferences must preserve cached views")
}

func TestApplySettingsDoesNotProbeUnknownImageSupport(t *testing.T) {
	setupSettingsConfigTest(t)
	m := newApplySettingsModel(t)
	m.imageWriter = tuiimage.NewWriter(io.Discard)
	m.imageWriter.SetSupported(false)
	m.imageWriter.SetEnabled(false)
	tuiimage.SetRenderingEnabled(false)
	t.Cleanup(func() { tuiimage.SetRenderingEnabled(true) })

	prefs := defaultTestPreferences()
	_, cmd := m.handleApplySettings(messages.ApplySettingsMsg{Preferences: prefs})
	msgs := collectMsgs(cmd)
	assert.Contains(t, msgs, notification.ShowMsg{Text: "Settings updated. Restart to check terminal image support.", Type: notification.TypeInfo})
	assert.False(t, hasMsg[tea.RawMsg](msgs))
	assert.True(t, m.imageWriter.Enabled(), "persist the preference without reading live input")
	assert.False(t, m.imageWriter.RenderingEnabled())
	assert.True(t, userconfig.Get().GetRenderImages())
}

func TestApplySettingsUsesConfirmedImageSupport(t *testing.T) {
	setupSettingsConfigTest(t)
	m := newApplySettingsModel(t)
	m.imageWriter = tuiimage.NewWriter(io.Discard)
	t.Cleanup(func() { tuiimage.SetRenderingEnabled(true) })
	prefs := defaultTestPreferences()
	for _, enabled := range []bool{false, true} {
		prefs.RenderImages = enabled
		_, cmd := m.handleApplySettings(messages.ApplySettingsMsg{Preferences: prefs})
		assert.Contains(t, collectMsgs(cmd), notification.ShowMsg{Text: "Settings updated", Type: notification.TypeSuccess})
		assert.Equal(t, enabled, m.imageWriter.RenderingEnabled())
	}
}
