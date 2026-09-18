package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func newExitAfterResponseModel(t *testing.T) (*appModel, string) {
	t.Helper()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	sess := session.New(session.WithWorkingDir("/initial"))
	a := app.New(t.Context(), stubRuntime{}, sess, app.WithExitAfterFirstResponse())
	m.supervisor.ReplaceRunnerApp(t.Context(), id, a, "/initial", nil)
	m.application = a
	m.bindTabSession(id, sess.ID)
	m.initSessionComponents(id, a, sess)
	_, _ = m.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.StreamStarted(sess.ID, "root")})
	_, _ = m.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.AgentChoice("root", sess.ID, "answer")})
	return m, id
}

func TestHiddenTabExitAfterResponseIsGlobal(t *testing.T) {
	t.Parallel()
	m, id := newExitAfterResponseModel(t)
	_, _ = m.handleSpawnSession("/other")
	_, cmd := m.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.StreamStopped("", "root", "normal")})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1, "only the global exit is dispatched; visible chrome stays untouched")
	effect, ok := msgs[0].(chat.GlobalMsg)
	require.True(t, ok)
	assert.Equal(t, id, effect.TabID)
	assert.Same(t, m.tabs[id].chatPage, effect.Origin)
	assert.IsType(t, messages.ExitAfterFirstResponseMsg{}, effect.Inner)
	_, quit := m.Update(effect)
	assert.True(t, hasMsg[tea.QuitMsg](collectMsgs(quit)), "a hidden page must still exit after its first response")
}

func TestPageGlobalEffectRejectsRetiredOrigin(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"switch", "close", "clear", "restore", "reload-same", "switch-back", "replace-app"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			m, id := newExitAfterResponseModel(t)
			_, cmd := m.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.StreamStopped("", "root", "normal")})
			var effect chat.GlobalMsg
			for _, msg := range collectMsgs(cmd) {
				if global, ok := msg.(chat.GlobalMsg); ok {
					effect = global
				}
			}
			require.NotNil(t, effect.Origin)
			switch change {
			case "switch":
				_, _ = m.handleSpawnSession("/other")
			case "switch-back":
				_, _ = m.handleSpawnSession("/other")
				_, _ = m.handleSwitchTab(id)
			case "close":
				_, _ = m.handleSpawnSession("/other")
				_, _ = m.handleCloseTab(id)
			case "replace-app":
				a := app.New(t.Context(), stubRuntime{}, session.New())
				m.supervisor.ReplaceRunnerApp(t.Context(), id, a, "/initial", nil)
			case "clear":
				_, _ = m.handleClearSession()
			case "restore":
				_, _ = m.replaceActiveSession(t.Context(), session.New(session.WithWorkingDir("/initial")))
			case "reload-same":
				_, _ = m.replaceActiveSession(t.Context(), m.application.Session())
			}
			_, next := m.Update(effect)
			if change == "switch" || change == "switch-back" {
				assert.True(t, hasMsg[tea.QuitMsg](collectMsgs(next)))
			} else {
				assert.Nil(t, next, "a retired page must not terminate its replacement")
			}
		})
	}
}

func TestHiddenAttentionEffectsDoNotDuplicateFIFO(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	origin, application := m.activeTab, m.application
	_, _ = m.handleSpawnSession("/other")
	first := &runtime.ElicitationRequestEvent{ElicitationID: "first", Message: "first prompt"}
	second := &runtime.ElicitationRequestEvent{ElicitationID: "second", Message: "second prompt"}
	for _, event := range []tea.Msg{first, second} {
		_, cmd := m.Update(messages.RoutedMsg{SessionID: id, Inner: event})
		assert.False(t, hasMsg[dialog.OpenDialogMsg](collectMsgs(cmd)), "hidden attention must not dispatch a dialog command")
	}
	assert.False(t, m.dialogMgr.Open())

	// Activate without replaying, so the native replay sequence can be checked.
	m.supervisor.SwitchTo(id)
	m.activeTab, m.application = origin, application
	_ = m.replayPendingEvent(id)
	opened := m.dialogMgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true })
	require.Len(t, opened, 2)
	assert.Same(t, first, opened[0].OriginatingEvent)
	assert.Same(t, second, opened[1].OriginatingEvent)
	assert.Nil(t, m.replayPendingEvent(id), "the FIFO is consumed exactly once")
}
