package tui

import (
	"context"
	"log/slog"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tabstate"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// tabModel owns the UI state keyed by a runtime tab ID, not the session-store ID.
// A restored tab can exist before its editor and chat page are initialized.
type tabModel struct {
	state        *tabstate.State
	chatPage     chat.Page
	editor       editor.Editor
	sessionState *service.SessionState

	// Non-nil until the saved conversation is loaded on first activation.
	pendingRestore          *string
	pendingSidebarCollapsed *bool
	// Keys are runtime event pointers; retain every prompt's unfinished input.
	attentionDialogs map[tea.Msg]dialog.Dialog
}

func (m *appModel) ensureTab(tabID string) *tabModel {
	tab := m.tabs[tabID]
	if tab == nil {
		tab = &tabModel{}
		m.tabs[tabID] = tab
	}
	if tab.state == nil && m.supervisor != nil {
		if runner := m.supervisor.GetRunner(tabID); runner != nil {
			tab.state = runner.State
		}
	}
	return tab
}

// persistActiveTab writes the active tab ID to the tuistate store.
func (m *appModel) persistActiveTab(persistedID string) {
	if m.tuiStore == nil {
		return
	}
	if err := m.tuiStore.SetActiveTab(m.ctx(), persistedID); err != nil {
		slog.Warn("Failed to set active tab", "error", err)
	}
}

// persistFreshTab clears the tab store and writes a single initial tab.
func (m *appModel) persistFreshTab(ctx context.Context, sessionID, workingDir string) {
	if m.tuiStore == nil {
		return
	}
	if err := m.tuiStore.ClearTabs(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to clear tabs", "error", err)
	}
	if err := m.tuiStore.AddTab(ctx, sessionID, workingDir); err != nil {
		slog.WarnContext(ctx, "Failed to persist initial tab", "error", err)
	}
	if err := m.tuiStore.SetActiveTab(ctx, sessionID); err != nil {
		slog.WarnContext(ctx, "Failed to set active tab", "error", err)
	}
}

// restoreTabs restores previously persisted tabs (if enabled) or persists the
// initial session as the sole tab. The tuistate DB always stores persisted
// session-store IDs. Each tab retains its pending persisted ID until first load.
func (m *appModel) restoreTabs(
	ctx context.Context,
	ts *tuistate.Store,
	sv *supervisor.Supervisor,
	spawner SessionSpawner,
	initialApp *app.App,
	initialTabID, initialWorkingDir string,
) {
	if ts == nil {
		return
	}

	var savedTabs []tuistate.TabEntry
	var savedActiveID string
	if userconfig.Get().GetRestoreTabs() {
		savedTabs, savedActiveID, _ = ts.GetTabs(ctx)
	}

	if len(savedTabs) == 0 {
		m.persistFreshTab(ctx, initialTabID, initialWorkingDir)
		return
	}

	sessionStore := initialApp.SessionStore()
	restoredFirst := false

	for _, saved := range savedTabs {
		// Validate the saved session still exists.
		if sessionStore != nil && saved.SessionID != "" {
			if _, err := sessionStore.GetSession(ctx, saved.SessionID); err != nil {
				slog.WarnContext(ctx, "Saved session no longer exists, removing stale tab",
					"session_id", saved.SessionID, "error", err)
				_ = ts.RemoveTab(ctx, saved.SessionID)
				continue
			}
		}

		// Determine the runtime tab ID to use.
		var runtimeID string
		if !restoredFirst {
			restoredFirst = true
			runtimeID = initialTabID
		} else {
			a, newSess, spawnCleanup, err := spawner(ctx, saved.WorkingDir)
			if err != nil {
				slog.WarnContext(ctx, "Failed to restore tab", "working_dir", saved.WorkingDir, "error", err)
				_ = ts.RemoveTab(ctx, saved.SessionID)
				continue
			}
			runtimeID = sv.AddSession(ctx, a, newSess, saved.WorkingDir, spawnCleanup)
		}

		// Stash persisted session ID for lazy loading on first switch.
		tab := m.ensureTab(runtimeID)
		tab.pendingRestore = new(saved.SessionID)
		if saved.SidebarCollapsed {
			tab.pendingSidebarCollapsed = new(true)
		}

		// If this was the active tab, queue a switch on Init().
		if saved.SessionID == savedActiveID {
			if restoredFirst && runtimeID == initialTabID {
				_ = ts.SetActiveTab(ctx, saved.SessionID)
			} else {
				m.pendingActiveTab = runtimeID
			}
		}

		// Peek at the session title so the tab bar shows a name before lazy load.
		if sessionStore != nil && saved.SessionID != "" {
			if oldSess, err := sessionStore.GetSession(ctx, saved.SessionID); err == nil && oldSess.Title != "" {
				sv.SetRunnerTitle(runtimeID, oldSess.Title)
			}
		}
	}

	// If all saved tabs were stale, persist the initial session.
	if !restoredFirst {
		m.persistFreshTab(ctx, initialTabID, initialWorkingDir)
	}
}

// persistedSessionID returns the session-store ID that should be used for
// tuistate persistence for the given runtime tab ID.
//
// If the tab has a pending restore (session not yet lazily loaded), the
// pending persisted ID is returned — this is the original
// session-store ID that was saved across restarts. Otherwise the live
// session ID from the app is used.
func (m *appModel) persistedSessionID(tabID string) string {
	if tab := m.tabs[tabID]; tab != nil && tab.pendingRestore != nil {
		return *tab.pendingRestore
	}
	if runner := m.supervisor.GetRunner(tabID); runner != nil {
		return runner.App.Session().ID
	}
	return tabID
}

// findTabByPersistedID scans all open tabs and returns the runtime tab ID
// whose persisted session-store ID matches the given ID. Returns "" if not found.
func (m *appModel) findTabByPersistedID(persistedID string) string {
	// Check pending restores first (tabs not yet lazily loaded).
	for tabID, tab := range m.tabs {
		if tab.pendingRestore != nil && *tab.pendingRestore == persistedID {
			return tabID
		}
	}
	// Check live sessions.
	tabs, _ := m.supervisor.GetTabs()
	for _, tab := range tabs {
		if runner := m.supervisor.GetRunner(tab.SessionID); runner != nil {
			if runner.App.Session().ID == persistedID {
				return tab.SessionID
			}
		}
	}
	return ""
}

// bindTabSession keeps subscription status aligned with the conversation loaded in the tab.
func (m *appModel) bindTabSession(tabID, sessionID string) {
	tab := m.ensureTab(tabID)
	if tab.state != nil && tab.state.SessionID() != sessionID {
		tab.state.ReplaceSession(sessionID)
		tab.attentionDialogs = nil
	}
}
