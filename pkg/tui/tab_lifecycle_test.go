package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
)

func newTabLifecycleModel(t *testing.T) *appModel {
	t.Helper()
	m := newSpawnTestModel(t, &spySpawner{})
	delete(m.tabs, "test")
	m.initSessionComponents(m.supervisor.ActiveID(), m.application, m.application.Session())
	t.Cleanup(func() {
		m.cleanupAll()
		m.cleanupManagedResources()
	})
	return m
}

func TestSwitchTabPreservesComponentsAndDraft(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	firstID := m.supervisor.ActiveID()
	firstPage, firstEditor, firstState := m.activeTab.chatPage, m.activeTab.editor, m.activeTab.sessionState
	firstEditor.SetValue("unfinished draft")

	_, _ = m.handleSpawnSession("/second")
	secondID := m.supervisor.ActiveID()
	secondPage, secondEditor := m.activeTab.chatPage, m.activeTab.editor
	secondEditor.SetValue("second draft")

	_, _ = m.handleSwitchTab(firstID)
	assert.Same(t, m.tabs[firstID], m.activeTab)
	assert.Same(t, firstPage, m.activeTab.chatPage)
	assert.Same(t, firstEditor, m.activeTab.editor)
	assert.Same(t, firstState, m.activeTab.sessionState)
	assert.Equal(t, "unfinished draft", m.activeTab.editor.Value())

	_, _ = m.handleSwitchTab(secondID)
	assert.Same(t, m.tabs[secondID], m.activeTab)
	assert.Same(t, secondPage, m.activeTab.chatPage)
	assert.Same(t, secondEditor, m.activeTab.editor)
	assert.Equal(t, "second draft", m.activeTab.editor.Value())
}

func TestSwitchTabFailureLeavesDialogAndComponents(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	page, ed, state := m.activeTab.chatPage, m.activeTab.editor, m.activeTab.sessionState
	event := &messages.SendMsg{}
	prompt := &stubDialog{id: "draft"}
	_, _ = m.dialogMgr.Update(dialog.OpenDialogMsg{Model: prompt, OriginatingEvent: event})

	_, cmd := m.handleSwitchTab("missing")

	assert.Equal(t, id, m.supervisor.ActiveID())
	assert.NotContains(t, m.tabs, "missing")
	assert.Same(t, page, m.activeTab.chatPage)
	assert.Same(t, ed, m.activeTab.editor)
	assert.Same(t, state, m.activeTab.sessionState)
	assert.Same(t, prompt, m.dialogMgr.TopDialog())
	assert.Nil(t, m.tabs[id].attentionDialogs)
	assert.Nil(t, m.ensureTab(id).state.Consume())
	assert.True(t, hasMsg[notification.ShowMsg](collectMsgs(cmd)))
}

func TestSwitchTabBuildsComponentsBeforeReplacingActiveUI(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	outgoingApp, outgoingPage, outgoingEditor := m.application, m.activeTab.chatPage, m.activeTab.editor
	calls := 0
	m.buildCommandCategories = func(_ context.Context, model tea.Model) []commands.Category {
		calls++
		assert.Same(t, outgoingApp, core.Resolve[*app.App](model))
		assert.Same(t, outgoingPage, core.Resolve[chat.Page](model))
		assert.Same(t, outgoingEditor, core.Resolve[editor.Editor](model))
		return nil
	}

	_, _ = m.handleSpawnSession("/second")

	assert.Equal(t, 2, calls)
	assert.NotSame(t, outgoingPage, m.activeTab.chatPage)
	assert.NotSame(t, outgoingEditor, m.activeTab.editor)
}

func TestSwitchTabFailedRestoreConsumesPendingState(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	outgoingPage := m.activeTab.chatPage
	sess := session.New()
	application := app.New(t.Context(), storeRuntime{store: session.NewInMemorySessionStore()}, sess)
	id := m.supervisor.AddSession(t.Context(), application, sess, "/second", nil)
	m.ensureTab(id).pendingRestore = new("missing")
	m.ensureTab(id).pendingSidebarCollapsed = new(true)
	m.buildCommandCategories = func(_ context.Context, model tea.Model) []commands.Category {
		assert.Same(t, application, core.Resolve[*app.App](model))
		assert.Same(t, outgoingPage, core.Resolve[chat.Page](model))
		return nil
	}

	_, _ = m.handleSwitchTab(id)

	assert.Nil(t, m.tabs[id].pendingRestore)
	assert.Nil(t, m.tabs[id].pendingSidebarCollapsed)
	assert.Same(t, application, m.application)
	assert.Same(t, m.tabs[id].chatPage, m.activeTab.chatPage)
	assert.True(t, m.activeTab.chatPage.GetSidebarSettings().Collapsed)
	assert.Nil(t, m.applySidebarCollapsed(id))
}

func TestCloseInactiveTabRemovesAllUIState(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	closedID := m.supervisor.ActiveID()
	closedEditor := &mockEditor{}
	m.ensureTab(closedID).editor = closedEditor
	m.ensureTab(closedID).pendingRestore = new("saved")
	m.ensureTab(closedID).pendingSidebarCollapsed = new(true)
	m.ensureTab(closedID).attentionDialogs = map[tea.Msg]dialog.Dialog{&runtime.ElicitationRequestEvent{}: &stubDialog{id: "stashed"}}
	_, _ = m.handleSpawnSession("/second")
	page, ed, state := m.activeTab.chatPage, m.activeTab.editor, m.activeTab.sessionState

	_, _ = m.handleCloseTab(closedID)

	assert.True(t, closedEditor.cleanupCalled)
	assert.NotContains(t, m.tabs, closedID)
	assert.Same(t, page, m.activeTab.chatPage)
	assert.Same(t, ed, m.activeTab.editor)
	assert.Same(t, state, m.activeTab.sessionState)
}

func TestCloseLastTabKeepsUIUntilReplacement(t *testing.T) {
	for _, name := range []string{"spawn", "spawn failure"} {
		t.Run(name, func(t *testing.T) {
			fail := name == "spawn failure"
			t.Parallel()
			m := newTabLifecycleModel(t)
			oldID := m.supervisor.ActiveID()
			oldPage, oldEditor := m.activeTab.chatPage, m.activeTab.editor
			if fail {
				m.supervisor.Shutdown()
				m.supervisor = supervisor.New(func(context.Context, string) (*app.App, *session.Session, func(), error) {
					return nil, nil, nil, errors.New("spawn failed")
				})
				m.supervisor.AddSession(t.Context(), m.application, m.application.Session(), "/initial", nil)
			}

			_, cmd := m.handleCloseTab(oldID)

			assert.NotContains(t, m.tabs, oldID)
			if fail {
				assert.Zero(t, m.supervisor.Count())
				assert.Same(t, oldPage, m.activeTab.chatPage)
				assert.Same(t, oldEditor, m.activeTab.editor)
				assert.True(t, hasMsg[notification.ShowMsg](collectMsgs(cmd)))
			} else {
				assert.Equal(t, 1, m.supervisor.Count())
				assert.NotSame(t, oldPage, m.activeTab.chatPage)
				assert.NotSame(t, oldEditor, m.activeTab.editor)
			}
		})
	}
}

func TestTabPersistedSessionLookup(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	assert.Equal(t, m.application.Session().ID, m.persistedSessionID(id))
	assert.Equal(t, id, m.findTabByPersistedID(m.application.Session().ID))

	for _, savedID := range []string{"saved-session", ""} {
		m.ensureTab(id).pendingRestore = new(savedID)
		assert.Equal(t, savedID, m.persistedSessionID(id))
		assert.Equal(t, id, m.findTabByPersistedID(savedID))
	}
	m.tabs[id].pendingRestore = nil

	loaded := session.New()
	m.application.ReplaceSession(t.Context(), loaded)
	assert.Equal(t, id, m.supervisor.ActiveID(), "loading a conversation must not change the routing key")
	assert.Equal(t, loaded.ID, m.persistedSessionID(id))
	assert.Equal(t, id, m.findTabByPersistedID(loaded.ID))
	assert.Equal(t, "missing", m.persistedSessionID("missing"))
	assert.Empty(t, m.findTabByPersistedID("missing"))
}

func TestRoutedEventDoesNotInitializeUnloadedTab(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id, err := m.supervisor.SpawnSession(t.Context(), "/second")
	require.NoError(t, err)
	m.ensureTab(id).pendingRestore = new("saved")
	m.ensureTab(id).sessionState = service.NewSessionState(session.New())

	_, cmd := m.handleRoutedMsg(messages.RoutedMsg{SessionID: id, Inner: messages.SendMsg{Content: "hidden"}})

	assert.Nil(t, cmd)
	assert.Nil(t, m.tabs[id].chatPage)
	assert.Equal(t, "saved", *m.tabs[id].pendingRestore)
}

func TestSwitchTabRestoresSavedSession(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	store := session.NewInMemorySessionStore()
	saved := session.New(session.WithWorkingDir("/second"))
	saved.AddMessage(session.UserMessage("saved conversation"))
	require.NoError(t, store.AddSession(t.Context(), saved))
	placeholder := session.New(session.WithWorkingDir("/second"))
	application := app.New(t.Context(), storeRuntime{store: store}, placeholder)
	id := m.supervisor.AddSession(t.Context(), application, placeholder, "/second", nil)
	tab := m.ensureTab(id)
	tab.pendingRestore = new(saved.ID)
	tab.pendingSidebarCollapsed = new(true)

	_, _ = m.handleSwitchTab(id)

	assert.Same(t, tab, m.tabs[id])
	assert.Same(t, tab, m.activeTab)
	assert.Equal(t, id, m.supervisor.ActiveID())
	assert.Equal(t, saved.ID, m.application.Session().ID)
	assert.Equal(t, saved.ID, tab.state.SessionID())
	assert.Equal(t, saved.ID, m.persistedSessionID(id))
	assert.Nil(t, tab.pendingRestore)
	assert.Nil(t, tab.pendingSidebarCollapsed)
	assert.Same(t, tab.chatPage, m.activeTab.chatPage)
	assert.Same(t, tab.editor, m.activeTab.editor)
	assert.True(t, m.activeTab.chatPage.GetSidebarSettings().Collapsed)
	assert.Contains(t, m.activeTab.chatPage.View(), "saved conversation")
}

func TestInitRestoresPendingTab(t *testing.T) {
	for _, background := range []bool{false, true} {
		name := "initial tab"
		if background {
			name = "background tab"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			store := session.NewInMemorySessionStore()
			saved := session.New(session.WithWorkingDir("/initial"))
			saved.AddMessage(session.UserMessage("startup conversation"))
			require.NoError(t, store.AddSession(t.Context(), saved))
			id := m.supervisor.ActiveID()
			placeholder := m.application.Session()
			application := app.New(t.Context(), storeRuntime{store: store}, placeholder)
			if background {
				placeholder = session.New()
				application = app.New(t.Context(), storeRuntime{store: store}, placeholder)
				id = m.supervisor.AddSession(t.Context(), application, placeholder, "/initial", nil)
				m.pendingActiveTab = id
			} else {
				m.supervisor.ReplaceRunnerApp(t.Context(), id, application, "/initial", nil)
				m.application = application
			}
			tab := m.ensureTab(id)
			tab.pendingRestore = new(saved.ID)
			tab.pendingSidebarCollapsed = new(true)

			_ = m.init()

			assert.Same(t, tab, m.tabs[id])
			assert.Same(t, tab, m.activeTab)
			assert.Equal(t, id, m.supervisor.ActiveID())
			assert.Empty(t, m.pendingActiveTab)
			assert.Nil(t, tab.pendingRestore)
			assert.Nil(t, tab.pendingSidebarCollapsed)
			assert.Equal(t, saved.ID, m.application.Session().ID)
			assert.Equal(t, saved.ID, tab.state.SessionID())
			assert.Contains(t, m.activeTab.chatPage.View(), "startup conversation")
		})
	}
}

func TestSettingsAndCleanupSkipUninitializedTabs(t *testing.T) {
	setupSettingsConfigTest(t)
	m := newApplySettingsModel(t)
	tab := m.ensureTab("restored")
	tab.pendingRestore = new("saved")

	_, _ = m.handleApplySettings(messages.ApplySettingsMsg{Preferences: defaultTestPreferences()})
	m.cleanupAll()
	m.cleanupManagedResources()

	assert.Same(t, tab, m.tabs["restored"])
	assert.Equal(t, "saved", *tab.pendingRestore)
	assert.Nil(t, tab.chatPage)
	assert.Nil(t, tab.editor)
}

func TestRestorePendingMessagesUsesActiveTabEditor(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	firstID := m.supervisor.ActiveID()
	firstEditor := m.activeTab.editor
	firstEditor.SetValue("first draft")
	_, _ = m.handleSpawnSession("/second")
	secondEditor := m.activeTab.editor

	_, _ = m.Update(messages.RestorePendingMessagesMsg{Content: "second pending message"})

	assert.Equal(t, "second pending message", secondEditor.Value())
	assert.Equal(t, "first draft", firstEditor.Value())

	_, _ = m.handleSwitchTab(firstID)
	_, _ = m.Update(messages.RestorePendingMessagesMsg{Content: "first pending message"})

	assert.Equal(t, "first pending message", firstEditor.Value())
	assert.Equal(t, "second pending message", secondEditor.Value())
}

func TestClearSessionPersistsNewConversation(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
	m := newTabLifecycleModel(t)
	store, err := tuistate.New(t.Context())
	require.NoError(t, err)
	m.tuiStore = store
	tabID := m.supervisor.ActiveID()
	oldID := m.application.Session().ID
	require.NoError(t, store.AddTab(t.Context(), oldID, "/initial"))
	require.NoError(t, store.SetActiveTab(t.Context(), oldID))

	_, _ = m.handleClearSession()

	newID := m.application.Session().ID
	require.NotEqual(t, oldID, newID)
	assert.Equal(t, tabID, m.supervisor.ActiveID(), "clearing preserves the routing identity")
	tabs, activeID, err := store.GetTabs(t.Context())
	require.NoError(t, err)
	require.Len(t, tabs, 1)
	assert.Equal(t, newID, tabs[0].SessionID)
	assert.Equal(t, newID, activeID)
}

func TestTabOwnsRuntimeAttentionState(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	tab := m.tabs[id]
	require.Same(t, m.supervisor.GetRunner(id).State, tab.state)
	tab.state.Apply(&runtime.ElicitationRequestEvent{Message: "prompt"}, false)
	tabs, _ := m.supervisor.GetTabs()
	require.Len(t, tabs, 1)
	assert.True(t, tabs[0].NeedsAttention)

	_, _ = m.handleSpawnSession("/second")
	_, _ = m.handleSwitchTab(id)
	assert.Same(t, tab, m.activeTab)
	assert.Same(t, m.supervisor.GetRunner(id).State, tab.state)
	assert.Nil(t, tab.state.Consume(), "activation drains the owning tab's queue")
	tabs, _ = m.supervisor.GetTabs()
	assert.False(t, tabs[0].NeedsAttention)
}

func TestRestoredTabStreamUsesConversationIdentity(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	tabID := m.supervisor.ActiveID()
	saved := session.New(session.WithWorkingDir("/initial"))
	saved.AddMessage(session.UserMessage("saved conversation"))
	_, _ = m.replaceActiveSession(t.Context(), saved)
	tab := m.activeTab
	require.NotEqual(t, tabID, saved.ID)
	tab.state.Apply(&runtime.StreamStartedEvent{SessionID: saved.ID}, true)
	_, running, _ := tab.state.Snapshot()
	assert.True(t, running, "restored conversation is the root stream, not a child")
	own := &runtime.ElicitationRequestEvent{SessionID: saved.ID}
	detached := &runtime.ElicitationRequestEvent{SessionID: "detached"}
	tab.state.Apply(own, false)
	tab.state.Apply(detached, false)
	tab.state.Apply(&runtime.StreamStoppedEvent{SessionID: saved.ID}, false)
	_, running, attention := tab.state.Snapshot()
	assert.False(t, running)
	assert.True(t, attention)
	assert.Same(t, detached, tab.state.Consume())
	assert.Nil(t, tab.state.Consume())
	assert.Equal(t, tabID, m.supervisor.ActiveID())
}

func TestClearSessionDiscardsPreviousAttention(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	tab := m.activeTab
	oldID := m.application.Session().ID
	event := &runtime.ElicitationRequestEvent{SessionID: "old-detached-job"}
	tab.state.Apply(event, false)
	tab.attentionDialogs = map[tea.Msg]dialog.Dialog{event: &stubDialog{}}

	_, _ = m.handleClearSession()

	assert.Same(t, tab, m.activeTab)
	assert.Nil(t, tab.attentionDialogs)
	assert.Nil(t, tab.state.Consume())
	_, running, attention := tab.state.Snapshot()
	assert.False(t, running)
	assert.False(t, attention)
	newID := m.application.Session().ID
	require.NotEqual(t, oldID, newID)
	tab.state.Apply(&runtime.StreamStartedEvent{SessionID: newID}, true)
	_, running, _ = tab.state.Snapshot()
	assert.True(t, running)
	tab.state.Apply(&runtime.StreamStoppedEvent{SessionID: oldID}, true)
	_, running, _ = tab.state.Snapshot()
	assert.True(t, running, "late stop from previous conversation is not the new root")
	assert.Equal(t, id, m.supervisor.ActiveID())
}
