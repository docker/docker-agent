package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/evaluation"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
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

	ctx := context.Background()

	parent, err := store.GetSession(ctx, msg.ParentSessionID)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load parent session: %v", err))
	}

	newSess, err := session.BranchSession(parent, msg.BranchAtPosition)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to branch session: %v", err))
	}

	if err := store.AddSession(ctx, newSess); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to save branched session: %v", err))
	}

	if current := m.application.Session(); current != nil {
		newSess.HideToolResults = current.HideToolResults
		newSess.ToolsApproved = current.ToolsApproved
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

	ctx := context.Background()

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
		if err := store.UpdateSession(context.Background(), currentSess); err != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to save session: %v", err))
		}
	} else {
		sess, err := store.GetSession(context.Background(), sessionID)
		if err != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to load session: %v", err))
		}
		if err := store.SetSessionStarred(context.Background(), sessionID, !sess.Starred); err != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to update session: %v", err))
		}
	}
	return m, nil
}

func (m *appModel) handleSetSessionTitle(title string) (tea.Model, tea.Cmd) {
	if err := m.application.UpdateSessionTitle(context.Background(), title); err != nil {
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
	if err := m.application.RegenerateSessionTitle(context.Background()); err != nil {
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

	if err := store.DeleteSession(context.Background(), sessionID); err != nil {
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
	exportFile, err := m.application.ExportHTML(context.Background(), filename)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to export session: %v", err))
	}
	return m, notification.SuccessCmd("Session exported to " + exportFile)
}

func (m *appModel) handleCompactSession(additionalPrompt string) (tea.Model, tea.Cmd) {
	return m, m.chatPage.CompactSession(additionalPrompt)
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
	result, err := m.application.UndoLastSnapshot(context.Background())
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
	result, err := m.application.ResetSnapshot(context.Background(), keep)
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
	if err := m.application.SwitchAgent(agentName); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to switch to agent '%s': %v", agentName, err))
	}
	m.sessionState.SetCurrentAgentName(agentName)
	return m, tea.Batch(
		m.updateChatCmd(messages.SessionToggleChangedMsg{}),
		notification.SuccessCmd(fmt.Sprintf("Switched to agent '%s'", agentName)),
	)
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

func (m *appModel) handleToggleYolo() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	sess.ToolsApproved = !sess.ToolsApproved
	m.sessionState.SetYoloMode(sess.ToolsApproved)
	return m.forwardChat(messages.SessionToggleChangedMsg{})
}

// handleTogglePause toggles whether the runtime loop is paused at iteration
// boundaries. The pause kicks in once the in-flight LLM request and its tool
// calls finish; running /pause again resumes the loop.
func (m *appModel) handleTogglePause() (tea.Model, tea.Cmd) {
	paused, supported := m.application.TogglePause()
	switch {
	case !supported:
		return m, notification.InfoCmd("Pause is not supported with remote runtimes")
	case paused:
		return m, notification.InfoCmd("Runtime paused — /pause again to resume")
	default:
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
	cfg, err := userconfig.Load()
	if err != nil {
		slog.Warn("Failed to load userconfig for split diff toggle", "error", err)
		return
	}
	if cfg.Settings == nil {
		cfg.Settings = &userconfig.Settings{}
	}
	cfg.Settings.SplitDiffView = &enabled
	if err := cfg.Save(); err != nil {
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

func (m *appModel) handleShowPermissionsDialog() (tea.Model, tea.Cmd) {
	perms := m.application.PermissionsInfo()
	sess := m.application.Session()
	yoloEnabled := sess != nil && sess.ToolsApproved
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewPermissionsDialog(perms, yoloEnabled),
	})
}

func (m *appModel) handleShowToolsDialog() (tea.Model, tea.Cmd) {
	agentTools, err := m.application.CurrentAgentTools(context.Background())
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
			if err := appRef.RestartToolset(context.Background(), name); err != nil {
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
	promptContent, err := m.application.ExecuteMCPPrompt(context.Background(), promptName, arguments)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Error executing MCP prompt '%s': %v", promptName, err))
	}
	return m, core.CmdHandler(messages.SendMsg{Content: promptContent})
}

// --- Model picker ---

func (m *appModel) handleOpenModelPicker() (tea.Model, tea.Cmd) {
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Model switching is not supported with remote runtimes")
	}
	models := m.application.AvailableModels(context.Background())
	if len(models) == 0 {
		return m, notification.InfoCmd("No models available for selection")
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewModelPickerDialog(models),
	})
}

func (m *appModel) handleChangeModel(modelRef string) (tea.Model, tea.Cmd) {
	if err := m.application.SetCurrentAgentModel(context.Background(), modelRef); err != nil {
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

	var choices []dialog.ThemeChoice
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
			IsCurrent: ref == currentRef,
			IsDefault: ref == styles.DefaultThemeRef,
			IsBuiltin: styles.IsBuiltinTheme(ref),
		})
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewThemePickerDialog(choices, currentRef),
	})
}

func (m *appModel) handleChangeTheme(themeRef string) (tea.Model, tea.Cmd) {
	if styles.GetPersistedThemeRef() == themeRef {
		return m, nil
	}
	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load theme: %v", err))
	}
	styles.ApplyTheme(theme)
	m.invalidateCachesForThemeChange()

	if err := styles.SaveThemeToUserConfig(themeRef); err != nil {
		slog.Warn("Failed to save theme to user config", "theme", themeRef, "error", err)
	}
	return m, tea.Sequence(
		notification.SuccessCmd("Theme changed to "+theme.Name),
		core.CmdHandler(messages.ThemeChangedMsg{}),
	)
}

func (m *appModel) handleThemePreview(themeRef string) (tea.Model, tea.Cmd) {
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
	theme, err := styles.LoadTheme(originalRef)
	if err != nil {
		theme = styles.DefaultTheme()
	}
	styles.ApplyTheme(theme)
	return m.applyThemeChanged()
}

func (m *appModel) invalidateCachesForThemeChange() {
	markdown.ResetStyles()
	m.statusBar.InvalidateCache()
}

func (m *appModel) applyThemeChanged() (tea.Model, tea.Cmd) {
	m.invalidateCachesForThemeChange()
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

// --- Miscellaneous ---

func (m *appModel) handleOpenURL(url string) (tea.Model, tea.Cmd) {
	_ = browser.Open(context.Background(), url)
	return m, nil
}

func (m *appModel) handleAgentCommand(command string) (tea.Model, tea.Cmd) {
	resolvedCommand := m.application.ResolveCommand(context.Background(), command)
	return m, core.CmdHandler(messages.SendMsg{Content: resolvedCommand})
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
	err := m.transcriber.Start(context.Background(), func(delta string) {
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

func (m *appModel) handleElicitationResponse(action tools.ElicitationAction, content map[string]any) (tea.Model, tea.Cmd) {
	if err := m.application.ResumeElicitation(context.Background(), action, content); err != nil {
		slog.Error("Failed to resume elicitation", "action", action, "error", err)
		return m, notification.ErrorCmd("Failed to complete server request: " + err.Error())
	}
	return m, nil
}

func (m *appModel) startShell() (tea.Model, tea.Cmd) {
	cmd := newInteractiveShellCmd("Type 'exit' to return to " + m.appName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return m, tea.ExecProcess(cmd, nil)
}

// newInteractiveShellCmd returns a command that launches the user's preferred
// interactive shell. The command is owned by tea.ExecProcess, not by any
// request-scoped context, so exec.Command is intentional.
func newInteractiveShellCmd(exitMsg string) *exec.Cmd {
	if goruntime.GOOS != "windows" {
		shell := shellpath.DetectUnixShell()
		return execCmd(shell, "-i", "-c", `echo -e "\n`+exitMsg+`"; exec `+shell)
	}

	psArgs := []string{"-NoLogo", "-NoExit", "-Command", `Write-Host ""; Write-Host "` + exitMsg + `"`}
	if path, err := exec.LookPath("pwsh.exe"); err == nil {
		return execCmd(path, psArgs...)
	}
	if path, err := exec.LookPath("powershell.exe"); err == nil {
		return execCmd(path, psArgs...)
	}
	// Use absolute path to cmd.exe to prevent PATH hijacking (CWE-426).
	return execCmd(shellpath.WindowsCmdExe(), "/K", "echo. & echo "+exitMsg)
}

// execCmd is a thin wrapper around exec.Command used for interactive
// processes whose lifecycle is owned by tea.ExecProcess (not a context).
func execCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) //nolint:noctx // owned by tea.ExecProcess
}
