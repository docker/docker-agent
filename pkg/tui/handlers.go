package tui

import (
	"errors"
	"fmt"
	"log/slog"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/evaluation"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// --- Session management ---

func (m *appModel) handleBranchFromEdit(msg messages.BranchFromEditMsg) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}
	if msg.ParentSessionID == "" {
		return m, notification.ErrorCmd("No parent session for branch")
	}
	ctx := m.ctx()

	parent, err := store.GetSession(ctx, msg.ParentSessionID)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load parent session: %v", err))
	}

	newSess, err := session.BranchSession(parent, msg.BranchAtPosition)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to branch session: %v", err))
	}

	// Apply live-session state before the store write so mid-session
	// changes the store copy lacks (e.g. a mode downgrade) are persisted
	// with the branch, not just patched in memory.
	if current := m.application.Session(); current != nil {
		newSess.HideToolResults = current.HideToolResults
		newSess.SetSafetyPolicy(current.GetSafetyPolicy())
		// SetSafetyPolicy clears the toggle memory; restore the live one
		// so a branch taken while escalated keeps its toggle-back
		// destination. newSess is not yet shared — direct write is safe.
		newSess.PriorSafetyPolicy = current.GetPriorSafetyPolicy()
	}

	if err := store.AddSession(ctx, newSess); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to save branched session: %v", err))
	}

	// Preserve sidebar settings across branch
	sidebarSettings := m.chatPage.GetSidebarSettings()

	activeID := m.supervisor.ActiveID()

	// Update tuistate so the tab points to the branched session on re-launch.
	if m.tuiStore != nil {
		oldPersistedID := m.persistedSessionID(activeID)
		if err := m.tuiStore.UpdateTabSessionID(ctx, oldPersistedID, newSess.ID); err != nil {
			slog.WarnContext(ctx, "Failed to update tab session ID after branch", "error", err)
		}
	}
	m.persistActiveTab(newSess.ID)

	// Replace the session in the app and rebuild all per-session components.
	m.application.ReplaceSession(ctx, newSess)
	m.initSessionComponents(activeID, m.application, newSess)
	m.dialogMgr = dialog.New()

	// Restore sidebar settings
	m.chatPage.SetSidebarSettings(sidebarSettings)

	m.reapplyKeyboardEnhancements()

	return m, tea.Sequence(
		m.chatPage.Init(),
		m.resizeAll(),
		m.editor.Focus(),
		core.CmdHandler(messages.SendMsg{
			Content:     msg.Content,
			Attachments: msg.Attachments,
		}),
	)
}

func (m *appModel) handleForkSession() (tea.Model, tea.Cmd) {
	currentSession := m.application.Session()
	if currentSession == nil {
		return m, notification.ErrorCmd("No active session to fork")
	}

	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}

	spawner := m.supervisor.Spawner()
	if spawner == nil {
		return m, notification.ErrorCmd("Session spawning not available")
	}
	ctx := m.ctx()

	// Fork the session and clone all messages.
	forkedSession, err := session.BranchSession(currentSession, len(currentSession.Messages))
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to fork session: %v", err))
	}

	if err := store.AddSession(ctx, forkedSession); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to save forked session: %v", err))
	}

	a, _, cleanup, err := spawner(ctx, forkedSession.WorkingDir)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to create runtime for fork: %v", err))
	}

	a.ReplaceSession(ctx, forkedSession)
	m.supervisor.AddSession(ctx, a, forkedSession, forkedSession.WorkingDir, cleanup)

	if m.tuiStore != nil {
		if err := m.tuiStore.AddTab(ctx, forkedSession.ID, forkedSession.WorkingDir); err != nil {
			slog.WarnContext(ctx, "Failed to persist forked tab", "error", err)
		}
	}

	return m.handleSwitchTab(forkedSession.ID)
}

func (m *appModel) handleToggleSessionStar(sessionID string) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}

	currentSess := m.application.Session()
	if currentSess != nil && currentSess.ID == sessionID {
		currentSess.Starred = !currentSess.Starred
		m.chatPage.SetSessionStarred(currentSess.Starred)
		if err := store.UpdateSession(m.ctx(), currentSess); err != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to save session: %v", err))
		}
	} else {
		sess, err := store.GetSession(m.ctx(), sessionID)
		if err != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to load session: %v", err))
		}
		if err := store.SetSessionStarred(m.ctx(), sessionID, !sess.Starred); err != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to update session: %v", err))
		}
	}
	return m, nil
}

func (m *appModel) handleSetSessionTitle(title string) (tea.Model, tea.Cmd) {
	if err := m.application.UpdateSessionTitle(m.ctx(), title); err != nil {
		if errors.Is(err, app.ErrTitleGenerating) {
			return m, notification.WarningCmd("Title is being generated, please wait")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to set session title: %v", err))
	}
	return m, notification.SuccessCmd("Title set to: " + title)
}

func (m *appModel) handleRegenerateTitle() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	if sess == nil {
		return m, notification.ErrorCmd("No active session")
	}
	if len(sess.GetLastUserMessages(1)) == 0 {
		return m, notification.ErrorCmd("Cannot regenerate title: no user message in session")
	}
	if err := m.application.RegenerateSessionTitle(m.ctx()); err != nil {
		if errors.Is(err, app.ErrTitleGenerating) {
			return m, notification.WarningCmd("Title is being generated, please wait")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to regenerate title: %v", err))
	}
	spinnerCmd := m.chatPage.SetTitleRegenerating(true)
	return m, tea.Batch(spinnerCmd, notification.SuccessCmd("Regenerating title..."))
}

func (m *appModel) handleDeleteSession(sessionID string) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}
	if err := store.DeleteSession(m.ctx(), sessionID); err != nil {
		return m, notification.ErrorCmd("Failed to delete session: " + err.Error())
	}

	return m, notification.SuccessCmd("Session deleted.")
}

// --- Eval / Export / Compact / Copy ---

func (m *appModel) handleEvalSession(filename string) (tea.Model, tea.Cmd) {
	evalFile, _ := evaluation.Save(m.application.Session(), filename)
	return m, notification.SuccessCmd("Eval saved to file " + evalFile)
}

func (m *appModel) handleExportSession(filename string) (tea.Model, tea.Cmd) {
	exportFile, err := m.application.ExportHTML(m.ctx(), filename)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to export session: %v", err))
	}
	return m, notification.SuccessCmd("Session exported to " + exportFile)
}

func (m *appModel) handleCompactSession(msg messages.CompactSessionMsg) (tea.Model, tea.Cmd) {
	if compactTargetsCurrentSession(msg, m.application.Session()) {
		return m, m.chatPage.CompactSession(msg.AdditionalPrompt)
	}
	// Targeted compaction of a live sub-agent session: queued onto the
	// target session's own run loop, so neither the root stream nor the
	// target stream is cancelled.
	if err := m.application.CompactLiveSession(m.ctx(), msg.SessionID, msg.AdditionalPrompt); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Compaction request failed: %v", err))
	}
	return m, notification.InfoCmd(fmt.Sprintf(
		"Compaction requested for %s; it runs at the session's next safe point.",
		compactTargetLabel(msg),
	))
}

// compactTargetsCurrentSession reports whether msg addresses the current
// root session: an empty target (the /compact command) or the root's own
// session ID (the main row of the /context team view). Both route through
// the existing root compaction path.
func compactTargetsCurrentSession(msg messages.CompactSessionMsg, current *session.Session) bool {
	return msg.SessionID == "" || (current != nil && msg.SessionID == current.ID)
}

// compactTargetLabel names the target of a targeted compaction request for
// notifications: "agent (session 0f9e8d7c)", or just the short session ID
// when the agent name is unknown.
func compactTargetLabel(msg messages.CompactSessionMsg) string {
	shortID := msg.SessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	if msg.AgentName == "" {
		return "session " + shortID
	}
	return fmt.Sprintf("%s (session %s)", msg.AgentName, shortID)
}

func (m *appModel) handleCopySessionToClipboard() (tea.Model, tea.Cmd) {
	transcript := m.application.PlainTextTranscript()
	if transcript == "" {
		return m, notification.SuccessCmd("Conversation is empty; nothing copied.")
	}
	return m, copyToClipboard(transcript, "Conversation copied to clipboard.")
}

func (m *appModel) handleCopyLastResponseToClipboard() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	if sess == nil {
		return m, notification.InfoCmd("No active session.")
	}
	lastResponse := sess.GetLastAssistantMessageContent()
	if lastResponse == "" {
		return m, notification.InfoCmd("No assistant response to copy.")
	}
	return m, copyToClipboard(lastResponse, "Last response copied to clipboard.")
}

func (m *appModel) handleUndoSnapshot() (tea.Model, tea.Cmd) {
	if m.chatPage.IsWorking() {
		return m, notification.WarningCmd("Wait for the current response to finish before undoing")
	}
	result, err := m.application.UndoLastSnapshot(m.ctx())
	if err != nil {
		if errors.Is(err, app.ErrNothingToUndo) {
			return m, notification.InfoCmd("No snapshot to undo")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to undo snapshot: %v", err))
	}

	text := fmt.Sprintf("Restored %d file%s from the last snapshot", result.RestoredFiles, plural(result.RestoredFiles))
	return m, notification.SuccessCmd(text)
}

func (m *appModel) handleShowSnapshotsDialog() (tea.Model, tea.Cmd) {
	snapshots := m.application.ListSnapshots()
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSnapshotsDialog(snapshots),
	})
}

func (m *appModel) handleResetSnapshot(keep int) (tea.Model, tea.Cmd) {
	if m.chatPage.IsWorking() {
		return m, notification.WarningCmd("Wait for the current response to finish before resetting")
	}
	result, err := m.application.ResetSnapshot(m.ctx(), keep)
	if err != nil {
		if errors.Is(err, app.ErrNothingToUndo) {
			return m, notification.InfoCmd("Nothing to reset")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to reset snapshot: %v", err))
	}

	target := "the original state"
	if keep > 0 {
		target = fmt.Sprintf("snapshot %d", keep)
	}
	text := fmt.Sprintf("Restored %d file%s to %s", result.RestoredFiles, plural(result.RestoredFiles), target)
	return m, notification.SuccessCmd(text)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// copyToClipboard returns a sequenced command that copies text to the system
// clipboard using both the OSC 52 escape sequence (for SSH/tmux compatibility)
// and the platform-native clipboard API, then shows a success notification.
func copyToClipboard(text, successMsg string) tea.Cmd {
	return tea.Sequence(
		tea.SetClipboard(text),
		func() tea.Msg {
			_ = clipboard.WriteAll(text)
			return nil
		},
		notification.SuccessCmd(successMsg),
	)
}

// --- Agent management ---

func (m *appModel) handleSwitchAgent(agentName string) (tea.Model, tea.Cmd) {
	if agentName == m.sessionState.CurrentAgentName() {
		return m, nil
	}

	if err := m.application.SwitchAgent(agentName); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to switch to agent '%s': %v", agentName, err))
	}
	m.sessionState.SetCurrentAgentName(agentName)
	cmd := m.updateChatCmd(messages.SessionToggleChangedMsg{})
	return m, cmd
}

// handleShowAgentDetails opens the read-only agent-details dialog for the named
// agent, looking it up in the available-agents roster. The agent's latest
// token-usage snapshot (if it has run) rides along so the dialog can show its
// context usage, as does its cumulative attributed cost (if any).
func (m *appModel) handleShowAgentDetails(agentName string) (tea.Model, tea.Cmd) {
	for _, agent := range m.sessionState.AvailableAgents() {
		if agent.Name != agentName {
			continue
		}
		cfg := m.application.AgentConfigInfo(m.ctx(), agentName)
		var usage *runtime.Usage
		if u, ok := m.sessionState.AgentUsage(agentName); ok {
			usage = &u
		}
		var cost *float64
		if c, ok := m.sessionState.AgentCost(agentName); ok {
			cost = &c
		}
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewAgentDetailsDialog(agent, cfg, usage, cost),
		})
	}
	return m, nil
}

func (m *appModel) handleCycleAgent() (tea.Model, tea.Cmd) {
	availableAgents := m.sessionState.AvailableAgents()
	if len(availableAgents) <= 1 {
		return m, notification.InfoCmd("No other agents available")
	}
	currentIndex := -1
	for i, agent := range availableAgents {
		if agent.Name == m.sessionState.CurrentAgentName() {
			currentIndex = i
			break
		}
	}
	nextIndex := (currentIndex + 1) % len(availableAgents)
	return m.handleSwitchToAgentByIndex(nextIndex)
}

func (m *appModel) handleSwitchToAgentByIndex(index int) (tea.Model, tea.Cmd) {
	availableAgents := m.sessionState.AvailableAgents()
	if index >= 0 && index < len(availableAgents) {
		agentName := availableAgents[index].Name
		if agentName != m.sessionState.CurrentAgentName() {
			return m, core.CmdHandler(messages.SwitchAgentMsg{AgentName: agentName})
		}
	}
	return m, nil
}

// --- Toggles ---

// handleToggleYolo goes through the safety mode, not the raw ToolsApproved
// flag, so toggle-off genuinely revokes an autonomous mode. Same contract
// as the server's ToggleToolApproval.
func (m *appModel) handleToggleYolo() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	sess.ToggleYolo()
	m.sessionState.SetYoloMode(sess.IsToolsApproved())
	return m.forwardChat(messages.SessionToggleChangedMsg{})
}

// handleTogglePause toggles whether the runtime loop is paused at iteration
// boundaries. The pause kicks in once the in-flight LLM request and its tool
// calls finish; running /pause again resumes the loop.
//
// The TUI reflects this in the resize-handle indicator: requesting a pause
// while the agent is working shows "Pausing…" until the runtime emits a
// RuntimePausedEvent at the next iteration boundary (flipping it to
// "Paused"); requesting a pause while idle shows "Paused" immediately.
func (m *appModel) handleTogglePause() (tea.Model, tea.Cmd) {
	paused, supported := m.application.TogglePause()
	switch {
	case !supported:
		return m, notification.InfoCmd("Pause is not supported with remote runtimes")
	case paused:
		if m.chatPage.IsWorking() {
			m.sessionState.SetPauseState(service.PausePausing)
			return m, notification.InfoCmd("Pausing after the current request — /pause again to resume")
		}
		m.sessionState.SetPauseState(service.PausePaused)
		return m, notification.InfoCmd("Runtime paused — /pause again to resume")
	default:
		m.sessionState.SetPauseState(service.PauseNone)
		return m, notification.SuccessCmd("Runtime resumed")
	}
}

func (m *appModel) handleToggleHideToolResults() (tea.Model, tea.Cmd) {
	return m.forwardChat(messages.ToggleHideToolResultsMsg{})
}

func (m *appModel) handleToggleSplitDiff() (tea.Model, tea.Cmd) {
	m.sessionState.ToggleSplitDiffView()
	enabled := m.sessionState.SplitDiffView()

	// Persist to global userconfig
	go persistSplitDiffView(enabled)

	return m, tea.Batch(
		m.updateChatCmd(editfile.ToggleDiffViewMsg{}),
		m.updateChatCmd(messages.SessionToggleChangedMsg{}),
	)
}

// persistSplitDiffView writes the current split-diff toggle to the user
// config without blocking the UI. Errors are logged but otherwise ignored
// because losing the persistence is non-fatal.
func persistSplitDiffView(enabled bool) {
	err := userconfig.Update(func(cfg *userconfig.Config) error {
		if cfg.Settings == nil {
			cfg.Settings = &userconfig.Settings{}
		}
		cfg.Settings.SplitDiffView = &enabled
		return nil
	})
	if err != nil {
		slog.Warn("Failed to persist split diff setting to userconfig", "error", err)
	}
}

// --- Dialogs ---

func (m *appModel) handleShowCostDialog() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewCostDialog(sess),
	})
}

// handleShowContextDialog computes the context breakdown in a tea.Cmd
// goroutine: the computation lists the agent's tools, which may start
// not-yet-started toolsets (e.g. MCP servers) and block for a while, so it
// must not run inside the Update loop. The dialog opens when the data is
// ready, together with the live-session team view (current root plus every
// running sub-agent session; empty for runtimes without live tracking).
func (m *appModel) handleShowContextDialog() (tea.Model, tea.Cmd) {
	appRef := m.application
	ctx := m.ctx()
	return m, func() tea.Msg {
		breakdown, err := appRef.ContextBreakdown(ctx)
		switch {
		case errors.Is(err, runtime.ErrUnsupported):
			return notification.ShowMsg{
				Text: "Context breakdown is not supported with remote runtimes",
				Type: notification.TypeInfo,
			}
		case err != nil:
			return notification.ShowMsg{
				Text: fmt.Sprintf("Failed to compute context breakdown: %v", err),
				Type: notification.TypeError,
			}
		}
		return dialog.OpenDialogMsg{Model: dialog.NewContextDialog(breakdown, appRef.LiveSessions(ctx)...)}
	}
}

// handleDropAttachedFile removes an attached file from the current session
// so it stops being shared with sub-agents and skill prompts.
func (m *appModel) handleDropAttachedFile(path string) (tea.Model, tea.Cmd) {
	dropped, err := m.application.DropAttachedFile(m.ctx(), path)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to drop attachment: %v", err))
	}
	return m, notification.SuccessCmd(fmt.Sprintf("Dropped %s from the session context.", filepath.Base(dropped)))
}

func (m *appModel) handleShowPermissionsDialog() (tea.Model, tea.Cmd) {
	perms := m.application.PermissionsInfo()
	sess := m.application.Session()
	yoloEnabled := sess != nil && sess.IsToolsApproved()
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewPermissionsDialog(perms, yoloEnabled),
	})
}

func (m *appModel) handleShowToolsDialog() (tea.Model, tea.Cmd) {
	agentTools, err := m.application.CurrentAgentTools(m.ctx())
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load tools: %v", err))
	}
	// Read toolset statuses *after* CurrentAgentTools so the snapshot
	// reflects the same Started state the user just observed (Tools()
	// drives lazy startup of any not-yet-started toolset).
	statuses := m.application.CurrentAgentToolsetStatuses()
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewToolsDialog(statuses, agentTools),
	})
}

func (m *appModel) handleShowSkillsDialog() (tea.Model, tea.Cmd) {
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSkillsDialog(m.application.CurrentAgentSkills()),
	})
}

// handleRestartToolset asks the runtime to restart the named toolset.
// The actual call can block for up to ~35s (the supervisor's
// reconnect timeout), so we run it inside a tea.Cmd goroutine and
// surface the result via a notification toast on completion.
func (m *appModel) handleRestartToolset(name string) (tea.Model, tea.Cmd) {
	if name == "" {
		return m, notification.ErrorCmd("usage: /toolset-restart <name>")
	}
	appRef := m.application
	return m, tea.Batch(
		notification.InfoCmd(fmt.Sprintf("Restarting toolset %q…", name)),
		func() tea.Msg {
			if err := appRef.RestartToolset(m.ctx(), name); err != nil {
				return notification.ShowMsg{
					Text: fmt.Sprintf("Failed to restart %q: %v", name, err),
					Type: notification.TypeError,
				}
			}
			return notification.ShowMsg{
				Text: fmt.Sprintf("Toolset %q restarted", name),
				Type: notification.TypeSuccess,
			}
		},
	)
}

// --- MCP prompts ---

func (m *appModel) handleShowMCPPromptInput(promptName string, promptInfo any) (tea.Model, tea.Cmd) {
	info, ok := promptInfo.(mcptools.PromptInfo)
	if !ok {
		return m, notification.ErrorCmd("Invalid prompt info")
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewMCPPromptInputDialog(promptName, info),
	})
}

func (m *appModel) handleMCPPrompt(promptName string, arguments map[string]string) (tea.Model, tea.Cmd) {
	promptContent, err := m.application.ExecuteMCPPrompt(m.ctx(), promptName, arguments)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Error executing MCP prompt '%s': %v", promptName, err))
	}
	return m, core.CmdHandler(messages.SendMsg{Content: promptContent})
}

// --- Model picker ---

func (m *appModel) handleOpenModelPicker() (tea.Model, tea.Cmd) {
	start := time.Now()
	defer func() {
		slog.Debug("TUI model picker open handled", "duration", time.Since(start))
	}()
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Model switching is not supported with remote runtimes")
	}
	loadStart := time.Now()
	models := m.application.AvailableModels(m.ctx())
	slog.Debug("TUI model picker available models loaded", "duration", time.Since(loadStart), "models", len(models))
	if len(models) == 0 {
		return m, notification.InfoCmd("No models available for selection")
	}
	dialogStart := time.Now()
	modelDialog := dialog.NewModelPickerDialog(models)
	slog.Debug("TUI model picker dialog built", "duration", time.Since(dialogStart), "models", len(models))
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: modelDialog,
	})
}

func (m *appModel) handleRefreshModelPicker(query string) (tea.Model, tea.Cmd) {
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Model switching is not supported with remote runtimes")
	}

	ctx := m.ctx()
	return m, tea.Batch(
		notification.InfoCmd("Refreshing models…"),
		func() tea.Msg {
			err := m.application.RefreshModelsCatalog(ctx)
			catalogRefreshed := err == nil
			if errors.Is(err, runtime.ErrUnsupported) {
				err = nil
			}
			if err != nil {
				return messages.ModelPickerRefreshedMsg{Query: query, Err: err}
			}
			return messages.ModelPickerRefreshedMsg{
				Models:           m.application.AvailableModels(ctx),
				Query:            query,
				CatalogRefreshed: catalogRefreshed,
			}
		},
	)
}

func (m *appModel) handleModelPickerRefreshed(msg messages.ModelPickerRefreshedMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to refresh models catalog: %v", msg.Err))
	}
	if len(msg.Models) == 0 {
		return m, notification.InfoCmd("No models available for selection")
	}

	modelDialog := dialog.NewModelPickerDialogWithQuery(msg.Models, msg.Query)
	toast := "Model list reloaded"
	if msg.CatalogRefreshed {
		toast = "Models refreshed"
	}
	return m, tea.Batch(
		notification.SuccessCmd(toast),
		core.CmdHandler(dialog.OpenDialogMsg{Model: modelDialog}),
	)
}

// handleCycleThinkingLevel advances the current agent's thinking-effort level
// (shift+tab). On success the new level is reflected in the sidebar via the
// re-emitted agent info; only failures surface a notification.
func (m *appModel) handleCycleThinkingLevel() (tea.Model, tea.Cmd) {
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Thinking levels can't be changed with remote runtimes")
	}
	if _, err := m.application.CycleAgentThinkingLevel(m.ctx()); err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Current model does not support thinking levels")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to change thinking level: %v", err))
	}
	return m, nil
}

// handleSetThinkingLevel applies the /effort command: it sets the current
// model's reasoning-effort level to the requested value. An empty level
// opens the effort picker dialog; unsupported levels surface the model's
// supported list via the runtime error.
func (m *appModel) handleSetThinkingLevel(level string) (tea.Model, tea.Cmd) {
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Thinking levels can't be changed with remote runtimes")
	}
	if level == "" {
		return m.openEffortPicker()
	}
	parsed, ok := effort.Parse(level)
	if !ok {
		return m, notification.ErrorCmd(fmt.Sprintf("Unknown effort level %q (valid: none, minimal, low, medium, high, xhigh, max)", level))
	}
	applied, err := m.application.SetAgentThinkingLevel(m.ctx(), parsed)
	if err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Current model does not support thinking levels")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to set thinking level: %v", err))
	}
	return m, notification.SuccessCmd("Reasoning effort set to " + applied.String())
}

// openEffortPicker opens the effort picker dialog listing the thinking-effort
// levels supported by the current agent's model (/effort without arguments).
// The sidebar's thinking label doubles as the support signal: it is empty
// exactly when the runtime reports no selectable thinking configuration.
func (m *appModel) openEffortPicker() (tea.Model, tea.Cmd) {
	agent := m.sessionState.GetCurrentAgent()
	if agent.Thinking == "" {
		return m, notification.InfoCmd("Current model does not support thinking levels")
	}
	levels := modelinfo.SupportedThinkingLevels(agent.Provider, agent.Model)
	// "off" maps onto none; adaptive/token labels leave no level marked current.
	current, ok := effort.Parse(agent.Thinking)
	if !ok && agent.Thinking == "off" {
		current = effort.None
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewEffortPickerDialog(levels, current),
	})
}

func (m *appModel) handleChangeModel(modelRef string) (tea.Model, tea.Cmd) {
	if err := m.application.SetCurrentAgentModel(m.ctx(), modelRef); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to change model: %v", err))
	}
	if modelRef == "" {
		return m, notification.SuccessCmd("Model reset to default")
	}
	return m, notification.SuccessCmd("Model changed to " + modelRef)
}

// --- Theme picker ---

func (m *appModel) handleOpenThemePicker() (tea.Model, tea.Cmd) {
	themeRefs, err := styles.ListThemeRefs()
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to list themes: %v", err))
	}
	currentTheme := styles.CurrentTheme()
	currentRef := currentTheme.Ref
	autoEnabled := styles.AutoThemeEnabled()

	// The auto entry resolves to the configured light/dark pair at apply
	// time. While it is active, the resolved concrete theme is not marked
	// current so only one entry carries the badge.
	choices := []dialog.ThemeChoice{{
		Ref:       styles.AutoThemeRef,
		Name:      styles.AutoThemeDisplayName,
		IsCurrent: autoEnabled,
		IsBuiltin: true,
	}}
	for _, ref := range themeRefs {
		theme, loadErr := styles.LoadTheme(ref)
		if loadErr != nil {
			continue
		}
		name := theme.Name
		if name == "" {
			name = strings.TrimPrefix(ref, styles.UserThemePrefix)
		}
		choices = append(choices, dialog.ThemeChoice{
			Ref:       ref,
			Name:      name,
			IsCurrent: !autoEnabled && ref == currentRef,
			IsDefault: ref == styles.DefaultThemeRef,
			IsBuiltin: styles.IsBuiltinTheme(ref),
		})
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewThemePickerDialog(choices, currentRef),
	})
}

func (m *appModel) handleChangeTheme(themeRef string) (tea.Model, tea.Cmd) {
	selectingAuto := themeRef == styles.AutoThemeRef
	if styles.GetPersistedThemeRef() == themeRef && styles.AutoThemeEnabled() == selectingAuto {
		return m, nil
	}
	theme, err := styles.LoadTheme(styles.ResolveThemeRef(themeRef))
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load theme: %v", err))
	}
	wasAuto := styles.AutoThemeEnabled()
	styles.SetAutoThemeEnabled(selectingAuto)
	styles.ApplyTheme(theme)
	m.invalidateCachesForThemeChange()

	if err := styles.SaveThemeToUserConfig(themeRef); err != nil {
		slog.Warn("Failed to save theme to user config", "theme", themeRef, "error", err)
	}

	displayName := theme.Name
	if selectingAuto {
		displayName = styles.AutoThemeDisplayName
	}
	cmds := []tea.Cmd{
		notification.SuccessCmd("Theme changed to " + displayName),
		core.CmdHandler(messages.ThemeChangedMsg{}),
	}
	// Keep terminal color-scheme reporting (DEC mode 2031) in sync with the
	// auto selection: enable it and re-query the polarity when auto is picked
	// mid-session, reset it when a concrete theme replaces auto.
	switch {
	case selectingAuto && !m.lightDarkModeSet:
		m.lightDarkModeSet = true
		cmds = append(cmds, tea.Raw(ansi.SetModeLightDark), tea.RequestBackgroundColor)
	case !selectingAuto && wasAuto && m.lightDarkModeSet:
		m.lightDarkModeSet = false
		cmds = append(cmds, tea.Raw(ansi.ResetModeLightDark))
	}
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleThemePreview(themeRef string) (tea.Model, tea.Cmd) {
	themeRef = styles.ResolveThemeRef(themeRef)
	if current := styles.CurrentTheme(); current != nil && current.Ref == themeRef {
		return m, nil
	}
	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		return m, nil
	}
	styles.ApplyTheme(theme)
	return m.applyThemeChanged()
}

func (m *appModel) handleThemeCancelPreview(originalRef string) (tea.Model, tea.Cmd) {
	if current := styles.CurrentTheme(); current != nil && current.Ref == originalRef {
		return m, nil
	}
	styles.ApplyThemeRef(originalRef)
	return m.applyThemeChanged()
}

func (m *appModel) invalidateCachesForThemeChange() {
	// markdown's style cache resets itself via styles.OnThemeChange.
	m.statusBar.InvalidateCache()
}

func (m *appModel) applyThemeChanged() (tea.Model, tea.Cmd) {
	m.invalidateCachesForThemeChange()
	// Re-target the file watcher: theme changes (picker selection, preview,
	// hot reload) can move the active theme to a different backing file.
	m.watchCurrentTheme()
	return m, tea.Batch(
		m.updateDialogCmd(messages.ThemeChangedMsg{}),
		m.updateChatCmd(messages.ThemeChangedMsg{}),
	)
}

// handleThemeFileChanged hot-reloads a theme that was modified on disk.
func (m *appModel) handleThemeFileChanged(themeRef string) (tea.Model, tea.Cmd) {
	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to hot-reload theme: %v", err))
	}
	styles.ApplyTheme(theme)
	return m, tea.Batch(
		notification.SuccessCmd("Theme hot-reloaded"),
		core.CmdHandler(messages.ThemeChangedMsg{}),
	)
}

// --- Settings (/settings) ---

// handleOpenSettingsDialog opens the /settings dialog. The Visuals tab is
// omitted when there is no sidebar to customize (--sidebar=false); lean mode
// never gets here (it has no overlay support, the message is dropped).
func (m *appModel) handleOpenSettingsDialog() (tea.Model, tea.Cmd) {
	settings := userconfig.Get()
	preferences := messages.Preferences{
		Layout:                m.layoutSettings,
		SendMode:              m.sendMode,
		SplitDiffView:         settings.GetSplitDiffView(),
		ExpandThinking:        settings.GetExpandThinking(),
		HideToolResults:       settings.HideToolResults,
		RenderImages:          settings.GetRenderImages(),
		ShowBanner:            settings.GetShowBanner(),
		YOLO:                  settings.YOLO,
		RestoreTabs:           settings.GetRestoreTabs(),
		Snapshot:              settings.SnapshotsEnabled(),
		CacheStablePrompts:    settings.CacheStablePromptsEnabled(),
		WarnOnCacheMiss:       settings.CacheMissWarningsEnabled(),
		Lean:                  settings.Lean,
		TabTitleMaxLength:     settings.GetTabTitleMaxLength(),
		Sound:                 settings.GetSound(),
		SoundThreshold:        settings.GetSoundThreshold(),
		InterruptConfirmation: messages.ParseInterruptMode(settings.GetInterruptConfirmation()),
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSettingsDialog(preferences, !m.hideSidebar),
	})
}

// handleApplySettings applies the settings chosen in the /settings dialog
// and persists them to the user config.
func (m *appModel) handleApplySettings(msg messages.ApplySettingsMsg) (tea.Model, tea.Cmd) {
	preferences := msg.Preferences
	model, cmd := m.applyLayoutSettings(preferences.Layout)

	m.sendMode = messages.ParseSendMode(string(preferences.SendMode))
	m.interruptMode = messages.ParseInterruptMode(string(preferences.InterruptConfirmation))
	m.showBanner = preferences.ShowBanner
	for _, page := range m.chatPages {
		page.SetSendMode(m.sendMode)
		page.SetInterruptMode(m.interruptMode)
		page.SetShowBanner(m.showBanner)
	}
	if m.sessionState.SplitDiffView() != preferences.SplitDiffView {
		m.sessionState.SetSplitDiffView(preferences.SplitDiffView)
		cmd = tea.Batch(cmd, m.updateChatCmd(editfile.ToggleDiffViewMsg{}))
	}
	m.sessionState.SetExpandThinking(preferences.ExpandThinking)
	m.sessionState.SetHideToolResults(preferences.HideToolResults)
	if m.imageWriter != nil {
		m.imageWriter.SetEnabled(preferences.RenderImages)
		tuiimage.SetRenderingEnabled(m.imageWriter.RenderingEnabled())
	}
	m.tabBar.SetMaxTitleLength(preferences.TabTitleMaxLength)
	cmd = tea.Batch(cmd, m.updateChatCmd(messages.SessionToggleChangedMsg{}), m.resizeAll())

	if err := savePreferences(preferences); err != nil {
		slog.Warn("Failed to save settings to user config", "error", err)
		return model, tea.Batch(cmd, notification.WarningCmd("Settings applied but could not be saved"))
	}
	return model, tea.Batch(cmd, notification.SuccessCmd("Settings updated"))
}

// applyLayoutSettings applies the given layout to every chat page (all tabs
// share the same layout) without persisting it.
func (m *appModel) applyLayoutSettings(settings messages.LayoutSettings) (tea.Model, tea.Cmd) {
	settings.SidebarPosition = messages.ParseSidebarPosition(string(settings.SidebarPosition))
	settings.SectionSpacing = messages.ParseSectionSpacing(string(settings.SectionSpacing))
	settings.SidebarInfoMode = messages.ParseSidebarInfoMode(string(settings.SidebarInfoMode))
	m.layoutSettings = settings

	var cmds []tea.Cmd
	for _, page := range m.chatPages {
		if cmd := page.SetLayoutSettings(settings); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	cmds = append(cmds, m.resizeAll())

	return m, tea.Batch(cmds...)
}

// layoutSettingsFromConfig converts persisted layout settings to their runtime form.
func layoutSettingsFromConfig(l userconfig.LayoutSettings) messages.LayoutSettings {
	return messages.LayoutSettings{
		SidebarPosition:  messages.ParseSidebarPosition(l.SidebarPosition),
		SectionSpacing:   messages.ParseSectionSpacing(l.SectionSpacing),
		SidebarInfoMode:  messages.ParseSidebarInfoMode(l.SidebarInfoMode),
		ActiveAgentsOnly: l.ActiveAgentsOnly,
		HideSessionPath:  l.HideSessionPath,
		HideUsage:        l.HideUsage,
		HideAgents:       l.HideAgents,
		HideTools:        l.HideTools,
		HideTodos:        l.HideTodos,
	}
}

// savePreferences persists every value managed by the settings dialog.
// Values matching their defaults are omitted to keep the config minimal.
func savePreferences(p messages.Preferences) error {
	return userconfig.Update(func(cfg *userconfig.Config) error {
		if cfg.Settings == nil {
			cfg.Settings = &userconfig.Settings{}
		}
		s := cfg.Settings
		if p.SendMode == messages.SendModeQueue {
			s.BusySendMode = string(messages.SendModeQueue)
		} else {
			s.BusySendMode = ""
		}
		s.SplitDiffView = boolPreference(p.SplitDiffView, true)
		s.ExpandThinking = boolPreference(p.ExpandThinking, false)
		s.RestoreTabs = boolPreference(p.RestoreTabs, false)
		s.Snapshot = boolPreference(p.Snapshot, false)
		s.CacheStablePrompts = boolPreference(p.CacheStablePrompts, false)
		s.WarnOnCacheMiss = boolPreference(p.WarnOnCacheMiss, false)
		s.HideToolResults = p.HideToolResults
		s.RenderImages = boolPreference(p.RenderImages, true)
		s.ShowBanner = boolPreference(p.ShowBanner, true)
		s.YOLO = p.YOLO
		s.Lean = p.Lean
		s.Sound = p.Sound
		s.InterruptConfirmation = string(p.InterruptConfirmation)
		if p.SoundThreshold == userconfig.DefaultSoundThreshold {
			s.SoundThreshold = 0
		} else {
			s.SoundThreshold = p.SoundThreshold
		}
		if p.TabTitleMaxLength == userconfig.DefaultTabTitleMaxLength {
			s.TabTitleMaxLength = 0
		} else {
			s.TabTitleMaxLength = p.TabTitleMaxLength
		}

		layout := p.Layout
		// Normalize before the default comparison so an unnormalized zero
		// value ("") and the explicit default ("compact") both clear the entry.
		layout.SidebarInfoMode = messages.ParseSidebarInfoMode(string(layout.SidebarInfoMode))
		if layout == (messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact}) {
			s.Layout = nil
			return nil
		}
		position := string(layout.SidebarPosition)
		if layout.SidebarPosition == messages.SidebarRight {
			position = ""
		}
		spacing := string(layout.SectionSpacing)
		if layout.SectionSpacing == messages.SpacingNormal {
			spacing = ""
		}
		infoMode := string(layout.SidebarInfoMode)
		if layout.SidebarInfoMode == messages.InfoModeCompact {
			infoMode = ""
		}
		s.Layout = &userconfig.LayoutSettings{
			SidebarPosition: position, SectionSpacing: spacing, SidebarInfoMode: infoMode,
			ActiveAgentsOnly: layout.ActiveAgentsOnly,
			HideSessionPath:  layout.HideSessionPath, HideUsage: layout.HideUsage,
			HideAgents: layout.HideAgents, HideTools: layout.HideTools, HideTodos: layout.HideTodos,
		}
		return nil
	})
}

func boolPreference(value, defaultValue bool) *bool {
	if value == defaultValue {
		return nil
	}
	return &value
}

// saveSettingsToUserConfig remains as a narrow compatibility helper for callers
// that only update layout and send mode.
func saveSettingsToUserConfig(layout messages.LayoutSettings, mode messages.SendMode) error {
	settings := userconfig.Get()
	return savePreferences(messages.Preferences{
		Layout: layout, SendMode: mode, SplitDiffView: settings.GetSplitDiffView(),
		ExpandThinking: settings.GetExpandThinking(), HideToolResults: settings.HideToolResults,
		RenderImages: settings.GetRenderImages(), ShowBanner: settings.GetShowBanner(),
		YOLO: settings.YOLO, RestoreTabs: settings.GetRestoreTabs(), Snapshot: settings.SnapshotsEnabled(),
		CacheStablePrompts: settings.CacheStablePromptsEnabled(),
		WarnOnCacheMiss:    settings.CacheMissWarningsEnabled(),
		Lean:               settings.Lean, TabTitleMaxLength: settings.GetTabTitleMaxLength(),
		Sound: settings.GetSound(), SoundThreshold: settings.GetSoundThreshold(),
		InterruptConfirmation: messages.ParseInterruptMode(settings.GetInterruptConfirmation()),
	})
}

// handleColorSchemeChange reacts to a terminal light/dark report (a DEC mode
// 2031 event or an OSC 11 response). The polarity is always recorded so a
// later switch to the auto theme starts from the freshest value; the theme
// itself only changes while the auto theme is active.
func (m *appModel) handleColorSchemeChange(dark bool) (tea.Model, tea.Cmd) {
	styles.SetTerminalDark(dark)
	if !styles.AutoThemeEnabled() {
		return m, nil
	}
	resolved := styles.ResolveThemeRef(styles.AutoThemeRef)
	if current := styles.CurrentTheme(); current != nil && current.Ref == resolved {
		return m, nil
	}
	theme, err := styles.LoadTheme(resolved)
	if err != nil {
		slog.Warn("Failed to load auto theme for terminal background change", "theme", resolved, "error", err)
		return m, nil
	}
	styles.ApplyTheme(theme)
	return m, core.CmdHandler(messages.ThemeChangedMsg{})
}

// --- Miscellaneous ---

func (m *appModel) handleOpenURL(url string) (tea.Model, tea.Cmd) {
	if err := browser.Open(m.ctx(), url); err != nil {
		slog.Warn("Failed to open URL", "url", url, "error", err)
		return m, notification.ErrorCmd("Failed to open URL in browser")
	}
	return m, nil
}

func (m *appModel) handleAgentCommand(command string) (tea.Model, tea.Cmd) {
	ctx := m.ctx()

	// Inspect the command before resolving so we can detect /commands that
	// switch to a sub-agent. For those, we switch first and only then send
	// the resolved message — otherwise the message would be processed by
	// the previous agent.
	cmd, _, ok := m.application.LookupCommand(ctx, command)

	// URL commands open the configured URL in the browser instead of sending
	// a prompt to the agent.
	if ok && cmd.URL != "" {
		return m, core.CmdHandler(messages.OpenURLMsg{URL: m.expandURLPlaceholders(cmd.URL)})
	}

	resolved := m.application.ResolveCommand(ctx, command)

	var cmds []tea.Cmd
	switchSucceeded := true
	if ok && cmd.Agent != "" && cmd.Agent != m.sessionState.CurrentAgentName() {
		// Attempt to switch agents. If the switch fails, handleSwitchAgent
		// returns an error notification command. We check if the agent actually
		// changed to determine success, rather than relying on the command type.
		prevAgent := m.sessionState.CurrentAgentName()
		switched, switchCmd := m.handleSwitchAgent(cmd.Agent)
		var ok bool
		if m, ok = switched.(*appModel); !ok {
			// This should never happen, but if it does, log and continue with the original model
			slog.WarnContext(ctx, "handleSwitchAgent returned unexpected type", "type", fmt.Sprintf("%T", switched))
			switchSucceeded = false
		} else {
			// Check if the agent actually changed to determine if the switch succeeded.
			// If it failed, we must not send the message to the wrong agent.
			switchSucceeded = m.sessionState.CurrentAgentName() != prevAgent
		}
		if switchCmd != nil {
			cmds = append(cmds, switchCmd)
		}
	}

	if resolved != "" && switchSucceeded {
		cmds = append(cmds, core.CmdHandler(messages.SendMsg{Content: resolved, BypassQueue: true}))
	}

	return m, tea.Batch(cmds...)
}

// expandURLPlaceholders substitutes runtime placeholders in a command URL.
// Currently only {{session_id}} is supported. The token is intentionally
// distinct from the ${...} syntax used for config-time JS expansion, since
// the session ID is only known at dispatch time.
func (m *appModel) expandURLPlaceholders(url string) string {
	var sessionID string
	if m.application != nil {
		if sess := m.application.Session(); sess != nil {
			sessionID = sess.ID
		}
	}
	return expandSessionPlaceholder(url, sessionID)
}

// expandSessionPlaceholder replaces the {{session_id}} token with sessionID,
// URL-query-escaped so it can't break the URL or inject extra parameters.
func expandSessionPlaceholder(url, sessionID string) string {
	if !strings.Contains(url, "{{session_id}}") {
		return url
	}
	return strings.ReplaceAll(url, "{{session_id}}", neturl.QueryEscape(sessionID))
}

func (m *appModel) handleAttachFile(filePath string) (tea.Model, tea.Cmd) {
	if filePath != "" {
		if err := m.editor.AttachFile(filePath); err != nil {
			slog.Warn("failed to attach file", "path", filePath, "error", err)
			// Attachment failed — open the file picker with an error notification
			return m, tea.Batch(
				notification.ErrorCmd("Failed to attach "+filePath),
				core.CmdHandler(dialog.OpenDialogMsg{
					Model: dialog.NewFilePickerDialog(filePath),
				}),
			)
		}
		return m, notification.SuccessCmd("File attached: " + filePath)
	}

	// No path provided — open the file picker dialog
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewFilePickerDialog(filePath),
	})
}

// --- Speech-to-text ---

func (m *appModel) handleStartSpeak() (tea.Model, tea.Cmd) {
	if m.transcriber.IsRunning() {
		return m, nil
	}

	// Close any previous channel to unblock stale waitForTranscript goroutines.
	m.closeTranscriptCh()

	ch := make(chan string, 100)
	m.transcriptCh = ch
	err := m.transcriber.Start(m.ctx(), func(delta string) {
		select {
		case ch <- delta:
		default:
		}
	})
	if err != nil {
		m.closeTranscriptCh()
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to start listening: %v", err))
	}

	return m, tea.Batch(
		notification.InfoCmd("🎤 Listening... (ENTER to send or ESC to cancel)"),
		m.editor.SetRecording(true),
		m.waitForTranscript(),
	)
}

func (m *appModel) handleStopSpeak() (tea.Model, tea.Cmd) {
	if !m.transcriber.IsRunning() {
		return m, nil
	}

	m.transcriber.Stop()
	m.closeTranscriptCh()

	return m, tea.Batch(m.editor.SetRecording(false), notification.SuccessCmd("Stopped listening"))
}

// waitForTranscript returns a command that blocks until the next transcript
// delta arrives and delivers it as a SpeakTranscriptMsg.
func (m *appModel) waitForTranscript() tea.Cmd {
	ch := m.transcriptCh
	return func() tea.Msg {
		delta, ok := <-ch
		if !ok {
			return nil
		}
		return messages.SpeakTranscriptMsg{Delta: delta}
	}
}

// closeTranscriptCh closes the transcript channel and sets it to nil,
// unblocking any goroutines waiting in waitForTranscript.
func (m *appModel) closeTranscriptCh() {
	if m.transcriptCh != nil {
		close(m.transcriptCh)
		m.transcriptCh = nil
	}
}

func (m *appModel) handleElicitationResponse(action tools.ElicitationAction, content map[string]any, elicitationID string) (tea.Model, tea.Cmd) {
	if err := m.application.ResumeElicitation(m.ctx(), action, content, elicitationID); err != nil {
		slog.Error("Failed to resume elicitation", "action", action, "error", err)
		return m, notification.ErrorCmd("Failed to complete server request: " + err.Error())
	}
	return m, nil
}

func (m *appModel) startShell() (tea.Model, tea.Cmd) {
	cmd := shellpath.InteractiveShellCmd("Type 'exit' to return to " + m.appName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Run the shell in the active session's working directory so it matches
	// where the tools operate (e.g. the worktree created by --worktree),
	// rather than inheriting the process CWD.
	if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil && runner.WorkingDir != "" {
		cmd.Dir = runner.WorkingDir
	}
	return m, tea.ExecProcess(cmd, nil)
}
