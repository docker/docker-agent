// Package tui provides the top-level TUI model with tab and session management.
package tui

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/audio/transcribe"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/editor/completions"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/statusbar"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/internal/editorname"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/docker/docker-agent/pkg/version"
)

// SessionSpawner creates new sessions with their own runtime.
// This is an alias to the supervisor package's SessionSpawner type.
type SessionSpawner = supervisor.SessionSpawner

// FocusedPanel represents which panel is currently focused
type FocusedPanel string

const (
	PanelContent FocusedPanel = "content"
	PanelEditor  FocusedPanel = "editor"

	// resizeHandleWidth is the width of the draggable center portion of the resize handle
	resizeHandleWidth = 8
	// appPaddingHorizontal is total horizontal padding from AppStyle (left + right)
	appPaddingHorizontal = 2 * styles.AppPadding
)

// Model is the top-level TUI model that wraps the chat page.
type appModel struct {
	supervisor *supervisor.Supervisor
	tabBar     *tabbar.TabBar
	tuiStore   *tuistate.Store

	// Per-session chat pages (kept alive for streaming continuity)
	chatPages     map[string]chat.Page
	sessionStates map[string]*service.SessionState

	// Per-session editors (preserved across tab switches for draft text)
	editors map[string]editor.Editor

	// Active session (convenience pointers to the currently visible session)
	application  *app.App
	sessionState *service.SessionState
	chatPage     chat.Page
	editor       editor.Editor

	// Shared history for command history across all editors
	history *history.History

	// UI components
	notification notification.Manager
	dialogMgr    dialog.Manager
	statusBar    statusbar.StatusBar
	completions  completion.Manager

	// Speech-to-text
	transcriber  Transcriber
	transcriptCh chan string // bridges transcriber goroutine → Bubble Tea event loop

	// Working state indicator (resize handle spinner)
	workingSpinner spinner.Spinner

	// animFrame is the current animation frame, used to rotate the window
	// title spinner so that tmux can detect pane activity.
	animFrame int

	// Window state
	wWidth, wHeight int
	width, height   int

	// Content area height (height minus editor, tab bar, resize handle, status bar)
	contentHeight int

	// Editor resize state
	editorLines      int
	isDragging       bool
	isHoveringHandle bool

	// Focus state
	focusedPanel FocusedPanel

	// keyboardEnhancements stores the last keyboard enhancements message
	keyboardEnhancements *tea.KeyboardEnhancementsMsg

	// keyboardEnhancementsSupported tracks whether the terminal supports keyboard enhancements
	keyboardEnhancementsSupported bool

	// program holds a reference to the tea.Program so that we can
	// perform a full terminal release/restore cycle on focus events.
	program *tea.Program

	// dockerDesktop is true when running inside Docker Desktop's terminal
	// (TERM_PROGRAM=docker_desktop). Focus reporting and the terminal
	// release/restore cycle on tab switch are only enabled in this
	// environment.
	dockerDesktop bool

	// focused tracks whether the terminal currently has focus. Used to
	// filter spurious FocusMsg events (RestoreTerminal re-enables focus
	// reporting and delivers one even though we never blurred). Starts
	// at the zero value (false) so the first FocusMsg is treated as a
	// real focus event — in Docker Desktop that runs the release/restore
	// cycle which re-emits terminal mode escape sequences.
	focused bool

	// tickPaused is true while we should drop animation.TickMsg events
	// (and let the tick chain die). Set on BlurMsg and cleared on the
	// next real FocusMsg. Tracked separately from `focused` so that ticks
	// keep flowing at startup even before any focus event arrives — some
	// terminals never send FocusMsg.
	tickPaused bool

	// pendingRestores maps runtime tab IDs (supervisor routing keys) to
	// persisted session-store IDs. When a tab with a pending restore is first
	// switched to, the persisted session is loaded via replaceActiveSession —
	// the same code path as the /sessions command.
	//
	// This map also serves as the authoritative source for "which persisted
	// session ID does this tab represent?" until the restore completes, at
	// which point the app's live session ID takes over.
	pendingRestores map[string]string

	// pendingSidebarCollapsed maps runtime tab IDs to their persisted sidebar
	// collapsed state. Consumed when a chat page is first created for a
	// restored tab (in handleSwitchTab) and then removed from the map.
	pendingSidebarCollapsed map[string]bool

	// stashedDialogs holds background dialog instances that were on screen
	// when the user navigated away from a tab. The dialog instance preserves
	// in-progress input (e.g. text typed into a user_prompt elicitation) so
	// that returning to the tab restores the same dialog rather than
	// rebuilding a fresh one from the originating runtime event.
	//
	// The stored event is matched against the supervisor's pending event on
	// return: if they no longer match (because the agent superseded the
	// prompt) the stashed dialog is discarded and a fresh one is built.
	stashedDialogs map[string]stashedDialog

	// pendingActiveTab is the tab ID to switch to on Init(). Set when the
	// previously focused tab differs from the initial tab.
	pendingActiveTab string

	ready bool
	err   error

	// leanMode enables a simplified TUI with minimal chrome.
	leanMode bool

	// buildCommandCategories is a function that returns the list of command categories.
	buildCommandCategories func(context.Context, tea.Model) []commands.Category

	appName    string
	appVersion string
}

// Transcriber is the speech-to-text interface used by the TUI. It is an
// interface (rather than the concrete *transcribe.Transcriber) so that tests
// can inject a fake implementation via WithTranscriber and so that the TUI
// does not depend on a concrete audio backend.
type Transcriber interface {
	Start(ctx context.Context, handler transcribe.TranscriptHandler) error
	Stop()
	IsRunning() bool
	IsSupported() bool
}

// Option configures the TUI.
type Option func(*appModel)

// WithLeanMode enables a simplified TUI with minimal chrome:
// no sidebar, no tab bar, no overlays, no resize handle.
func WithLeanMode() Option {
	return func(m *appModel) {
		m.leanMode = true
	}
}

// WithAppName sets the application name.
//
// If not provided, defaults to "docker agent".
func WithAppName(name string) Option {
	return func(m *appModel) {
		m.appName = name
	}
}

// WithVersion sets the application version.
//
// If not provided, defaults to version.Version.
func WithVersion(v string) Option {
	return func(m *appModel) {
		m.appVersion = v
	}
}

// WithCommandBuilder builds the command categories shown in the command
// palette from the given function. It overrides the default command category
// builder. To include the default commands, the given function should call
// commands.BuildCommandCategories and merge the result with its own.
//
// The tea.Model passed to the builder function must not be accessed during
// the build call itself - it should only be captured for use within command
// Execute functions. There is no guarantee that the tea.Model holds all
// dependencies during the build phase, which may cause [core.Resolve] to panic.
func WithCommandBuilder(
	fn func(context.Context, tea.Model) []commands.Category,
) Option {
	return func(m *appModel) {
		m.buildCommandCategories = fn
	}
}

// WithTranscriber overrides the speech-to-text backend used by the TUI. This
// is intended for tests that need to exercise speech handlers without
// connecting to a real audio device or external API.
func WithTranscriber(t Transcriber) Option {
	return func(m *appModel) {
		if t != nil {
			m.transcriber = t
		}
	}
}

// New creates a new Model.
func New(ctx context.Context, spawner SessionSpawner, initialApp *app.App, initialWorkingDir string, cleanup func(), opts ...Option) tea.Model {
	// Initialize supervisor
	sv := supervisor.New(spawner)

	// Initialize tab bar with configurable title length from user settings
	tabTitleMaxLen := userconfig.Get().GetTabTitleMaxLength()
	tb := tabbar.New(tabTitleMaxLen)

	// Initialize tab store
	var ts *tuistate.Store
	var tsErr error
	ts, tsErr = tuistate.New()
	if tsErr != nil {
		slog.WarnContext(ctx, "Failed to open TUI state store, tabs won't persist", "error", tsErr)
	}

	// Initialize shared command history
	historyStore, err := history.New("")
	if err != nil {
		slog.WarnContext(ctx, "Failed to initialize command history", "error", err)
	}

	initialSessionState := service.NewSessionState(initialApp.Session())
	sessID := initialApp.Session().ID

	m := &appModel{
		buildCommandCategories: func(ctx context.Context, _ tea.Model) []commands.Category {
			return commands.BuildCommandCategories(ctx, initialApp)
		},
		supervisor:              sv,
		tabBar:                  tb,
		tuiStore:                ts,
		chatPages:               map[string]chat.Page{},
		editors:                 map[string]editor.Editor{},
		sessionStates:           map[string]*service.SessionState{sessID: initialSessionState},
		application:             initialApp,
		sessionState:            initialSessionState,
		history:                 historyStore,
		pendingRestores:         make(map[string]string),
		pendingSidebarCollapsed: make(map[string]bool),
		stashedDialogs:          make(map[string]stashedDialog),
		notification:            notification.New(),
		dialogMgr:               dialog.New(),
		completions:             completion.New(),
		transcriber:             transcribe.New(os.Getenv("OPENAI_API_KEY")),
		workingSpinner:          spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle),
		focusedPanel:            PanelEditor,
		editorLines:             3,
		dockerDesktop:           os.Getenv("TERM_PROGRAM") == "docker_desktop",
		appName:                 "docker agent",
		appVersion:              version.Version,
	}

	// Apply options
	for _, opt := range opts {
		opt(m)
	}

	// Create initial editor (after options are applied so command builder is set)
	initialEditor := editor.New(historyStore, m.editorOpts()...)
	m.editors[sessID] = initialEditor
	m.editor = initialEditor

	// Create initial chat page (after options are applied so leanMode is set)
	initialChatPage := chat.New(initialApp, initialSessionState, m.chatPageOpts()...)
	m.chatPages[sessID] = initialChatPage
	m.chatPage = initialChatPage

	// Initialize status bar (pass m as help provider)
	m.statusBar = statusbar.New(m, statusbar.WithTitle(m.appName+" "+m.appVersion))

	// Add the initial session to the supervisor
	sv.AddSession(ctx, initialApp, initialApp.Session(), initialWorkingDir, cleanup)

	// Restore persisted tabs or persist the initial one.
	m.restoreTabs(ctx, ts, sv, spawner, initialApp, sessID, initialWorkingDir)

	// Initialize tab bar with current tabs
	tabs, activeIdx := sv.GetTabs()
	tb.SetTabs(tabs, activeIdx)
	m.statusBar.SetShowNewTab(tb.Height() == 0)

	// Make sure to stop on context cancellation.
	// Note: chatPages/editors cleanup is handled by cleanupAll() on the
	// normal exit path (ExitConfirmedMsg). We don't iterate those maps
	// here to avoid racing with the Bubble Tea event loop.
	go func() {
		<-ctx.Done()
		if ts != nil {
			_ = ts.Close()
		}
		sv.Shutdown()
	}()

	return m
}

// Resolve implements dependency resolution for the appModel.
// See core.Resolve for additional information.
func (m *appModel) Resolve(v any) any {
	switch v.(type) {
	case **app.App:
		return m.application
	case **service.SessionState:
		return m.sessionState
	case *chat.Page:
		return m.chatPage
	case *editor.Editor:
		return m.editor
	}

	return nil
}

// SetProgram sets the tea.Program for the supervisor to send routed messages.
func (m *appModel) SetProgram(p *tea.Program) {
	m.program = p
	m.supervisor.SetProgram(p)
}

// reapplyKeyboardEnhancements forwards the cached keyboard enhancements message
// to the active chat page and editor so new/replaced instances pick up the
// terminal's key disambiguation support.
func (m *appModel) reapplyKeyboardEnhancements() {
	if m.keyboardEnhancements == nil {
		return
	}
	_ = m.updateChatCmd(*m.keyboardEnhancements)
	_ = m.updateEditorCmd(*m.keyboardEnhancements)
}

func (m *appModel) commandCategories() []commands.Category {
	return m.buildCommandCategories(context.Background(), m)
}

// chatPageOpts returns the chat.PageOption slice derived from the current
// appModel configuration (e.g. lean mode).
func (m *appModel) chatPageOpts() []chat.PageOption {
	opts := []chat.PageOption{
		chat.WithCommandParser(commands.NewParser(m.commandCategories()...)),
	}

	if m.leanMode {
		opts = append(opts, chat.WithLeanMode())
	}
	return opts
}

// editorOpts returns the editor.Option slice derived from the current appModel.
func (m *appModel) editorOpts() []editor.Option {
	return []editor.Option{
		editor.WithCompletions(
			completions.NewCommandCompletion(m.commandCategories()),
			completions.NewFileCompletion(),
		),
	}
}

// initSessionComponents creates a new chat page, session state, and editor for
// the given app and stores them in the per-session maps under tabID. The active
// convenience pointers (m.chatPage, m.sessionState, m.editor) are also updated.
func (m *appModel) initSessionComponents(tabID string, a *app.App, sess *session.Session) {
	ss := service.NewSessionState(sess)
	cp := chat.New(a, ss, m.chatPageOpts()...)
	ed := editor.New(m.history, m.editorOpts()...)

	m.chatPages[tabID] = cp
	m.sessionStates[tabID] = ss
	m.editors[tabID] = ed

	m.application = a
	m.sessionState = ss
	m.chatPage = cp
	m.editor = ed
}

// initAndFocusComponents returns a batch of commands that initializes and focuses
// the active chat page and editor, then resizes everything.
func (m *appModel) initAndFocusComponents() tea.Cmd {
	m.reapplyKeyboardEnhancements()
	return tea.Batch(
		m.chatPage.Init(),
		m.editor.Init(),
		m.editor.Focus(),
		m.resizeAll(),
	)
}

// Init initializes the model.
func (m *appModel) Init() tea.Cmd {
	// If a different tab should be active on startup, switch to it directly.
	// The initial tab's pending restore stays lazy — it will be loaded via
	// handleSwitchTab when the user eventually opens it, just like every
	// other non-active restored tab.
	if m.pendingActiveTab != "" {
		tabID := m.pendingActiveTab
		m.pendingActiveTab = ""
		_, switchCmd := m.handleSwitchTab(tabID)
		return tea.Batch(m.dialogMgr.Init(), switchCmd)
	}

	// If the initial tab has a pending session restore, go through
	// replaceActiveSession — the same code path as the /sessions command.
	activeID := m.supervisor.ActiveID()
	if oldSessionID, ok := m.pendingRestores[activeID]; ok {
		delete(m.pendingRestores, activeID)
		if store := m.application.SessionStore(); store != nil {
			if sess, err := store.GetSession(context.Background(), oldSessionID); err == nil {
				_, cmd := m.replaceActiveSession(context.Background(), sess)

				if m.tuiStore != nil && sess.WorkingDir != "" {
					if err := m.tuiStore.UpdateTabWorkingDir(context.Background(), oldSessionID, sess.WorkingDir); err != nil {
						slog.Warn("Failed to update persisted working dir", "error", err)
					}
				}

				cmd = tea.Batch(cmd, m.applySidebarCollapsed(activeID))
				m.persistActiveTab(sess.ID)

				return tea.Batch(m.dialogMgr.Init(), cmd)
			}
		}
	}

	return tea.Batch(
		m.dialogMgr.Init(),
		m.chatPage.Init(),
		m.editor.Init(),
		m.editor.Focus(),
		m.application.SendFirstMessage(),
	)
}

// Update handles messages.
func (m *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// In lean mode, silently drop messages for features that don't exist.
	if m.leanMode {
		switch msg.(type) {
		case messages.SpawnSessionMsg, messages.SwitchTabMsg,
			messages.CloseTabMsg, messages.ReorderTabMsg,
			messages.ToggleSidebarMsg:
			return m, nil
		}
	}

	switch msg := msg.(type) {
	// --- Routing & Animation ---

	case messages.RoutedMsg:
		return m.handleRoutedMsg(msg)

	case animation.TickMsg:
		// Drop the tick (and let the chain die) while we're blurred.
		// animation.StartTick re-arms the chain on the next FocusMsg so
		// spinners resume immediately when the user comes back.
		if m.tickPaused {
			return m, nil
		}
		cmds := []tea.Cmd{m.updateChatCmd(msg)}
		// Update working spinner
		if m.chatPage.IsWorking() {
			model, cmd := m.workingSpinner.Update(msg)
			m.workingSpinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
		}
		// Track frame for window-title spinner (tmux activity detection)
		m.animFrame = msg.Frame
		// Forward frame to tab bar for running indicator animation
		m.tabBar.SetAnimFrame(msg.Frame)
		if animation.HasActive() {
			cmds = append(cmds, animation.StartTick())
		}
		return m, tea.Batch(cmds...)

	// --- Tab management ---

	case messages.TabsUpdatedMsg:
		prevHeight := m.tabBar.Height()
		m.tabBar.SetTabs(msg.Tabs, msg.ActiveIdx)
		m.statusBar.SetShowNewTab(m.tabBar.Height() == 0)
		if m.tabBar.Height() != prevHeight {
			cmd := m.resizeAll()
			return m, cmd
		}
		return m, nil

	case messages.SpawnSessionMsg:
		return m.handleSpawnSession(msg.WorkingDir)

	case messages.SwitchTabMsg:
		return m.handleSwitchTab(msg.SessionID)

	case messages.CloseTabMsg:
		return m.handleCloseTab(msg.SessionID)

	case messages.ReorderTabMsg:
		return m.handleReorderTab(msg)

	case messages.ToggleSidebarMsg:
		if m.tuiStore != nil {
			persistedID := m.persistedSessionID(m.supervisor.ActiveID())
			if err := m.tuiStore.ToggleSidebarCollapsed(context.Background(), persistedID); err != nil {
				slog.Warn("Failed to persist sidebar collapsed state", "error", err)
			}
		}
		return m, nil

	// --- Focus requests from content view ---

	case messages.RequestFocusMsg:
		switch msg.Target {
		case messages.PanelMessages:
			if m.focusedPanel != PanelContent {
				m.focusedPanel = PanelContent
				m.statusBar.InvalidateCache()
				m.editor.Blur()
			}
			if msg.ClickX != 0 || msg.ClickY != 0 {
				return m, m.chatPage.FocusMessageAt(msg.ClickX, msg.ClickY)
			}
			return m, m.chatPage.FocusMessages()
		case messages.PanelSidebarTitle:
			if m.focusedPanel != PanelContent {
				m.focusedPanel = PanelContent
				m.statusBar.InvalidateCache()
				m.chatPage.BlurMessages()
				m.editor.Blur()
			}
			return m, nil
		case messages.PanelEditor:
			if m.focusedPanel != PanelEditor {
				m.focusedPanel = PanelEditor
				m.statusBar.InvalidateCache()
				m.chatPage.BlurMessages()
				return m, m.editor.Focus()
			}
		}
		return m, nil

	// --- Working state from content view ---

	case messages.WorkingStateChangedMsg:
		return m.handleWorkingStateChanged(msg)

	// --- Statusbar invalidation ---

	case messages.InvalidateStatusBarMsg:
		m.statusBar.InvalidateCache()
		return m, nil

	// --- Window / Terminal ---

	case tea.WindowSizeMsg:
		m.wWidth, m.wHeight = msg.Width, msg.Height
		cmd := m.handleWindowResize(msg.Width, msg.Height)
		return m, cmd

	case tea.BlurMsg:
		m.focused = false
		m.tickPaused = true
		return m, nil

	case tea.FocusMsg:
		// Filter spurious FocusMsg: RestoreTerminal re-enables focus
		// reporting which delivers a FocusMsg even when we never blurred.
		if m.focused {
			return m, nil
		}
		m.focused = true

		var cmds []tea.Cmd
		if m.tickPaused {
			// Re-arm the tick chain that died while we were blurred.
			m.tickPaused = false
			if animation.HasActive() {
				cmds = append(cmds, animation.StartTick())
			}
		}
		if m.dockerDesktop && m.program != nil {
			// Docker Desktop: the terminal may have lost all mode state (alt
			// screen, mouse tracking, keyboard enhancements, background
			// color, etc.). A full release/restore cycle re-emits every mode
			// sequence and forces a complete repaint.
			cmds = append(cmds, func() tea.Msg {
				_ = m.program.ReleaseTerminal()
				_ = m.program.RestoreTerminal()
				return nil
			})
		}
		return m, tea.Batch(cmds...)

	case tea.KeyboardEnhancementsMsg:
		m.keyboardEnhancements = &msg
		m.keyboardEnhancementsSupported = msg.Flags != 0
		return m, tea.Batch(m.updateChatCmd(msg), m.updateEditorCmd(msg))

	// --- Keyboard input ---

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)

	case tea.PasteMsg:
		if m.dialogMgr.Open() {
			return m.forwardDialog(msg)
		}
		// When inline editing a past message, forward paste to the chat page
		// so the messages component can insert content into the inline textarea.
		if m.chatPage.IsInlineEditing() {
			return m.forwardChat(msg)
		}
		// Forward paste to editor
		return m.forwardEditor(msg)

	// --- Mouse ---

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)

	case messages.WheelCoalescedMsg:
		return m.handleWheelCoalesced(msg)

	// --- Dialog lifecycle ---

	case dialog.OpenDialogMsg, dialog.CloseDialogMsg:
		return m.forwardDialog(msg)

	case dialog.ExitConfirmedMsg:
		m.cleanupAll()
		return m, tea.Quit

	case dialog.RuntimeResumeMsg:
		m.application.Resume(msg.Request)
		return m, nil

	case dialog.MultiChoiceResultMsg:
		if msg.DialogID == dialog.ToolRejectionDialogID {
			if msg.Result.IsCancelled {
				return m, nil
			}
			resumeMsg := dialog.HandleToolRejectionResult(msg.Result)
			if resumeMsg != nil {
				return m, tea.Sequence(
					core.CmdHandler(dialog.CloseDialogMsg{}),
					core.CmdHandler(*resumeMsg),
				)
			}
		}
		return m, nil

	// --- Terminal bell ---

	case messages.BellMsg:
		// Ring the terminal bell to alert the user that an inactive tab needs attention.
		// The BEL character (\a) is written to stderr which is typically the terminal.
		_, _ = fmt.Fprint(os.Stderr, "\a")
		return m, nil

	// --- Notifications ---

	case notification.ShowMsg, notification.HideMsg:
		updated, cmd := m.notification.Update(msg)
		m.notification = updated
		return m, cmd

	// --- Runtime event specializations ---

	case *runtime.TeamInfoEvent:
		m.sessionState.SetAvailableAgents(msg.AvailableAgents)
		m.sessionState.SetCurrentAgentName(msg.CurrentAgent)
		return m.forwardChat(msg)

	case *runtime.AgentInfoEvent:
		m.sessionState.SetCurrentAgentName(msg.AgentName)
		m.application.TrackCurrentAgentModel(msg.Model)
		return m.forwardChat(msg)

	case *runtime.SessionTitleEvent:
		m.sessionState.SetSessionTitle(msg.Title)
		return m.forwardChat(msg)

	// --- New session (slash command /new) ---

	case messages.NewSessionMsg:
		// /new spawns a new tab when a session spawner is configured.
		return m.handleSpawnSession("")

	case messages.ClearSessionMsg:
		// /clear resets the current tab with a fresh session in the same working dir.
		return m.handleClearSession()

	// --- Exit ---

	case messages.ExitSessionMsg:
		// If multiple tabs are open, close only the current tab instead of
		// quitting the entire application (see #2373).
		if m.supervisor != nil && m.supervisor.Count() > 1 {
			return m.handleCloseTab(m.supervisor.ActiveID())
		}
		m.cleanupAll()
		return m, tea.Quit

	case messages.ExitAfterFirstResponseMsg:
		m.cleanupAll()
		return m, tea.Quit

	// --- SendMsg from editor ---

	case messages.SendMsg:
		// Forward send messages to the active content view
		if m.history != nil {
			_ = m.history.Add(msg.Content)
		}
		return m.forwardChat(msg)

	// --- File attachments (routed to editor) ---

	case messages.InsertFileRefMsg:
		if err := m.editor.AttachFile(msg.FilePath); err != nil {
			slog.Warn("failed to attach file", "path", msg.FilePath, "error", err)
			return m, nil
		}
		return m, notification.SuccessCmd("File attached: " + msg.FilePath)

	// --- Agent management ---

	case messages.SwitchAgentMsg:
		return m.handleSwitchAgent(msg.AgentName)

	// --- Session browser ---

	case messages.OpenSessionBrowserMsg:
		return m.handleOpenSessionBrowser()

	case messages.LoadSessionMsg:
		return m.handleLoadSession(msg.SessionID)

	case messages.BranchFromEditMsg:
		return m.handleBranchFromEdit(msg)

	case messages.ForkSessionMsg:
		return m.handleForkSession()

	// --- Session commands (slash commands, command palette) ---

	case messages.ToggleYoloMsg:
		return m.handleToggleYolo()

	case messages.TogglePauseMsg:
		return m.handleTogglePause()

	case messages.ToggleHideToolResultsMsg:
		return m.handleToggleHideToolResults()

	case messages.ToggleSplitDiffMsg:
		return m.handleToggleSplitDiff()

	case messages.ClearQueueMsg:
		return m.forwardChat(msg)

	case messages.CompactSessionMsg:
		return m.handleCompactSession(msg.AdditionalPrompt)

	case messages.CopySessionToClipboardMsg:
		return m.handleCopySessionToClipboard()

	case messages.CopyLastResponseToClipboardMsg:
		return m.handleCopyLastResponseToClipboard()

	case messages.UndoSnapshotMsg:
		return m.handleUndoSnapshot()

	case messages.ShowSnapshotsDialogMsg:
		return m.handleShowSnapshotsDialog()

	case messages.ResetSnapshotMsg:
		return m.handleResetSnapshot(msg.Keep)

	case messages.EvalSessionMsg:
		return m.handleEvalSession(msg.Filename)

	case messages.ExportSessionMsg:
		return m.handleExportSession(msg.Filename)

	case messages.ToggleSessionStarMsg:
		sessionID := msg.SessionID
		if sessionID == "" {
			if sess := m.application.Session(); sess != nil {
				sessionID = sess.ID
			} else {
				return m, nil
			}
		}
		return m.handleToggleSessionStar(sessionID)

	case messages.DeleteSessionMsg:
		return m.handleDeleteSession(msg.SessionID)

	case messages.SetSessionTitleMsg:
		return m.handleSetSessionTitle(msg.Title)

	case messages.RegenerateTitleMsg:
		return m.handleRegenerateTitle()

	case messages.ShowCostDialogMsg:
		return m.handleShowCostDialog()

	case messages.ShowPermissionsDialogMsg:
		return m.handleShowPermissionsDialog()

	case messages.ShowToolsDialogMsg:
		return m.handleShowToolsDialog()

	case messages.RestartToolsetMsg:
		return m.handleRestartToolset(msg.Name)

	case messages.AgentCommandMsg:
		return m.handleAgentCommand(msg.Command)

	case messages.StartShellMsg:
		return m.startShell()

	// --- Model picker ---

	case messages.OpenModelPickerMsg:
		return m.handleOpenModelPicker()

	case messages.ChangeModelMsg:
		return m.handleChangeModel(msg.ModelRef)

	// --- Theme picker ---

	case messages.OpenThemePickerMsg:
		return m.handleOpenThemePicker()

	case messages.ChangeThemeMsg:
		return m.handleChangeTheme(msg.ThemeRef)

	case messages.ThemePreviewMsg:
		return m.handleThemePreview(msg.ThemeRef)

	case messages.ThemeCancelPreviewMsg:
		return m.handleThemeCancelPreview(msg.OriginalRef)

	case messages.ThemeChangedMsg:
		return m.applyThemeChanged()

	case messages.ThemeFileChangedMsg:
		return m.handleThemeFileChanged(msg.ThemeRef)

	// --- Speech-to-text ---

	case messages.StartSpeakMsg:
		if !m.transcriber.IsSupported() {
			return m, notification.InfoCmd("Speech-to-text is only supported on macOS")
		}
		return m.handleStartSpeak()

	case messages.StopSpeakMsg:
		return m.handleStopSpeak()

	case messages.SpeakTranscriptMsg:
		m.editor.InsertText(msg.Delta)
		cmd := m.waitForTranscript()
		return m, cmd

	// --- MCP prompts ---

	case messages.ShowMCPPromptInputMsg:
		return m.handleShowMCPPromptInput(msg.PromptName, msg.PromptInfo)

	case messages.MCPPromptMsg:
		return m.handleMCPPrompt(msg.PromptName, msg.Arguments)

	// --- File attachments ---

	case messages.AttachFileMsg:
		return m.handleAttachFile(msg.FilePath)

	case messages.SendAttachmentMsg:
		m.application.RunWithMessage(context.Background(), nil, msg.Content)
		return m, nil

	// --- URL opening ---

	case messages.OpenURLMsg:
		return m.handleOpenURL(msg.URL)

	// --- Elicitation ---

	case messages.ElicitationResponseMsg:
		return m.handleElicitationResponse(msg.Action, msg.Content)

	// --- Errors ---

	case error:
		m.err = msg
		return m, nil

	default:
		// Handle runtime events for active session
		if event, isRuntimeEvent := msg.(runtime.Event); isRuntimeEvent {
			if agentName := event.GetAgentName(); agentName != "" {
				m.sessionState.SetCurrentAgentName(agentName)
			}
			return m.forwardChat(msg)
		}

		// Forward to dialog if open (and to chat in parallel)
		if m.dialogMgr.Open() {
			return m, tea.Batch(m.updateDialogCmd(msg), m.updateChatCmd(msg))
		}

		// Forward to completion manager, editor, and chat page in parallel
		return m, tea.Batch(m.updateCompletionsCmd(msg), m.updateEditorCmd(msg), m.updateChatCmd(msg))
	}
}

// handleRoutedMsg processes messages routed to specific sessions.
func (m *appModel) handleRoutedMsg(msg messages.RoutedMsg) (tea.Model, tea.Cmd) {
	activeID := m.supervisor.ActiveID()

	if msg.SessionID == activeID {
		// Active session: forward through Update for full processing (spinners, cmds, etc.)
		return m.Update(msg.Inner)
	}

	// Background session: update its chat page directly so streaming content accumulates.
	// UI-only cmds (spinners, scroll) are discarded since the page isn't visible.
	chatPage, ok := m.chatPages[msg.SessionID]
	if !ok {
		return m, nil
	}

	// Update session state for inactive sessions
	if event, isRuntimeEvent := msg.Inner.(runtime.Event); isRuntimeEvent {
		if sessionState, ok := m.sessionStates[msg.SessionID]; ok {
			if agentName := event.GetAgentName(); agentName != "" {
				sessionState.SetCurrentAgentName(agentName)
			}
		}
	}

	// Update the inactive chat page (discard cmds — UI effects aren't needed for hidden pages)
	updated, _ := chatPage.Update(msg.Inner)
	m.chatPages[msg.SessionID] = updated.(chat.Page)
	return m, nil
}

// handleWorkingStateChanged updates the editor working indicator and resize handle spinner.
func (m *appModel) handleWorkingStateChanged(msg messages.WorkingStateChangedMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Update editor working state
	cmds = append(cmds, m.editor.SetWorking(msg.Working))

	// Start/stop working spinner
	if msg.Working {
		cmds = append(cmds, m.workingSpinner.Init())
	} else {
		m.workingSpinner.Stop()
	}

	return m, tea.Batch(cmds...)
}

// handleOpenSessionBrowser opens the session browser dialog.
func (m *appModel) handleOpenSessionBrowser() (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.InfoCmd("No session store configured")
	}

	sessions, err := store.GetSessionSummaries(context.Background())
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load sessions: %v", err))
	}
	if len(sessions) == 0 {
		return m, notification.InfoCmd("No previous sessions found")
	}

	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSessionBrowserDialog(sessions),
	})
}

// handleLoadSession loads a saved session into the current tab (if empty) or a new tab.
func (m *appModel) handleLoadSession(sessionID string) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}

	sess, err := store.GetSession(context.Background(), sessionID)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load session: %v", err))
	}

	// Check if this session is already open in another tab — switch instead of duplicating.
	if tabID := m.findTabByPersistedID(sessionID); tabID != "" {
		return m.handleSwitchTab(tabID)
	}

	// Determine working directory from the loaded session.
	workingDir := sess.WorkingDir
	if workingDir == "" {
		workingDir = m.application.Session().WorkingDir
	}
	ctx := context.Background()

	// If the current session is empty (no messages, no title — the default state
	// when opening the TUI or creating a new tab), replace it in-place instead of
	// spawning yet another tab.
	currentSess := m.application.Session()
	if len(currentSess.Messages) == 0 && currentSess.Title == "" {
		activeID := m.supervisor.ActiveID()
		oldPersistedID := m.persistedSessionID(activeID)

		model, cmd := m.replaceActiveSession(ctx, sess)

		// Update tuistate: replace old persisted ID with the loaded session's ID
		if m.tuiStore != nil {
			if err := m.tuiStore.UpdateTabSessionID(ctx, oldPersistedID, sess.ID); err != nil {
				slog.WarnContext(ctx, "Failed to update tab session ID after in-place load", "error", err)
			}
			if sess.WorkingDir != "" {
				if err := m.tuiStore.UpdateTabWorkingDir(ctx, sess.ID, sess.WorkingDir); err != nil {
					slog.WarnContext(ctx, "Failed to update tab working dir after in-place load", "error", err)
				}
			}
		}
		m.persistActiveTab(sess.ID)
		return model, cmd
	}

	slog.DebugContext(ctx, "Loading session into new tab", "session_id", sessionID)

	// Spawn a new tab.
	newSessionID, err := m.supervisor.SpawnSession(ctx, workingDir)
	if err != nil {
		return m, notification.ErrorCmd("Failed to create tab: " + err.Error())
	}

	// Persist the new tab using the loaded session's persisted ID (not the ephemeral tab ID).
	if m.tuiStore != nil {
		if err := m.tuiStore.AddTab(ctx, sess.ID, workingDir); err != nil {
			slog.WarnContext(ctx, "Failed to persist loaded session tab", "error", err)
		}
	}

	// Switch to the new tab so m.application points to the new app.
	model, switchCmd := m.handleSwitchTab(newSessionID)

	// Replace the blank session with the loaded one and rebuild all components.
	m.application.ReplaceSession(ctx, sess)
	m.initSessionComponents(newSessionID, m.application, sess)

	if sess.Title != "" {
		m.supervisor.SetRunnerTitle(newSessionID, sess.Title)
	}

	m.persistActiveTab(sess.ID)

	return model, tea.Batch(
		switchCmd,
		m.initAndFocusComponents(),
	)
}

// replaceActiveSession replaces the current (empty) tab's session with a loaded one in-place.
// If the loaded session's working directory differs from the runner's current one,
// a fresh runtime is spawned via the supervisor so that tools operate in the correct directory.
func (m *appModel) replaceActiveSession(ctx context.Context, sess *session.Session) (tea.Model, tea.Cmd) {
	activeID := m.supervisor.ActiveID()

	slog.DebugContext(ctx, "Replacing empty session in-place", "tab_id", activeID, "loaded_session", sess.ID)

	// Cleanup old editor for the active session
	if ed, ok := m.editors[activeID]; ok {
		ed.Cleanup()
	}

	// If the loaded session's working directory differs from the runner's,
	// we need a fresh runtime whose tools operate in the correct directory.
	runner := m.supervisor.GetRunner(activeID)
	sessWorkingDir := sess.WorkingDir
	if sessWorkingDir != "" && runner != nil && sessWorkingDir != runner.WorkingDir {
		newApp, _, spawnCleanup, err := m.supervisor.Spawner()(ctx, sessWorkingDir)
		if err == nil {
			slog.DebugContext(ctx, "Respawning runtime for working dir mismatch",
				"tab_id", activeID,
				"old_dir", runner.WorkingDir,
				"new_dir", sessWorkingDir)
			m.supervisor.ReplaceRunnerApp(ctx, activeID, newApp, sessWorkingDir, spawnCleanup)
			m.application = newApp
		} else {
			slog.WarnContext(ctx, "Failed to respawn runtime for working dir, using existing",
				"working_dir", sessWorkingDir, "error", err)
		}
	}

	// Replace the session in the app and rebuild all per-session components.
	m.application.ReplaceSession(ctx, sess)
	m.initSessionComponents(activeID, m.application, sess)

	if sess.Title != "" {
		m.supervisor.SetRunnerTitle(activeID, sess.Title)
	}

	cmd := m.initAndFocusComponents()
	return m, cmd
}

// handleClearSession resets the current tab by creating a fresh session
// in the same working directory.
func (m *appModel) handleClearSession() (tea.Model, tea.Cmd) {
	activeID := m.supervisor.ActiveID()

	// Cleanup old editor for the active session.
	if ed, ok := m.editors[activeID]; ok {
		ed.Cleanup()
	}

	// Create a fresh session in the same app, preserving the working dir.
	m.application.NewSession()
	newSess := m.application.Session()

	// Rebuild all per-session UI components.
	m.initSessionComponents(activeID, m.application, newSess)
	m.dialogMgr = dialog.New()
	m.supervisor.SetRunnerTitle(activeID, "")
	m.sessionState.SetSessionTitle("")
	m.sessionState.SetPreviousMessage(nil)

	// Update persisted tab to point to the new session.
	if m.tuiStore != nil {
		ctx := context.Background()
		oldPersistedID := m.persistedSessionID(activeID)
		if err := m.tuiStore.UpdateTabSessionID(ctx, oldPersistedID, newSess.ID); err != nil {
			slog.WarnContext(ctx, "Failed to update tab session ID after clear", "error", err)
		}
	}
	m.persistActiveTab(newSess.ID)

	m.reapplyKeyboardEnhancements()

	return m, tea.Sequence(
		m.chatPage.Init(),
		m.resizeAll(),
		m.editor.Focus(),
	)
}

// handleSpawnSession spawns a new session.
func (m *appModel) handleSpawnSession(workingDir string) (tea.Model, tea.Cmd) {
	// If no working dir specified, open the picker
	if workingDir == "" {
		return m.openWorkingDirPicker()
	}

	// Spawn the new session
	ctx := context.Background()
	sessionID, err := m.supervisor.SpawnSession(ctx, workingDir)
	if err != nil {
		return m, notification.ErrorCmd("Failed to spawn session: " + err.Error())
	}

	// Persist the new tab (for new tabs, persisted ID == runtime tab ID).
	if m.tuiStore != nil {
		if err := m.tuiStore.AddTab(ctx, sessionID, workingDir); err != nil {
			slog.WarnContext(ctx, "Failed to persist new tab", "error", err)
		}
	}

	// Switch to the new session
	return m.handleSwitchTab(sessionID)
}

// openWorkingDirPicker opens the working directory picker dialog.
func (m *appModel) openWorkingDirPicker() (tea.Model, tea.Cmd) {
	var recentDirs, favoriteDirs []string
	if m.tuiStore != nil {
		recentDirs, _ = m.tuiStore.GetRecentDirs(context.Background(), 10)
		favoriteDirs, _ = m.tuiStore.GetFavoriteDirs(context.Background())
	}

	// Use the active session's working directory so the picker reflects it
	// instead of the process CWD.
	var sessionWorkingDir string
	if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil {
		sessionWorkingDir = runner.WorkingDir
	}

	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewWorkingDirPickerDialog(recentDirs, favoriteDirs, m.tuiStore, sessionWorkingDir),
	})
}

// stashedDialog holds a background dialog instance that was on screen when
// the user navigated away from a tab, paired with the runtime event that
// caused it to open. The event is used as an identity check on return: if
// the supervisor's pending event for the tab no longer matches, the agent
// has superseded the prompt and we discard the stash in favour of building
// a fresh dialog from the new event.
type stashedDialog struct {
	dialog dialog.Dialog
	event  tea.Msg
}

// handleSwitchTab switches to a different session.
// Existing chat pages and editors are preserved (not recreated) so that in-flight streaming
// content and draft text are retained when switching back to a tab.
func (m *appModel) handleSwitchTab(sessionID string) (tea.Model, tea.Cmd) {
	// If a background dialog (e.g. pending elicitation) is open on the
	// outgoing tab, capture both its originating event and the live dialog
	// instance before the supervisor flips activeID. We only commit the
	// re-stash after SwitchTo succeeds — otherwise a failed switch would
	// leave the supervisor with a stale pending event and the dialog still
	// on screen.
	//
	// Stashing the dialog instance (rather than rebuilding it from the event
	// on return) preserves any in-progress input the user typed — e.g. text
	// already entered into a user_prompt elicitation. See issue #2770.
	var (
		backgroundEvent  tea.Msg
		backgroundDialog dialog.Dialog
		outgoingTabID    string
	)
	if m.dialogMgr.Open() && m.dialogMgr.TopIsBackground() {
		backgroundEvent = m.dialogMgr.TopBackgroundEvent()
		backgroundDialog = m.dialogMgr.TopDialog()
		outgoingTabID = m.supervisor.ActiveID()
	}

	runner := m.supervisor.SwitchTo(sessionID)
	if runner == nil {
		return m, notification.ErrorCmd("Session not found")
	}

	// Now that the switch is committed, finalize the dialog hand-off.
	var closeBackgroundDialogCmd tea.Cmd
	if backgroundEvent != nil && outgoingTabID != "" && outgoingTabID != sessionID {
		m.supervisor.SetPendingEvent(outgoingTabID, backgroundEvent)
		if backgroundDialog != nil {
			m.stashedDialogs[outgoingTabID] = stashedDialog{
				dialog: backgroundDialog,
				event:  backgroundEvent,
			}
		}
		closeBackgroundDialogCmd = core.CmdHandler(dialog.CloseDialogMsg{})
	}

	// Blur current editor before switching
	m.editor.Blur()

	// If this tab has a pending session restore, load it through
	// replaceActiveSession — the same code path as the /sessions command.
	if oldSessionID, ok := m.pendingRestores[sessionID]; ok {
		delete(m.pendingRestores, sessionID)
		m.application = runner.App
		if store := runner.App.SessionStore(); store != nil {
			if sess, err := store.GetSession(context.Background(), oldSessionID); err == nil {
				m.persistActiveTab(sess.ID)
				model, cmd := m.replaceActiveSession(context.Background(), sess)

				if m.tuiStore != nil && sess.WorkingDir != "" {
					if err := m.tuiStore.UpdateTabWorkingDir(context.Background(), oldSessionID, sess.WorkingDir); err != nil {
						slog.Warn("Failed to update persisted working dir", "error", err)
					}
				}

				cmd = tea.Batch(cmd, m.applySidebarCollapsed(sessionID), closeBackgroundDialogCmd)
				return model, cmd
			}
		}
		// Fall through to normal tab switch if session couldn't be loaded.
	}

	// Get or create per-session components.
	_, pageExists := m.chatPages[sessionID]
	_, editorExists := m.editors[sessionID]

	if !pageExists || !editorExists {
		// Create all missing components at once.
		m.initSessionComponents(sessionID, runner.App, runner.App.Session())
		m.applySidebarCollapsed(sessionID)
	} else {
		// Reuse existing components — just update convenience pointers.
		m.application = runner.App
		m.sessionState = m.sessionStates[sessionID]
		m.chatPage = m.chatPages[sessionID]
		m.editor = m.editors[sessionID]
	}

	m.reapplyKeyboardEnhancements()
	m.persistActiveTab(m.persistedSessionID(sessionID))

	// Sync editor working state and reset working spinner.
	m.editor.SetWorking(m.chatPage.IsWorking())
	m.workingSpinner.Stop()
	m.workingSpinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)

	var cmds []tea.Cmd

	if !pageExists || !editorExists {
		if !pageExists {
			cmds = append(cmds, m.chatPage.Init())
		}
		if !editorExists {
			cmds = append(cmds, m.editor.Init())
		}
		cmds = append(cmds, m.editor.Focus(), m.resizeAll())
	} else {
		cmds = append(cmds, m.resizeAll(), m.chatPage.ScrollToBottom(), m.editor.Focus())
	}

	if m.chatPage.IsWorking() {
		cmds = append(cmds, m.workingSpinner.Init())
	}
	if pendingCmd := m.replayPendingEvent(sessionID); pendingCmd != nil {
		cmds = append(cmds, pendingCmd)
	}
	if closeBackgroundDialogCmd != nil {
		cmds = append(cmds, closeBackgroundDialogCmd)
	}

	return m, tea.Batch(cmds...)
}

// applySidebarCollapsed applies and consumes the persisted sidebar collapsed state
// for the given tab ID. Returns a resize command if the state was applied, nil otherwise.
func (m *appModel) applySidebarCollapsed(sessionID string) tea.Cmd {
	collapsed, ok := m.pendingSidebarCollapsed[sessionID]
	if !ok {
		return nil
	}
	m.chatPage.SetSidebarSettings(chat.SidebarSettings{Collapsed: collapsed})
	delete(m.pendingSidebarCollapsed, sessionID)
	return m.resizeAll()
}

// replayPendingEvent checks if a session has a pending attention event (e.g. tool confirmation,
// max iterations, elicitation) that was received while the tab was inactive.
// If found, it opens the appropriate dialog. The event was already processed by the chat page
// (updating the message list), but the dialog command was discarded for inactive sessions.
//
// If a stashed dialog instance is available for this session and its
// associated event still matches the pending one, the same instance is
// re-opened so any in-progress input survives the round trip (issue #2770).
// Otherwise the stash is discarded and a fresh dialog is built.
func (m *appModel) replayPendingEvent(sessionID string) tea.Cmd {
	pendingEvent := m.supervisor.ConsumePendingEvent(sessionID)
	if pendingEvent == nil {
		// No pending event: any stash is stale (e.g. the agent finished).
		delete(m.stashedDialogs, sessionID)
		return nil
	}

	sessionState, ok := m.sessionStates[sessionID]
	if !ok {
		delete(m.stashedDialogs, sessionID)
		return nil
	}

	// If we stashed the live dialog instance when leaving this tab and the
	// pending event hasn't changed, re-open the same instance so the user's
	// in-progress input is preserved.
	if stash, ok := m.stashedDialogs[sessionID]; ok {
		delete(m.stashedDialogs, sessionID)
		if stash.event == pendingEvent && stash.dialog != nil {
			return core.CmdHandler(dialog.OpenDialogMsg{
				Model:            stash.dialog,
				OriginatingEvent: pendingEvent,
			})
		}
	}

	switch ev := pendingEvent.(type) {
	case *runtime.ToolCallConfirmationEvent:
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewToolConfirmationDialog(ev, sessionState),
			OriginatingEvent: ev,
		})

	case *runtime.MaxIterationsReachedEvent:
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewMaxIterationsDialog(ev.MaxIterations, m.application),
			OriginatingEvent: ev,
		})

	case *runtime.ElicitationRequestEvent:
		return m.replayElicitationEvent(ev)
	}

	return nil
}

// replayElicitationEvent opens the appropriate elicitation dialog for a pending event.
func (m *appModel) replayElicitationEvent(ev *runtime.ElicitationRequestEvent) tea.Cmd {
	// Check if this is an OAuth flow
	if ev.Meta != nil {
		if elicitationType, ok := ev.Meta["cagent/type"].(string); ok && elicitationType == "oauth_flow" {
			var serverURL string
			if url, ok := ev.Meta["cagent/server_url"].(string); ok {
				serverURL = url
			}
			return core.CmdHandler(dialog.OpenDialogMsg{
				Model:            dialog.NewOAuthAuthorizationDialog(serverURL, m.application),
				OriginatingEvent: ev,
			})
		}
	}

	switch ev.Mode {
	case "url":
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewURLElicitationDialog(ev.Message, ev.URL),
			OriginatingEvent: ev,
		})
	default:
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewElicitationDialog(ev.Message, ev.Schema, ev.Meta),
			OriginatingEvent: ev,
		})
	}
}

// handleReorderTab moves a tab from one position to another.
func (m *appModel) handleReorderTab(msg messages.ReorderTabMsg) (tea.Model, tea.Cmd) {
	m.supervisor.ReorderTab(msg.FromIdx, msg.ToIdx)

	if m.tuiStore != nil {
		tabs, _ := m.supervisor.GetTabs()
		ids := make([]string, len(tabs))
		for i, tab := range tabs {
			ids[i] = m.persistedSessionID(tab.SessionID)
		}
		if err := m.tuiStore.ReorderTab(context.Background(), ids); err != nil {
			slog.Warn("Failed to persist tab reorder", "error", err)
		}
	}

	return m, nil
}

// handleCloseTab closes a session tab.
func (m *appModel) handleCloseTab(sessionID string) (tea.Model, tea.Cmd) {
	wasActive := sessionID == m.supervisor.ActiveID()

	// Capture the working dir before closing so we can reuse it if this is the last tab.
	var closedWorkingDir string
	if runner := m.supervisor.GetRunner(sessionID); runner != nil {
		closedWorkingDir = runner.WorkingDir
	}

	// Compute persisted session-store ID *before* closing (runner goes away).
	persistedID := m.persistedSessionID(sessionID)

	nextActiveID := m.supervisor.CloseSession(sessionID)

	// Clean up per-session state
	delete(m.chatPages, sessionID)
	if ed, ok := m.editors[sessionID]; ok {
		ed.Cleanup()
		delete(m.editors, sessionID)
	}
	delete(m.sessionStates, sessionID)
	delete(m.pendingRestores, sessionID)
	delete(m.pendingSidebarCollapsed, sessionID)
	delete(m.stashedDialogs, sessionID)

	var cmds []tea.Cmd
	// Remove from persistent store using the persisted session-store ID.
	if m.tuiStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if err := m.tuiStore.RemoveTab(ctx, persistedID); err != nil {
			slog.ErrorContext(ctx, "Failed to remove tab from store", "error", err)
			cmds = append(cmds, notification.ErrorCmd(fmt.Sprintf("Failed to remove tab from tui state db: %v", err)))
		}
	}

	// If we closed all tabs, spawn a new one reusing the previous working dir.
	// We always provide a concrete dir to avoid showing the picker — pressing Esc
	// in the picker with zero tabs would leave the TUI in a broken state.
	if m.supervisor.Count() == 0 {
		workingDir := closedWorkingDir
		if workingDir == "" {
			workingDir, _ = os.Getwd()
		}
		if workingDir == "" {
			workingDir = "/"
		}
		return m.handleSpawnSession(workingDir)
	}

	// If the closed tab was active, switch to the next one
	if wasActive && nextActiveID != "" {
		return m.handleSwitchTab(nextActiveID)
	}

	return m, tea.Batch(cmds...)
}

// handleWindowResize handles window resize.
func (m *appModel) handleWindowResize(width, height int) tea.Cmd {
	m.wWidth, m.wHeight = width, height

	m.statusBar.SetWidth(width)
	m.tabBar.SetWidth(width - appPaddingHorizontal)

	m.width = width
	m.height = height

	if !m.ready {
		m.ready = true
	}

	return m.resizeAll()
}

// resizeAll recalculates all component sizes based on current window dimensions.
func (m *appModel) resizeAll() tea.Cmd {
	var cmds []tea.Cmd

	width, height := m.width, m.height
	innerWidth := width - appPaddingHorizontal

	// Calculate chrome height (everything that isn't content or editor)
	chromeHeight := 0
	if m.leanMode {
		if m.chatPage.IsWorking() {
			chromeHeight = 1 // working indicator line
		}
	} else {
		chromeHeight = m.tabBar.Height() + m.statusBar.Height() + 1 // +1 for resize handle
	}

	// Calculate editor height
	minLines := 4
	maxLines := max(minLines, (height-6)/2)
	m.editorLines = max(minLines, min(m.editorLines, maxLines))

	targetEditorHeight := m.editorLines - 1
	cmds = append(cmds, m.editor.SetSize(innerWidth, targetEditorHeight))
	_, editorHeight := m.editor.GetSize()
	// The editor's View() adds MarginBottom(1) which isn't included in GetSize(),
	// so account for it in the layout calculation.
	editorRenderedHeight := editorHeight + 1

	// Content gets remaining space
	m.contentHeight = max(1, height-chromeHeight-editorRenderedHeight)
	cmds = append(cmds, m.chatPage.SetSize(width, m.contentHeight))

	if m.leanMode {
		return tea.Batch(cmds...)
	}

	// Full mode: update overlay components
	cmds = append(cmds, m.updateDialogCmd(tea.WindowSizeMsg{Width: width, Height: height}))

	m.completions.SetEditorBottom(editorHeight + m.tabBar.Height())
	m.completions.Update(tea.WindowSizeMsg{Width: width, Height: height})

	m.notification.SetSize(width, height)

	return tea.Batch(cmds...)
}

// Help returns help information for the status bar.
func (m *appModel) Help() help.KeyMap {
	return core.NewSimpleHelp(m.Bindings())
}

// AllBindings returns ALL available key bindings for the help dialog (comprehensive list).
func (m *appModel) AllBindings() []key.Binding {
	quitBinding := key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("Ctrl+c", "quit"),
	)

	if m.leanMode {
		return []key.Binding{quitBinding}
	}

	tabBinding := key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "switch focus"),
	)

	bindings := []key.Binding{quitBinding, tabBinding}
	bindings = append(bindings, m.tabBar.Bindings()...)

	// Additional global shortcuts
	bindings = append(bindings,
		key.NewBinding(
			key.WithKeys("ctrl+k"),
			key.WithHelp("Ctrl+k", "commands"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+h"),
			key.WithHelp("Ctrl+h", "help"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+y"),
			key.WithHelp("Ctrl+y", "toggle yolo mode"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+o"),
			key.WithHelp("Ctrl+o", "toggle hide tool results"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("Ctrl+s", "cycle agent"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+m"),
			key.WithHelp("Ctrl+m", "model picker"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+x"),
			key.WithHelp("Ctrl+x", "clear queue"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+z"),
			key.WithHelp("Ctrl+z", "suspend"),
		),
	)

	if !m.leanMode {
		bindings = append(bindings, key.NewBinding(
			key.WithKeys("ctrl+b"),
			key.WithHelp("Ctrl+b", "toggle sidebar"),
		))
	}

	// Show newline help based on keyboard enhancement support
	if m.keyboardEnhancementsSupported {
		bindings = append(bindings, key.NewBinding(
			key.WithKeys("shift+enter"),
			key.WithHelp("Shift+Enter", "newline"),
		))
	} else {
		bindings = append(bindings, key.NewBinding(
			key.WithKeys("ctrl+j"),
			key.WithHelp("Ctrl+j", "newline"),
		))
	}

	if m.focusedPanel == PanelContent {
		bindings = append(bindings, m.chatPage.Bindings()...)
	} else {
		editorName := editorname.FromEnv(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
		bindings = append(bindings,
			key.NewBinding(
				key.WithKeys("ctrl+g"),
				key.WithHelp("Ctrl+g", "edit in "+editorName),
			),
			key.NewBinding(
				key.WithKeys("ctrl+r"),
				key.WithHelp("Ctrl+r", "history search"),
			),
		)
	}
	return bindings
}

// Bindings returns the key bindings shown in the status bar (a curated subset).
// This filters AllBindings() to show only the most essential commands.
func (m *appModel) Bindings() []key.Binding {
	all := m.AllBindings()

	// Define which keys should appear in the status bar
	statusBarKeys := map[string]bool{
		"ctrl+c":      true, // quit
		"tab":         true, // switch focus
		"ctrl+t":      true, // new tab (from tabBar)
		"ctrl+w":      true, // close tab (from tabBar)
		"ctrl+p":      true, // prev tab (from tabBar)
		"ctrl+n":      true, // next tab (from tabBar)
		"ctrl+k":      true, // commands
		"ctrl+h":      true, // help
		"shift+enter": true, // newline
		"ctrl+j":      true, // newline fallback
		"ctrl+g":      true, // edit in external editor (editor context)
		"ctrl+r":      true, // history search (editor context)
		// Content panel bindings (↑↓, c, e, d) are always included
		"up":   true,
		"down": true,
		"c":    true,
		"e":    true,
		"d":    true,
	}

	// Filter to only include status bar keys
	var filtered []key.Binding
	for _, binding := range all {
		if len(binding.Keys()) > 0 {
			bindingKey := binding.Keys()[0]
			if statusBarKeys[bindingKey] {
				filtered = append(filtered, binding)
			}
		}
	}

	return filtered
}

// handleKeyPress handles all keyboard input with proper priority routing.
func (m *appModel) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Check if we should stop transcription on Enter or Escape
	if m.transcriber.IsRunning() {
		switch msg.String() {
		case "enter":
			model, cmd := m.handleStopSpeak()
			sendCmd := m.editor.SendContent()
			return model, tea.Batch(cmd, sendCmd)

		case "esc":
			return m.handleStopSpeak()
		}
	}

	// Ctrl+c is intercepted before any dialog handling so that every dialog
	// reacts to it consistently:
	//   - With no dialog open: open the exit confirmation dialog.
	//   - With any other dialog open: stack the exit confirmation on top so
	//     that the user can confirm exit (a second ctrl+c or Y exits) or
	//     cancel it (N/Esc) and return to the original dialog.
	//   - With the exit confirmation already on top: forward the key so it
	//     can exit the program via its own Yes binding.
	if msg.String() == "ctrl+c" {
		if m.dialogMgr.TopIsExitConfirmation() {
			return m.forwardDialog(msg)
		}
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewExitConfirmationDialog(),
		})
	}

	// Dialog gets priority when open, EXCEPT for background dialogs (e.g.
	// pending elicitations) which let tab-navigation keys keep working so
	// the user can switch to another conversation while the prompt waits.
	if m.dialogMgr.Open() {
		if m.dialogMgr.TopIsBackground() && !m.leanMode && !m.editor.IsHistorySearchActive() {
			m.tabBar.SetCloseTabEnabled(true)
			if cmd := m.tabBar.Update(msg); cmd != nil {
				return m, cmd
			}
		}
		return m.forwardDialog(msg)
	}

	// Tab bar keys (Ctrl+t, Ctrl+p, Ctrl+n, Ctrl+w) are suppressed during
	// history search so that ctrl+n/ctrl+p cycle through matches instead.
	// Ctrl+w (close tab) is disabled when the editor is focused so that the
	// standard "delete word" shortcut works while typing.
	if !m.leanMode && !m.editor.IsHistorySearchActive() {
		m.tabBar.SetCloseTabEnabled(m.focusedPanel != PanelEditor)
		if cmd := m.tabBar.Update(msg); cmd != nil {
			return m, cmd
		}
	}

	// Completion popup gets priority when open
	if m.completions.Open() {
		if core.IsNavigationKey(msg) {
			return m.forwardCompletions(msg)
		}
		// For all other keys (typing), send to both completion (for filtering) and editor
		return m, tea.Batch(m.updateCompletionsCmd(msg), m.updateEditorCmd(msg))
	}

	// Global keyboard shortcuts (active even during history search)
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+z"))):
		return m, tea.Suspend

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+k"))):
		categories := m.commandCategories()
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewCommandPaletteDialog(categories),
		})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+y"))):
		return m, core.CmdHandler(messages.ToggleYoloMsg{})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+o"))):
		return m, core.CmdHandler(messages.ToggleHideToolResultsMsg{})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return m.handleCycleAgent()

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+m"))):
		return m.handleOpenModelPicker()

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+x"))):
		return m, core.CmdHandler(messages.ClearQueueMsg{})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+h", "f1", "ctrl+?"))):
		// Show contextual help dialog with ALL available key bindings
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewHelpDialog(m.AllBindings()),
		})
	}

	// History search is a modal state — capture all remaining keys before normal routing
	if m.focusedPanel == PanelEditor && m.editor.IsHistorySearchActive() {
		return m.forwardEditor(msg)
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+g"))):
		return m.openExternalEditor()

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+r"))):
		if m.focusedPanel == PanelEditor && !m.editor.IsRecording() {
			model, cmd := m.editor.EnterHistorySearch()
			m.editor = model.(editor.Editor)
			return m, cmd
		}

	// Toggle sidebar (propagates to content view regardless of focus)
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+b"))):
		if m.leanMode {
			return m, nil
		}
		return m.forwardChat(msg)

	// Focus switching: Tab key toggles between content and editor
	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		return m.switchFocus()

	// Esc: cancel stream (works regardless of focus)
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		// Forward to content view for stream cancellation
		return m.forwardChat(msg)

	default:
		// Handle ctrl+1 through ctrl+9 for quick agent switching
		if index := parseCtrlNumberKey(msg); index >= 0 {
			return m.handleSwitchToAgentByIndex(index)
		}
	}

	// Focus-based routing
	switch m.focusedPanel {
	case PanelEditor:
		return m.forwardEditor(msg)
	case PanelContent:
		return m.forwardChat(msg)
	}

	return m, nil
}

// parseCtrlNumberKey checks if msg is ctrl+1 through ctrl+9 and returns the index (0-8), or -1 if not matched
func parseCtrlNumberKey(msg tea.KeyPressMsg) int {
	s := msg.String()
	if len(s) == 6 && s[:5] == "ctrl+" && s[5] >= '1' && s[5] <= '9' {
		return int(s[5] - '1')
	}
	return -1
}

// switchFocus toggles between content and editor panels.
func (m *appModel) switchFocus() (tea.Model, tea.Cmd) {
	switch m.focusedPanel {
	case PanelEditor:
		// Check if editor has a suggestion to accept first
		if cmd := m.editor.AcceptSuggestion(); cmd != nil {
			return m, cmd
		}
		m.focusedPanel = PanelContent
		m.statusBar.InvalidateCache()
		m.editor.Blur()
		return m, m.chatPage.FocusMessages()
	case PanelContent:
		m.focusedPanel = PanelEditor
		m.statusBar.InvalidateCache()
		m.chatPage.BlurMessages()
		return m, m.editor.Focus()
	}
	return m, nil
}

// handleMouseClick routes mouse clicks to the appropriate component based on Y coordinate.
func (m *appModel) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	// Check if click hits a notification close button
	if cmd := m.notification.HandleClick(msg.X, msg.Y); cmd != nil {
		return m, cmd
	}

	// Dialogs use full-window coordinates (they're positioned over the entire screen)
	if m.dialogMgr.Open() {
		// Background dialogs (e.g. pending elicitations) let tab-bar clicks
		// pass through so the user can keep navigating between tabs.
		if m.dialogMgr.TopIsBackground() && !m.leanMode && m.hitTestRegion(msg.Y) == regionTabBar {
			adjustedMsg := msg
			adjustedMsg.X = msg.X - styles.AppPadding
			adjustedMsg.Y = msg.Y - m.contentHeight - 1
			if cmd := m.tabBar.Update(adjustedMsg); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		return m.forwardDialog(msg)
	}

	region := m.hitTestRegion(msg.Y)

	switch region {
	case regionContent:
		return m.forwardChat(msg)

	case regionResizeHandle:
		if msg.Button == tea.MouseLeft {
			m.isDragging = true
		}
		return m, nil

	case regionTabBar:
		// Adjust coordinates for tab bar (relative to its start, accounting for padding)
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		adjustedMsg.Y = msg.Y - m.contentHeight - 1
		if cmd := m.tabBar.Update(adjustedMsg); cmd != nil {
			return m, cmd
		}
		return m, nil

	case regionEditor:
		// Focus editor on click
		if m.focusedPanel != PanelEditor {
			m.focusedPanel = PanelEditor
			m.statusBar.InvalidateCache()
			m.chatPage.BlurMessages()
		}
		// Adjust coordinates for editor padding
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		adjustedMsg.Y = msg.Y - m.editorTop()
		return m, tea.Batch(m.updateEditorCmd(adjustedMsg), m.editor.Focus())

	case regionStatusBar:
		if msg.Button == tea.MouseLeft && m.statusBar.ClickedNewTab(msg.X) {
			return m.handleSpawnSession("")
		}
	}

	return m, nil
}

// handleMouseMotion routes mouse motion events with adjusted coordinates.
func (m *appModel) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if m.isDragging {
		cmd := m.handleEditorResize(msg.Y)
		return m, cmd
	}

	// Forward drag motion to tab bar when a tab drag is active.
	if m.tabBar.IsDragging() {
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		m.tabBar.Update(adjustedMsg)
		return m, nil
	}

	if m.dialogMgr.Open() {
		return m.forwardDialog(msg)
	}

	// Update hover state for resize handle
	region := m.hitTestRegion(msg.Y)
	m.isHoveringHandle = region == regionResizeHandle
	switch region {
	case regionContent:
		return m.forwardChat(msg)
	case regionEditor:
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		adjustedMsg.Y = msg.Y - m.editorTop()
		return m.forwardEditor(adjustedMsg)
	}

	return m, nil
}

// handleMouseRelease routes mouse release events with adjusted coordinates.
func (m *appModel) handleMouseRelease(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if m.isDragging {
		m.isDragging = false
		return m, nil
	}

	// Forward release to tab bar when a tab drag is active.
	if m.tabBar.IsDragging() {
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		if cmd := m.tabBar.Update(adjustedMsg); cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	if m.dialogMgr.Open() {
		return m.forwardDialog(msg)
	}

	region := m.hitTestRegion(msg.Y)
	switch region {
	case regionContent:
		return m.forwardChat(msg)
	case regionEditor:
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		adjustedMsg.Y = msg.Y - m.editorTop()
		return m.forwardEditor(adjustedMsg)
	}

	return m, nil
}

// handleWheelCoalesced routes coalesced wheel events with adjusted coordinates.
func (m *appModel) handleWheelCoalesced(msg messages.WheelCoalescedMsg) (tea.Model, tea.Cmd) {
	if msg.Delta == 0 {
		return m, nil
	}

	if m.dialogMgr.Open() {
		return m.forwardDialog(msg)
	}

	region := m.hitTestRegion(msg.Y)
	switch region {
	case regionContent:
		return m.forwardChat(msg)
	case regionEditor:
		m.editor.ScrollByWheel(msg.Delta)
		return m, nil
	}

	return m, nil
}

// layoutRegion represents a vertical region in the TUI layout.
type layoutRegion int

const (
	regionContent layoutRegion = iota
	regionResizeHandle
	regionTabBar
	regionEditor
	regionStatusBar
)

// hitTestRegion determines which layout region a Y coordinate falls in.
func (m *appModel) hitTestRegion(y int) layoutRegion {
	if m.leanMode {
		return hitTestLeanRegion(y, m.contentHeight)
	}
	_, editorHeight := m.editor.GetSize()
	return hitTestFullRegion(y, m.contentHeight, m.tabBar.Height(), editorHeight)
}

// hitTestLeanRegion is the pure layout calculation used in lean mode where
// the screen is split between content and editor only.
func hitTestLeanRegion(y, contentHeight int) layoutRegion {
	if y < contentHeight {
		return regionContent
	}
	return regionEditor
}

// hitTestFullRegion is the pure layout calculation used in full mode where the
// screen is content | resize handle | [tab bar] | editor | status bar.
// It is exported as a free function (rather than a method) so that it can be
// unit-tested without constructing a full appModel.
func hitTestFullRegion(y, contentHeight, tabBarHeight, editorHeight int) layoutRegion {
	resizeHandleTop := contentHeight
	tabBarTop := resizeHandleTop + 1
	editorTop := tabBarTop + tabBarHeight

	switch {
	case y < resizeHandleTop:
		return regionContent
	case y < tabBarTop:
		return regionResizeHandle
	case y < editorTop:
		return regionTabBar
	default:
		if y < editorTop+editorHeight {
			return regionEditor
		}
		return regionStatusBar
	}
}

// editorTop returns the Y coordinate where the editor starts.
func (m *appModel) editorTop() int {
	return m.contentHeight + 1 + m.tabBar.Height()
}

// handleEditorResize adjusts editor height based on drag position.
func (m *appModel) handleEditorResize(y int) tea.Cmd {
	// Calculate target lines from drag position
	editorPadding := styles.EditorStyle.GetVerticalFrameSize()
	targetLines := m.height - y - 1 - editorPadding - m.tabBar.Height()
	minLines := 4
	maxLines := max(minLines, (m.height-6)/2)
	newLines := max(minLines, min(targetLines, maxLines))
	if newLines != m.editorLines {
		m.editorLines = newLines
		return m.resizeAll()
	}
	return nil
}

// renderLeanWorkingIndicator renders a single-line working indicator for lean mode.
func (m *appModel) renderLeanWorkingIndicator() string {
	innerWidth := m.width - appPaddingHorizontal
	workingText := "Working\u2026"
	if queueLen := m.chatPage.QueueLength(); queueLen > 0 {
		workingText = fmt.Sprintf("Working\u2026 (%d queued)", queueLen)
	}
	line := m.workingSpinner.View() + " " + styles.SpinnerDotsHighlightStyle.Render(workingText)
	return lipgloss.NewStyle().Padding(0, styles.AppPadding).Width(innerWidth + appPaddingHorizontal).Render(line)
}

// renderResizeHandle renders the draggable separator between content and bottom panel.
func (m *appModel) renderResizeHandle(width int) string {
	if width <= 0 {
		return ""
	}

	innerWidth := width - appPaddingHorizontal

	// Use brighter style when actively dragging
	centerStyle := styles.ResizeHandleHoverStyle
	if m.isDragging {
		centerStyle = styles.ResizeHandleActiveStyle
	}

	// Show a small centered highlight when hovered or dragging
	centerPart := strings.Repeat("─", min(resizeHandleWidth, innerWidth))
	handle := centerStyle.Render(centerPart)

	// Always center handle on full width
	fullLine := lipgloss.PlaceHorizontal(
		max(0, innerWidth), lipgloss.Center, handle,
		lipgloss.WithWhitespaceChars("─"),
		lipgloss.WithWhitespaceStyle(styles.ResizeHandleStyle),
	)

	var result string
	switch {
	case m.chatPage.IsWorking():
		// Truncate right side and append spinner (handle stays centered)
		workingText := "Working…"
		if queueLen := m.chatPage.QueueLength(); queueLen > 0 {
			workingText = fmt.Sprintf("Working… (%d queued)", queueLen)
		}
		suffix := " " + m.workingSpinner.View() + " " + styles.SpinnerDotsHighlightStyle.Render(workingText)
		cancelKeyPart := styles.HighlightWhiteStyle.Render("Esc")
		suffix += " (" + cancelKeyPart + " to interrupt)"
		suffixWidth := lipgloss.Width(suffix)
		result = lipgloss.NewStyle().MaxWidth(innerWidth-suffixWidth).Render(fullLine) + suffix

	case m.chatPage.QueueLength() > 0:
		queueText := fmt.Sprintf("%d queued", m.chatPage.QueueLength())
		suffix := " " + styles.WarningStyle.Render(queueText) + " "
		suffixWidth := lipgloss.Width(suffix)
		result = lipgloss.NewStyle().MaxWidth(innerWidth-suffixWidth).Render(fullLine) + suffix

	default:
		result = fullLine
	}

	return lipgloss.NewStyle().Padding(0, styles.AppPadding).Render(result)
}

// View renders the model.
func (m *appModel) View() tea.View {
	windowTitle := m.windowTitle()

	if m.err != nil {
		return toFullscreenView(styles.ErrorStyle.Render(m.err.Error()), windowTitle, false, m.leanMode)
	}

	if !m.ready {
		return toFullscreenView(
			styles.CenterStyle.
				Width(m.wWidth).
				Height(m.wHeight).
				Render(styles.MutedStyle.Render("Loading…")),
			windowTitle,
			false,
			m.leanMode,
		)
	}

	// Content area (messages + sidebar) -- swaps per tab
	contentView := m.chatPage.View()

	// Lean mode: editor appears right after the last message, with empty
	// space pushed to the top via bottom-alignment.
	if m.leanMode {
		viewParts := []string{contentView}
		if m.chatPage.IsWorking() {
			viewParts = append(viewParts, m.renderLeanWorkingIndicator())
		}
		viewParts = append(viewParts, m.editor.View())
		inner := lipgloss.JoinVertical(lipgloss.Top, viewParts...)
		baseView := lipgloss.PlaceVertical(m.height, lipgloss.Bottom, inner)
		return toFullscreenView(baseView, windowTitle, m.chatPage.IsWorking(), m.leanMode)
	}

	// Resize handle (between content and bottom panel)
	resizeHandle := m.renderResizeHandle(m.width)

	// Tab bar (above editor)
	tabBarView := m.tabBar.View()

	// Editor (fixed position, per-session state)
	editorView := m.editor.View()

	// Status bar
	statusBarView := m.statusBar.View()

	// Combine: content | resize handle | [tab bar] | editor | status bar
	viewParts := []string{
		contentView,
		resizeHandle,
	}
	if tabBarView != "" {
		viewParts = append(viewParts, lipgloss.NewStyle().
			Padding(0, styles.AppPadding).
			Render(tabBarView))
	}
	viewParts = append(viewParts, editorView)
	if statusBarView != "" {
		viewParts = append(viewParts, statusBarView)
	}
	baseView := lipgloss.JoinVertical(lipgloss.Top, viewParts...)

	// Handle overlays
	hasOverlays := m.dialogMgr.Open() || m.notification.Open() || m.completions.Open()

	if hasOverlays {
		baseLayer := lipgloss.NewLayer(baseView)
		var allLayers []*lipgloss.Layer
		allLayers = append(allLayers, baseLayer)

		if m.dialogMgr.Open() {
			dialogLayers := m.dialogMgr.GetLayers()
			allLayers = append(allLayers, dialogLayers...)
		}

		if m.notification.Open() {
			allLayers = append(allLayers, m.notification.GetLayer())
		}

		if m.completions.Open() {
			allLayers = append(allLayers, m.completions.GetLayers()...)
		}

		compositor := lipgloss.NewCompositor(allLayers...)
		return toFullscreenView(compositor.Render(), windowTitle, m.chatPage.IsWorking(), m.leanMode)
	}

	return toFullscreenView(baseView, windowTitle, m.chatPage.IsWorking(), m.leanMode)
}

// windowTitle returns the terminal window title for the current model state.
// When the agent is working, a rotating spinner character is prepended so that
// terminal multiplexers (tmux) can detect activity in the pane.
func (m *appModel) windowTitle() string {
	return formatWindowTitle(m.appName, m.sessionState.SessionTitle(), m.chatPage.IsWorking(), m.animFrame)
}

// formatWindowTitle assembles the terminal window title string from the
// individual inputs that contribute to it. Pure function — extracted from the
// windowTitle method so that it can be unit-tested without constructing a
// full appModel.
func formatWindowTitle(appName, sessionTitle string, working bool, animFrame int) string {
	title := appName
	if sessionTitle != "" {
		title = sessionTitle + " - " + appName
	}
	if working {
		title = spinner.Frame(animFrame) + " " + title
	}
	return title
}

// exitFunc is the function called by the shutdown safety net when the
// graceful exit times out. It defaults to os.Exit but can be replaced
// in tests.
var exitFunc = os.Exit

var shutdownTimeout = 5 * time.Second

// cleanupAll cleans up all sessions, editors, and resources.
func (m *appModel) cleanupAll() {
	m.transcriber.Stop()
	m.closeTranscriptCh()
	for _, ed := range m.editors {
		ed.Cleanup()
	}

	// Safety net: force-exit if bubbletea's shutdown gets stuck.
	// This can happen when the renderer's flush goroutine blocks on a
	// stdout write (terminal buffer full) while holding the renderer
	// mutex, preventing the event loop from completing the render call
	// that follows tea.Quit.
	go func() {
		time.Sleep(shutdownTimeout)
		slog.Warn("Graceful shutdown timed out, forcing exit")
		exitFunc(0)
	}()
}

// openExternalEditor opens the current editor content in an external editor.
func (m *appModel) openExternalEditor() (tea.Model, tea.Cmd) {
	content := m.editor.Value()

	// Create a temporary file with the current content
	tmpFile, err := os.CreateTemp("", "cagent-*.md")
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to create temp file: %v", err))
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.WriteString(content); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to write temp file: %v", err))
	}
	_ = tmpFile.Close()

	// Get the editor command (VISUAL, EDITOR, or platform default)
	editorCmd := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editorCmd == "" {
		if goruntime.GOOS == "windows" {
			editorCmd = "notepad"
		} else {
			editorCmd = "vi"
		}
	}

	// Parse editor command (may include arguments like "code --wait")
	parts := strings.Fields(editorCmd)
	args := append(parts[1:], tmpPath)
	// External editor is owned by tea.ExecProcess, so exec.Command is intentional.
	cmd := exec.Command(parts[0], args...) //nolint:noctx // owned by tea.ExecProcess

	ed := m.editor
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			os.Remove(tmpPath)
			return notification.ShowMsg{Text: fmt.Sprintf("Editor error: %v", err), Type: notification.TypeError}
		}

		updatedContent, readErr := os.ReadFile(tmpPath)
		os.Remove(tmpPath)

		if readErr != nil {
			return notification.ShowMsg{Text: fmt.Sprintf("Failed to read edited file: %v", readErr), Type: notification.TypeError}
		}

		// Trim trailing newline that editors often add
		c := strings.TrimSuffix(string(updatedContent), "\n")

		if strings.TrimSpace(c) == "" {
			ed.SetValue("")
		} else {
			ed.SetValue(c)
		}

		return nil
	})
}

func toFullscreenView(content, windowTitle string, working, leanMode bool) tea.View {
	view := tea.NewView(content)
	view.AltScreen = !leanMode
	view.MouseMode = tea.MouseModeAllMotion
	view.BackgroundColor = styles.Background
	view.WindowTitle = windowTitle
	if working {
		view.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, 0)
	}
	return view
}
