package tui

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/cagent/pkg/app"
	"github.com/docker/cagent/pkg/audio/transcribe"
	"github.com/docker/cagent/pkg/environment"
	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/tui/animation"
	"github.com/docker/cagent/pkg/tui/commands"
	"github.com/docker/cagent/pkg/tui/components/completion"
	"github.com/docker/cagent/pkg/tui/components/markdown"
	"github.com/docker/cagent/pkg/tui/components/notification"
	"github.com/docker/cagent/pkg/tui/components/statusbar"
	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/dialog"
	"github.com/docker/cagent/pkg/tui/messages"
	"github.com/docker/cagent/pkg/tui/page/chat"
	"github.com/docker/cagent/pkg/tui/service"
	"github.com/docker/cagent/pkg/tui/styles"
	"github.com/docker/cagent/pkg/tui/subscription"
)

// appModel represents the main application model
type appModel struct {
	application     *app.App
	wWidth, wHeight int
	width, height   int
	keyMap          KeyMap

	chatPage  chat.Page
	statusBar statusbar.StatusBar

	notification notification.Manager
	dialog       dialog.Manager
	completions  completion.Manager

	sessionState *service.SessionState

	transcriber *transcribe.Transcriber

	// External event subscriptions (Elm Architecture pattern)
	themeWatcher      *styles.ThemeWatcher
	themeSubscription *subscription.ChannelSubscription[string] // Listens for theme file changes
	themeSubStarted   bool                                      // Guard against multiple subscriptions

	// keyboardEnhancements stores the last keyboard enhancements message from the terminal.
	// This is reapplied to new chat/editor instances when sessions are switched.
	keyboardEnhancements *tea.KeyboardEnhancementsMsg

	ready bool
	err   error
}

// KeyMap defines global key bindings
type KeyMap struct {
	Quit                  key.Binding
	Suspend               key.Binding
	CommandPalette        key.Binding
	ToggleYolo            key.Binding
	ToggleHideToolResults key.Binding
	CycleAgent            key.Binding
	ModelPicker           key.Binding
	Speak                 key.Binding
	ClearQueue            key.Binding
}

// DefaultKeyMap returns the default global key bindings
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("Ctrl+c", "quit"),
		),
		Suspend: key.NewBinding(
			key.WithKeys("ctrl+z"),
			key.WithHelp("Ctrl+z", "suspend"),
		),
		CommandPalette: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("Ctrl+p", "commands"),
		),
		ToggleYolo: key.NewBinding(
			key.WithKeys("ctrl+y"),
			key.WithHelp("Ctrl+y", "toggle yolo mode"),
		),
		ToggleHideToolResults: key.NewBinding(
			key.WithKeys("ctrl+o"),
			key.WithHelp("Ctrl+o", "toggle tool output"),
		),
		CycleAgent: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("Ctrl+s", "cycle agent"),
		),
		ModelPicker: key.NewBinding(
			key.WithKeys("ctrl+m"),
			key.WithHelp("Ctrl+m", "models"),
		),
		Speak: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("Ctrl+l", "speak"),
		),
		ClearQueue: key.NewBinding(
			key.WithKeys("ctrl+x"),
			key.WithHelp("Ctrl+x", "clear queue"),
		),
	}
}

func getEnvVar(key string) string {
	val, _ := environment.NewOsEnvProvider().Get(context.Background(), key)
	return val
}

// New creates and initializes a new TUI application model
func New(ctx context.Context, a *app.App) tea.Model {
	sessionState := service.NewSessionState(a.Session())

	// Create a channel for theme file change events
	themeEventCh := make(chan string, 1)

	t := &appModel{
		keyMap:       DefaultKeyMap(),
		dialog:       dialog.New(),
		notification: notification.New(),
		completions:  completion.New(),
		application:  a,
		sessionState: sessionState,
		transcriber:  transcribe.New(getEnvVar("OPENAI_API_KEY")),
		// Set up theme subscription using the subscription package
		themeSubscription: subscription.NewChannelSubscription(themeEventCh, func(themeRef string) tea.Msg {
			return messages.ThemeFileChangedMsg{ThemeRef: themeRef}
		}),
	}

	// Create theme watcher with callback that sends to the subscription channel
	t.themeWatcher = styles.NewThemeWatcher(func(themeRef string) {
		// Non-blocking send to the event channel
		select {
		case themeEventCh <- themeRef:
		default:
			// Channel full, event will be coalesced
		}
	})

	t.statusBar = statusbar.New(t)
	t.chatPage = chat.New(a, sessionState)

	// Start watching the current theme (if it's a user theme file)
	currentTheme := styles.CurrentTheme()
	if currentTheme != nil && currentTheme.Ref != "" {
		_ = t.themeWatcher.Watch(currentTheme.Ref)
	}

	// Make sure to stop the progress bar and theme watcher when the app quits abruptly.
	go func() {
		<-ctx.Done()
		t.chatPage.Cleanup()
		t.themeWatcher.Stop()
	}()

	return t
}

// Init initializes the application
func (a *appModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		a.dialog.Init(),
		a.chatPage.Init(),
		a.application.SendFirstMessage(),
	}

	// Start theme subscription only once (guard against Init being called multiple times)
	if !a.themeSubStarted {
		a.themeSubStarted = true
		cmds = append(cmds, a.themeSubscription.Listen())
	}

	return tea.Sequence(cmds...)
}

// Help returns help information
func (a *appModel) Help() help.KeyMap {
	return core.NewSimpleHelp(a.Bindings())
}

func (a *appModel) Bindings() []key.Binding {
	return append([]key.Binding{
		a.keyMap.Quit,
		a.keyMap.CommandPalette,
		a.keyMap.ModelPicker,
	}, a.chatPage.Bindings()...)
}

func (a *appModel) handleWheelMsg(msg tea.MouseWheelMsg) tea.Cmd {
	if a.dialog.Open() {
		u, dialogCmd := a.dialog.Update(msg)
		a.dialog = u.(dialog.Manager)
		return dialogCmd
	}

	updated, chatCmd := a.chatPage.Update(msg)
	a.chatPage = updated.(chat.Page)
	return chatCmd
}

func (a *appModel) handleDialogWheelDelta(msg messages.WheelCoalescedMsg) tea.Cmd {
	steps := msg.Delta
	button := tea.MouseWheelDown
	if steps < 0 {
		steps = -steps
		button = tea.MouseWheelUp
	}

	var cmds []tea.Cmd
	for range steps {
		u, dialogCmd := a.dialog.Update(tea.MouseWheelMsg{X: msg.X, Y: msg.Y, Button: button})
		a.dialog = u.(dialog.Manager)
		if dialogCmd != nil {
			cmds = append(cmds, dialogCmd)
		}
	}

	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// Update handles incoming messages and updates the application state
func (a *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	// Handle global animation tick - broadcast to all components
	case animation.TickMsg:
		var cmds []tea.Cmd
		// Forward to chat page (which forwards to all child components with animations)
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		cmds = append(cmds, cmd)
		// Continue ticking if any animations are still active
		if animation.HasActive() {
			cmds = append(cmds, animation.StartTick())
		}
		return a, tea.Batch(cmds...)

	// Handle dialog-specific messages first
	case dialog.OpenDialogMsg, dialog.CloseDialogMsg:
		u, dialogCmd := a.dialog.Update(msg)
		a.dialog = u.(dialog.Manager)
		return a, dialogCmd

	case *runtime.TeamInfoEvent:
		a.sessionState.SetAvailableAgents(msg.AvailableAgents)
		a.sessionState.SetCurrentAgentName(msg.CurrentAgent)
		// Forward to chat page
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case *runtime.AgentInfoEvent:
		a.sessionState.SetCurrentAgentName(msg.AgentName)
		a.application.TrackCurrentAgentModel(msg.Model)
		// Forward to chat page
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case *runtime.SessionTitleEvent:
		a.sessionState.SetSessionTitle(msg.Title)
		// Forward to chat page (which forwards to sidebar)
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case messages.SwitchAgentMsg:
		return a.handleSwitchAgent(msg.AgentName)

	case tea.WindowSizeMsg:
		a.wWidth, a.wHeight = msg.Width, msg.Height
		cmd := a.handleWindowResize(msg.Width, msg.Height)
		a.completions.Update(msg)
		return a, cmd

	case tea.KeyboardEnhancementsMsg:
		// Store the keyboard enhancements message so we can reapply it to new chat pages
		a.keyboardEnhancements = &msg
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case notification.ShowMsg, notification.HideMsg:
		updated, cmd := a.notification.Update(msg)
		a.notification = updated
		return a, cmd

	case messages.InvalidateStatusBarMsg:
		// Invalidate statusbar cache when bindings change (e.g., inline edit mode)
		a.statusBar.InvalidateCache()
		return a, nil

	case tea.KeyPressMsg:
		return a.handleKeyPressMsg(msg)

	case tea.PasteMsg:
		// If dialogs are active, only they should receive paste events.
		// This prevents paste content from going to both dialog and editor.
		if a.dialog.Open() {
			u, dialogCmd := a.dialog.Update(msg)
			a.dialog = u.(dialog.Manager)
			return a, dialogCmd
		}
		// Otherwise forward to chat page (editor)
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case tea.MouseWheelMsg:
		cmd := a.handleWheelMsg(msg)
		return a, cmd

	case messages.WheelCoalescedMsg:
		if msg.Delta == 0 {
			return a, nil
		}
		if a.dialog.Open() {
			cmd := a.handleDialogWheelDelta(msg)
			return a, cmd
		}

		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		// If dialogs are active, they get priority for mouse events
		if a.dialog.Open() {
			u, dialogCmd := a.dialog.Update(msg)
			a.dialog = u.(dialog.Manager)
			return a, dialogCmd
		}
		// Otherwise forward to chat page
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case messages.ExitSessionMsg:
		// /exit command exits immediately without confirmation
		a.chatPage.Cleanup()
		return a, tea.Quit

	case messages.NewSessionMsg:
		return a.handleNewSession()

	case messages.OpenSessionBrowserMsg:
		return a.handleOpenSessionBrowser()

	case messages.LoadSessionMsg:
		return a.handleLoadSession(msg.SessionID)

	case messages.BranchFromEditMsg:
		return a.handleBranchFromEdit(msg)

	case messages.ToggleSessionStarMsg:
		sessionID := msg.SessionID
		if sessionID == "" {
			// Empty ID means current session
			if sess := a.application.Session(); sess != nil {
				sessionID = sess.ID
			} else {
				return a, nil
			}
		}
		return a.handleToggleSessionStar(sessionID)

	case messages.SetSessionTitleMsg:
		return a.handleSetSessionTitle(msg.Title)

	case messages.RegenerateTitleMsg:
		return a.handleRegenerateTitle()

	case messages.StartShellMsg:
		return a.startShell()

	case messages.EvalSessionMsg:
		return a.handleEvalSession(msg.Filename)

	case messages.ExportSessionMsg:
		return a.handleExportSession(msg.Filename)

	case messages.CompactSessionMsg:
		return a.handleCompactSession(msg.AdditionalPrompt)

	case messages.CopySessionToClipboardMsg:
		return a.handleCopySessionToClipboard()

	case messages.CopyLastResponseToClipboardMsg:
		return a.handleCopyLastResponseToClipboard()

	case messages.ToggleYoloMsg:
		return a.handleToggleYolo()

	case messages.ToggleThinkingMsg:
		return a.handleToggleThinking()

	case messages.ToggleHideToolResultsMsg:
		return a.handleToggleHideToolResults()

	case messages.ClearQueueMsg:
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd

	case messages.ShowCostDialogMsg:
		return a.handleShowCostDialog()

	case messages.ShowPermissionsDialogMsg:
		return a.handleShowPermissionsDialog()

	case messages.AgentCommandMsg:
		return a.handleAgentCommand(msg.Command)

	case messages.ShowMCPPromptInputMsg:
		return a.handleShowMCPPromptInput(msg.PromptName, msg.PromptInfo)

	case messages.MCPPromptMsg:
		return a.handleMCPPrompt(msg.PromptName, msg.Arguments)

	case messages.OpenURLMsg:
		return a.handleOpenURL(msg.URL)

	case messages.AttachFileMsg:
		return a.handleAttachFile(msg.FilePath)

	case messages.StartSpeakMsg:
		if !a.transcriber.IsSupported() {
			return a, notification.InfoCmd("Speech-to-text is only supported on macOS")
		}
		return a.handleStartSpeak()

	case messages.StopSpeakMsg:
		return a.handleStopSpeak()

	case messages.SpeakTranscriptMsg:
		return a.handleSpeakTranscript(msg.Delta)

	case messages.OpenModelPickerMsg:
		return a.handleOpenModelPicker()

	case messages.ChangeModelMsg:
		return a.handleChangeModel(msg.ModelRef)

	case messages.OpenThemePickerMsg:
		return a.handleOpenThemePicker()

	case messages.ChangeThemeMsg:
		return a.handleChangeTheme(msg.ThemeRef)

	case messages.ThemePreviewMsg:
		return a.handleThemePreview(msg.ThemeRef)

	case messages.ThemeCancelPreviewMsg:
		return a.handleThemeCancelPreview(msg.OriginalRef)

	case messages.ThemeChangedMsg:
		return a.applyThemeChanged()

	case messages.ThemeFileChangedMsg:
		// Theme file was modified on disk - load and apply on the main goroutine
		theme, err := styles.LoadTheme(msg.ThemeRef)
		if err != nil {
			// Failed to load - show error but keep current theme
			return a, tea.Batch(
				a.themeSubscription.Listen(), // Re-subscribe to continue listening
				notification.ErrorCmd(fmt.Sprintf("Failed to hot-reload theme: %v", err)),
			)
		}
		styles.ApplyTheme(theme)
		// Continue listening for more changes and emit ThemeChangedMsg for cache invalidation
		return a, tea.Batch(
			a.themeSubscription.Listen(), // Re-subscribe to continue listening
			notification.SuccessCmd("Theme hot-reloaded"),
			core.CmdHandler(messages.ThemeChangedMsg{}),
		)

	case messages.ElicitationResponseMsg:
		return a.handleElicitationResponse(msg.Action, msg.Content)

	case messages.SendAttachmentMsg:
		// Handle first message with image attachment using the pre-prepared message
		a.application.RunWithMessage(context.Background(), nil, msg.Content)
		return a, nil

	case speakTranscriptAndContinue:
		// Insert the transcript delta into the editor
		a.chatPage.InsertText(msg.delta)
		// Continue listening for more transcripts
		cmd := a.listenForTranscripts(msg.ch)
		return a, cmd

	case dialog.MultiChoiceResultMsg:
		// Handle multi-choice dialog results
		if msg.DialogID == dialog.ToolRejectionDialogID {
			if msg.Result.IsCancelled {
				// User cancelled - multi-choice dialog already closed, tool confirmation still open
				return a, nil
			}
			// User selected a reason - close the tool confirmation dialog and send resume
			resumeMsg := dialog.HandleToolRejectionResult(msg.Result)
			if resumeMsg != nil {
				return a, tea.Sequence(
					core.CmdHandler(dialog.CloseDialogMsg{}), // Close tool confirmation dialog
					core.CmdHandler(*resumeMsg),
				)
			}
		}
		return a, nil

	case dialog.RuntimeResumeMsg:
		a.application.Resume(msg.Request)
		return a, nil

	case dialog.ExitConfirmedMsg:
		a.chatPage.Cleanup()
		return a, tea.Quit

	case messages.ExitAfterFirstResponseMsg:
		a.chatPage.Cleanup()
		return a, tea.Quit

	case chat.EditorHeightChangedMsg:
		a.completions.SetEditorBottom(msg.Height)
		return a, nil

	case error:
		a.err = msg
		return a, nil

	default:
		if event, isRuntimeEvent := msg.(runtime.Event); isRuntimeEvent {
			// Update current agent name if the event carries one
			if agentName := event.GetAgentName(); agentName != "" {
				a.sessionState.SetCurrentAgentName(agentName)
			}
			// Forward to chat page
			updated, cmd := a.chatPage.Update(msg)
			a.chatPage = updated.(chat.Page)
			return a, cmd
		}

		// For other messages, check if dialogs should handle them first
		// If dialogs are active, they get priority for input
		if a.dialog.Open() {
			u, dialogCmd := a.dialog.Update(msg)
			a.dialog = u.(dialog.Manager)

			updated, cmdChatPage := a.chatPage.Update(msg)
			a.chatPage = updated.(chat.Page)

			return a, tea.Batch(dialogCmd, cmdChatPage)
		}

		updated, cmdCompletions := a.completions.Update(msg)
		a.completions = updated.(completion.Manager)

		updated, cmdChatPage := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)

		return a, tea.Batch(cmdCompletions, cmdChatPage)
	}
}

// handleWindowResize processes window resize events
func (a *appModel) handleWindowResize(width, height int) tea.Cmd {
	var cmds []tea.Cmd

	// Update status bar width first so we can measure its height
	a.statusBar.SetWidth(width)

	// Compute status bar height from rendered content
	statusBarHeight := 1 // default fallback
	if statusBarView := a.statusBar.View(); statusBarView != "" {
		statusBarHeight = lipgloss.Height(statusBarView)
	}

	// Update dimensions, reserving space for the status bar
	a.width, a.height = width, height-statusBarHeight

	if !a.ready {
		a.ready = true
	}

	// Update dialog system
	u, cmd := a.dialog.Update(tea.WindowSizeMsg{Width: width, Height: height})
	a.dialog = u.(dialog.Manager)
	cmds = append(cmds, cmd)

	cmd = a.chatPage.SetSize(a.width, a.height)
	cmds = append(cmds, cmd)

	// Update completion manager with actual editor height for popup positioning
	a.completions.SetEditorBottom(a.chatPage.GetInputHeight())

	// Update notification size
	a.notification.SetSize(a.width, a.height)

	return tea.Batch(cmds...)
}

func (a *appModel) handleKeyPressMsg(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Check if we should stop transcription on Enter or Escape
	if a.transcriber.IsRunning() {
		switch msg.String() {
		case "enter":
			model, cmd := a.handleStopSpeak()
			sendCmd := a.chatPage.SendEditorContent()
			return model, tea.Batch(cmd, sendCmd)

		case "esc":
			return a.handleStopSpeak()
		}
	}

	if a.dialog.Open() {
		u, dialogCmd := a.dialog.Update(msg)
		a.dialog = u.(dialog.Manager)
		return a, dialogCmd
	}

	if a.completions.Open() {
		// Check if this is a navigation key that the completion manager should handle
		if core.IsNavigationKey(msg) {
			// Let completion manager handle navigation keys
			u, completionCmd := a.completions.Update(msg)
			a.completions = u.(completion.Manager)
			return a, completionCmd
		}

		// For all other keys (typing), send to both completion (for filtering) and editor
		var cmds []tea.Cmd
		u, completionCmd := a.completions.Update(msg)
		a.completions = u.(completion.Manager)
		cmds = append(cmds, completionCmd)

		// Also send to chat page/editor so user can continue typing
		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		cmds = append(cmds, cmd)

		return a, tea.Batch(cmds...)
	}

	switch {
	case key.Matches(msg, a.keyMap.Quit):
		return a, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewExitConfirmationDialog(),
		})

	case key.Matches(msg, a.keyMap.Suspend):
		return a, tea.Suspend

	case key.Matches(msg, a.keyMap.CommandPalette):
		categories := commands.BuildCommandCategories(context.Background(), a.application)
		return a, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewCommandPaletteDialog(categories),
		})

	case key.Matches(msg, a.keyMap.ToggleYolo):
		return a, core.CmdHandler(messages.ToggleYoloMsg{})

	case key.Matches(msg, a.keyMap.ToggleHideToolResults):
		return a, core.CmdHandler(messages.ToggleHideToolResultsMsg{})

	case key.Matches(msg, a.keyMap.CycleAgent):
		return a.handleCycleAgent()

	case key.Matches(msg, a.keyMap.ModelPicker):
		return a.handleOpenModelPicker()

	case key.Matches(msg, a.keyMap.Speak):
		if a.transcriber.IsSupported() {
			return a.handleStartSpeak()
		}
		return a, notification.InfoCmd("Speech-to-text is only supported on macOS")

	case key.Matches(msg, a.keyMap.ClearQueue):
		return a, core.CmdHandler(messages.ClearQueueMsg{})

	default:
		// Handle ctrl+1 through ctrl+9 for quick agent switching
		if index := parseCtrlNumberKey(msg); index >= 0 {
			return a.handleSwitchToAgentByIndex(index)
		}

		updated, cmd := a.chatPage.Update(msg)
		a.chatPage = updated.(chat.Page)
		return a, cmd
	}
}

// parseCtrlNumberKey checks if msg is ctrl+1 through ctrl+9 and returns the index (0-8), or -1 if not matched
func parseCtrlNumberKey(msg tea.KeyPressMsg) int {
	s := msg.String()
	if len(s) == 6 && s[:5] == "ctrl+" && s[5] >= '1' && s[5] <= '9' {
		return int(s[5] - '1')
	}
	return -1
}

// View renders the complete application interface
func (a *appModel) View() tea.View {
	windowTitle := a.windowTitle()

	// Show error if present
	if a.err != nil {
		return toFullscreenView(styles.ErrorStyle.Render(a.err.Error()), windowTitle)
	}

	// Show loading if not ready
	if !a.ready {
		return toFullscreenView(
			styles.CenterStyle.
				Width(a.wWidth).
				Height(a.wHeight).
				Render(styles.MutedStyle.Render("Loading…")),
			windowTitle,
		)
	}

	// Render chat page
	pageView := a.chatPage.View()

	// Create status bar
	statusBar := a.statusBar.View()

	// Combine page view with status bar
	var components []string
	components = append(components, pageView)
	if statusBar != "" {
		components = append(components, statusBar)
	}

	baseView := lipgloss.JoinVertical(lipgloss.Top, components...)

	hasOverlays := a.dialog.Open() || a.notification.Open() || a.completions.Open()

	if hasOverlays {
		baseLayer := lipgloss.NewLayer(baseView)
		var allLayers []*lipgloss.Layer
		allLayers = append(allLayers, baseLayer)

		// Add dialog layers
		if a.dialog.Open() {
			dialogLayers := a.dialog.GetLayers()
			allLayers = append(allLayers, dialogLayers...)
		}

		if a.notification.Open() {
			allLayers = append(allLayers, a.notification.GetLayer())
		}

		if a.completions.Open() {
			layers := a.completions.GetLayers()
			allLayers = append(allLayers, layers...)
		}

		canvas := lipgloss.NewCanvas(allLayers...)
		return toFullscreenView(canvas.Render(), windowTitle)
	}

	return toFullscreenView(baseView, windowTitle)
}

// windowTitle returns the terminal window title
func (a *appModel) windowTitle() string {
	if sessionTitle := a.sessionState.SessionTitle(); sessionTitle != "" {
		return sessionTitle + " - cagent"
	}
	return "cagent"
}

func (a *appModel) startShell() (tea.Model, tea.Cmd) {
	var cmd *exec.Cmd

	if goruntime.GOOS == "windows" {
		// Prefer PowerShell (pwsh or Windows PowerShell) when available, otherwise fall back to cmd.exe
		if path, err := exec.LookPath("pwsh.exe"); err == nil {
			cmd = exec.Command(path, "-NoLogo", "-NoExit", "-Command",
				`Write-Host ""; Write-Host "Type 'exit' to return to cagent 🐳"`)
		} else if path, err := exec.LookPath("powershell.exe"); err == nil {
			cmd = exec.Command(path, "-NoLogo", "-NoExit", "-Command",
				`Write-Host ""; Write-Host "Type 'exit' to return to cagent 🐳"`)
		} else {
			// Use ComSpec if available, otherwise default to cmd.exe
			shell := cmp.Or(os.Getenv("ComSpec"), "cmd.exe")
			cmd = exec.Command(shell, "/K", `echo. & echo Type 'exit' to return to cagent`)
		}
	} else {
		// Unix-like: use SHELL or default to /bin/sh
		shell := cmp.Or(os.Getenv("SHELL"), "/bin/sh")
		cmd = exec.Command(shell, "-i", "-c",
			`echo -e "\nType 'exit' to return to cagent 🐳"; exec `+shell)
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return a, tea.ExecProcess(cmd, nil)
}

// invalidateCachesForThemeChange performs synchronous cache invalidation
// after a theme change. This does NOT forward messages to child components.
// Use applyThemeChanged() when you also need to forward ThemeChangedMsg.
func (a *appModel) invalidateCachesForThemeChange() {
	markdown.ResetStyles()
	a.statusBar.InvalidateCache()
}

// applyThemeChanged invalidates all theme-dependent caches and forwards
// ThemeChangedMsg to child components. This is called synchronously when
// themes are changed/previewed to ensure View() renders with updated styles.
func (a *appModel) applyThemeChanged() (tea.Model, tea.Cmd) {
	// Invalidate all caches
	a.invalidateCachesForThemeChange()

	// Update theme watcher to watch new theme file
	currentTheme := styles.CurrentTheme()
	if currentTheme != nil {
		_ = a.themeWatcher.Watch(currentTheme.Ref)
	}

	var cmds []tea.Cmd

	// Forward to dialog manager to propagate to all open dialogs
	dialogUpdated, dialogCmd := a.dialog.Update(messages.ThemeChangedMsg{})
	a.dialog = dialogUpdated.(dialog.Manager)
	cmds = append(cmds, dialogCmd)

	// Forward to chat page to propagate to all child components
	chatUpdated, chatCmd := a.chatPage.Update(messages.ThemeChangedMsg{})
	a.chatPage = chatUpdated.(chat.Page)
	cmds = append(cmds, chatCmd)

	return a, tea.Batch(cmds...)
}

func toFullscreenView(content, windowTitle string) tea.View {
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.BackgroundColor = styles.Background
	view.WindowTitle = windowTitle

	return view
}
