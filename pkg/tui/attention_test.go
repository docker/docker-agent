package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func attentionEvents() map[string]func() tea.Msg {
	return map[string]func() tea.Msg{
		"tool": func() tea.Msg {
			return runtime.ToolCallConfirmation(tools.ToolCall{ID: "tool"}, tools.Tool{Name: "shell"}, "root", nil)
		},
		"iterations": func() tea.Msg { return &runtime.MaxIterationsReachedEvent{MaxIterations: 10} },
		"form":       func() tea.Msg { return &runtime.ElicitationRequestEvent{ElicitationID: "form", Message: "question"} },
		"url": func() tea.Msg {
			return &runtime.ElicitationRequestEvent{ElicitationID: "url", Mode: "url", URL: "https://example.com"}
		},
		"oauth": func() tea.Msg {
			return &runtime.ElicitationRequestEvent{ElicitationID: "oauth", Meta: map[string]any{"docker-agent/type": "oauth_flow"}}
		},
	}
}

func queuedRuntimeDelivery(m *appModel, id string, event tea.Msg) messages.RoutedMsg {
	return messages.RoutedMsg{SessionID: id, Scope: m.supervisor.GetRunner(id).Scope, Inner: event}
}

func takeAttention(m *appModel) []dialog.OpenDialogMsg {
	return m.dialogMgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true })
}

func TestAttentionVisibilityIsDecidedAtDelivery(t *testing.T) {
	t.Parallel()
	for name, makeEvent := range attentionEvents() {
		for _, initiallyActive := range []bool{false, true} {
			mode := "hidden-to-visible"
			if initiallyActive {
				mode = "visible-to-hidden"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				m := newTabLifecycleModel(t)
				id := m.supervisor.ActiveID()
				origin := m.activeTab
				_, _ = m.handleSpawnSession("/other")
				otherID := m.supervisor.ActiveID()
				if initiallyActive {
					_, _ = m.handleSwitchTab(id)
				}
				event := makeEvent()
				queued := queuedRuntimeDelivery(m, id, event)
				if initiallyActive {
					_, _ = m.handleSwitchTab(otherID)
				} else {
					_, _ = m.handleSwitchTab(id)
				}

				_, cmd := m.Update(queued)
				result := collectMsgs(cmd)
				assert.False(t, hasMsg[dialog.OpenDialogMsg](result), "delivery never emits an unscoped deferred open")
				_, _, attention := origin.state.Snapshot()
				assert.Equal(t, initiallyActive, attention)
				assert.Equal(t, initiallyActive, hasMsg[messages.BellMsg](result), "only hidden delivery rings")
				if initiallyActive {
					assert.False(t, m.dialogMgr.Open(), "a hidden prompt must not open over the incoming tab")
					_, _ = m.handleSwitchTab(id)
				}
				opened := takeAttention(m)
				require.Len(t, opened, 1, "exactly one prompt survives either ordering")
				assert.Same(t, event, opened[0].OriginatingEvent)
				assert.Nil(t, origin.state.Consume())
			})
		}
	}
}

func TestAttentionReplayCannotRaceTabSwitch(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	_, _ = m.handleSpawnSession("/other")
	otherID := m.supervisor.ActiveID()
	var events []tea.Msg
	for _, name := range []string{"first", "second", "third"} {
		event := &runtime.ElicitationRequestEvent{ElicitationID: name, Message: name}
		events = append(events, event)
		_, _ = m.Update(queuedRuntimeDelivery(m, id, event))
	}
	_, replayCmd := m.handleSwitchTab(id)
	require.Same(t, events[2], m.dialogMgr.TopBackgroundEvent(), "replay completes in Update, before any init command runs")
	_, _ = m.handleSwitchTab(otherID)
	other := &runtime.ElicitationRequestEvent{ElicitationID: "other"}
	_, _ = m.Update(queuedRuntimeDelivery(m, otherID, other))
	// Run the old tab's initialization after the switch; it cannot reopen or close prompts.
	for _, msg := range collectMsgs(replayCmd) {
		_, _ = m.Update(msg)
	}
	assert.Same(t, other, m.dialogMgr.TopBackgroundEvent())
	_, _ = m.handleSwitchTab(id)
	opened := takeAttention(m)
	require.Len(t, opened, 3)
	for i, event := range events {
		assert.Same(t, event, opened[i].OriginatingEvent)
	}
}

func TestAttentionRoundTripRetainsTypedInputAndModal(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	first := &runtime.ElicitationRequestEvent{ElicitationID: "first"}
	_, _ = m.Update(queuedRuntimeDelivery(m, id, first))
	firstDialog := m.dialogMgr.TopDialog()
	_, _ = m.Update(tea.PasteMsg{Content: "first draft"})
	second := &runtime.ElicitationRequestEvent{ElicitationID: "second"}
	_, _ = m.Update(queuedRuntimeDelivery(m, id, second))
	secondDialog := m.dialogMgr.TopDialog()
	_, _ = m.Update(tea.PasteMsg{Content: "second draft"})
	modal := &stubDialog{id: "modal"}
	_, _ = m.dialogMgr.Update(dialog.OpenDialogMsg{Model: modal})

	_, _ = m.handleSpawnSession("/other")
	assert.Same(t, modal, m.dialogMgr.TopDialog(), "unrelated modal is not parked")
	_, _ = m.handleSwitchTab(id)
	opened := takeAttention(m)
	require.Len(t, opened, 2)
	assert.Same(t, firstDialog, opened[0].Model)
	assert.Same(t, secondDialog, opened[1].Model)
	assert.Contains(t, opened[0].Model.View(), "first draft")
	assert.Contains(t, opened[1].Model.View(), "second draft")
	assert.Same(t, modal, m.dialogMgr.TopDialog())
}

func TestAttentionRetiredPageRejectsQueuedDelivery(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"close", "clear", "reload-same", "restore", "replace-app"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			id := m.supervisor.ActiveID()
			event := &runtime.ElicitationRequestEvent{ElicitationID: "old"}
			queued := queuedRuntimeDelivery(m, id, event)
			_, _ = m.Update(queued)
			require.True(t, m.dialogMgr.Open())
			switch change {
			case "close":
				_, _ = m.handleCloseTab(id)
			case "clear":
				_, _ = m.handleClearSession()
			case "reload-same":
				_, _ = m.replaceActiveSession(t.Context(), m.application.Session())
			case "restore":
				_, _ = m.replaceActiveSession(t.Context(), session.New(session.WithWorkingDir("/initial")))
			case "replace-app":
				a := app.New(t.Context(), stubRuntime{}, session.New())
				m.supervisor.ReplaceRunnerApp(t.Context(), id, a, "/initial", nil)
				m.initSessionComponents(id, a, a.Session())
			}
			assert.False(t, m.dialogMgr.Open(), "retiring a page also retires its visible prompts")
			_, cmd := m.Update(queued)
			assert.Nil(t, cmd, "old subscription delivery must not affect the replacement page")
			assert.False(t, m.dialogMgr.Open())
		})
	}
}

func TestAttentionRetirementUsesRootBoundary(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		for _, boundary := range []string{"start", "stop", "cancel", "child-stop"} {
			name := "visible/" + boundary
			if hidden {
				name = "hidden/" + boundary
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				m := newTabLifecycleModel(t)
				id, rootID := m.supervisor.ActiveID(), m.application.Session().ID
				foreground := &runtime.ElicitationRequestEvent{ElicitationID: "root", SessionID: rootID}
				detached := &runtime.ElicitationRequestEvent{ElicitationID: "job", SessionID: "job-session"}
				_, _ = m.Update(queuedRuntimeDelivery(m, id, foreground))
				_, _ = m.Update(queuedRuntimeDelivery(m, id, detached))
				if hidden {
					_, _ = m.handleSpawnSession("/other")
				}
				var event tea.Msg
				switch boundary {
				case "start":
					event = runtime.StreamStarted(rootID, "root")
				case "stop":
					event = runtime.StreamStopped(rootID, "root", "normal")
				case "cancel":
					event = messages.StreamCancelledMsg{}
				case "child-stop":
					event = runtime.StreamStopped("child", "child", "normal")
				}
				_, _ = m.Update(queuedRuntimeDelivery(m, id, event))
				if hidden {
					_, _ = m.handleSwitchTab(id)
				}
				opened := takeAttention(m)
				if boundary == "child-stop" {
					require.Len(t, opened, 2)
					assert.Same(t, foreground, opened[0].OriginatingEvent)
				} else {
					require.Len(t, opened, 1)
				}
				assert.Same(t, detached, opened[len(opened)-1].OriginatingEvent)
			})
		}
	}
}

func TestAttentionPageScopeRotatesBeforeReplacementStartup(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	old := queuedRuntimeDelivery(m, id, &runtime.SessionTitleEvent{Title: "old"})
	m.preparePageReplacement(id)
	startup := queuedRuntimeDelivery(m, id, &runtime.SessionTitleEvent{Title: "replacement"})
	m.application.ReplaceSession(t.Context(), session.New(session.WithWorkingDir("/initial")))
	m.initSessionComponents(id, m.application, m.application.Session())
	_, _ = m.Update(startup)
	assert.Equal(t, "replacement", m.activeTab.sessionState.SessionTitle())
	_, cmd := m.Update(old)
	assert.Nil(t, cmd)
	assert.Equal(t, "replacement", m.activeTab.sessionState.SessionTitle())
}

func TestUnloadedTabAppliesAttentionAtDelivery(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id, err := m.supervisor.SpawnSession(t.Context(), "/unloaded")
	require.NoError(t, err)
	event := &runtime.ElicitationRequestEvent{ElicitationID: "pending"}
	_, _ = m.Update(queuedRuntimeDelivery(m, id, event))
	require.Nil(t, m.tabs[id].chatPage)
	_, _, attention := m.tabs[id].state.Snapshot()
	assert.True(t, attention)
	_, _ = m.handleSwitchTab(id)
	assert.Same(t, event, m.dialogMgr.TopBackgroundEvent())
}

func TestCloseActiveTabRemovesOnlyItsAttention(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	firstID := m.supervisor.ActiveID()
	first := &runtime.ElicitationRequestEvent{ElicitationID: "first"}
	_, _ = m.Update(queuedRuntimeDelivery(m, firstID, first))
	_, _ = m.handleSpawnSession("/other")
	otherID := m.supervisor.ActiveID()
	_, _ = m.Update(queuedRuntimeDelivery(m, otherID, &runtime.ElicitationRequestEvent{ElicitationID: "other"}))
	_, cmd := m.handleCloseTab(otherID)
	assert.Equal(t, firstID, m.supervisor.ActiveID())
	assert.Same(t, first, m.dialogMgr.TopBackgroundEvent())
	assert.False(t, hasMsg[dialog.CloseDialogMsg](collectMsgs(cmd)))
	assert.Len(t, takeAttention(m), 1)
}
