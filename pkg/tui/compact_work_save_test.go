package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// recordingChatPage keeps the last Compact work and visible-replies values a
// tab received, so the root wiring can be observed per tab.
type recordingChatPage struct {
	*mockChatPage

	compactWorkLog bool
	policy         messages.VisibleReplies
}

// UpdateEffects must hand back the recorder itself: the mock returns its inner
// page, which would silently replace the recorder on the first tab update.
func (p *recordingChatPage) UpdateEffects(tea.Msg) (chat.Page, chat.Effects) {
	return p, chat.Effects{}
}

func (p *recordingChatPage) SetCompactWorkLog(enabled bool) tea.Cmd {
	p.compactWorkLog = enabled
	return nil
}
func (p *recordingChatPage) SetVisibleReplies(policy messages.VisibleReplies) { p.policy = policy }

// Saving the Behavior settings must reach every open tab, persist to the user
// config, survive a restart, and let preview/cancel move each tab back.
func TestCompactWorkAndVisibleRepliesSaveRestartAndPreview(t *testing.T) {
	setupSettingsConfigTest(t)

	m := newTabLifecycleModel(t)
	_, _ = m.handleSpawnSession("/second")
	var pages []*recordingChatPage
	for _, tab := range m.tabs {
		page := &recordingChatPage{mockChatPage: &mockChatPage{}}
		tab.chatPage = page
		pages = append(pages, page)
	}
	require.Len(t, pages, 2)

	settings := userconfig.Get()
	assert.False(t, settings.GetCompactWorkLog(), "Compact work log defaults off")
	assert.Equal(t, "3", settings.GetVisibleReplies(), "visible replies default to Last 3")

	layout := messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact}
	saved := messages.Preferences{
		Layout:            layout,
		SendMode:          messages.SendModeSteer,
		SplitDiffView:     true,
		ShowBanner:        true,
		CompactWorkLog:    true,
		VisibleReplies:    messages.VisibleRepliesFive,
		TabTitleMaxLength: userconfig.DefaultTabTitleMaxLength,
		SoundThreshold:    userconfig.DefaultSoundThreshold,
	}
	_, _ = m.handleApplySettings(messages.ApplySettingsMsg{Preferences: saved})

	for _, page := range pages {
		assert.True(t, page.compactWorkLog, "every tab gets Compact work log")
		assert.Equal(t, messages.VisibleRepliesFive, page.policy, "every tab gets the saved policy")
	}
	settings = userconfig.Get()
	assert.True(t, settings.GetCompactWorkLog(), "Compact work log is persisted")
	assert.Equal(t, "5", settings.GetVisibleReplies(), "Last 5 is persisted")

	_, _ = m.Update(messages.PreviewVisibleRepliesMsg{Policy: messages.VisibleRepliesLatest})
	for i, page := range pages {
		assert.Equal(t, messages.VisibleRepliesLatest, page.policy, "preview applies live to tab %d", i)
	}
	_, _ = m.Update(messages.CancelLayoutPreviewMsg{Original: layout, OriginalVisibleReplies: messages.VisibleRepliesFive})
	for i, page := range pages {
		assert.Equal(t, messages.VisibleRepliesFive, page.policy, "cancel restores the saved policy on tab %d", i)
	}

	// Restart: a new app model reads the persisted settings.
	spawner := func(ctx context.Context, _ string) (*app.App, *session.Session, func(), error) {
		sess := session.New()
		return app.New(ctx, stubRuntime{}, sess), sess, func() {}, nil
	}
	restarted := New(t.Context(), spawner, app.New(t.Context(), stubRuntime{}, session.New()), t.TempDir(), func() {}).(*appModel)
	t.Cleanup(restarted.cleanupManagedResources)
	assert.True(t, restarted.compactWorkLog, "restart restores Compact work log")
	assert.Equal(t, messages.VisibleRepliesFive, restarted.visibleReplies, "restart restores the saved policy")
}
