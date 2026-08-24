// Package tui provides the top-level TUI model with tab and session management.
package tui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/audio/transcribe"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/plans"
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
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tour"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/internal/editorname"
	"github.com/docker/docker-agent/pkg/tui/internal/termfeatures"
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
	ar           *animation.Runtime
	shutdownDone <-chan struct{}
	cleanupOnce  sync.Once

	// cleanupAllOnce guards the full cleanupAll shutdown sequence so repeat
	// invocations (ExitSessionMsg followed by ExitConfirmedMsg, …) are no-ops:
	// they must not stack goroutines behind a wedged cleanup or arm parallel
	// safety nets that would each call exit.
	cleanupAllOnce sync.Once

	// exitFunc and shutdownTimeout override the shutdown safety net's
	// behaviour. Both are zero by default and resolved through
	// exitFn()/shutdownTimeoutOrDefault(), which fall back to the package
	// defaults (os.Exit / 5s). Tests set them per-model so they can run in
	// parallel without mutating package globals.
	exitFunc        func(int)
	shutdownTimeout time.Duration

	supervisor *supervisor.Supervisor
	tabBar     *tabbar.TabBar
	tuiStore   *tuistate.Store

	// plansSvc is the host-facing plan service behind /plans. Built lazily
	// by plansService() so plan.SharedStorage() only resolves its directory
	// after path configuration; tests inject one via WithPlansService.
	plansSvc plans.Service

	// planMutationTimeout and planReadTimeout override the bounded timeouts
	// of plan persistence and plan read commands. Zero means the package
	// defaults (defaultPlanMutationTimeout / defaultPlanReadTimeout); tests
	// set them per-model so timeout scenarios stay fast and parallel-safe.
	planMutationTimeout time.Duration
	planReadTimeout     time.Duration

	// planRefreshInFlight coalesces plan-dialog refreshes: at most one read
	// command runs at a time, and requests arriving meanwhile only mark
	// planRefreshQueued (plus planRefreshQueuedWarnings when the request
	// wanted listing warnings notified) so a single follow-up reload is
	// launched when the in-flight result lands. All three fields are touched
	// exclusively from Update.
	planRefreshInFlight       bool
	planRefreshQueued         bool
	planRefreshQueuedWarnings bool

	// planBrowserLoadInFlight and planBrowserLoadSessionID guard the /plans
	// browser-opening read: a repeated request for the same session while its
	// List is in flight is dropped, so duplicate browsers can never stack and
	// no redundant read starts. A request for a different session (the user
	// switched tabs) may launch; the superseded result is dropped as stale by
	// its session stamp and only the matching result clears the guard. Both
	// fields are touched exclusively from Update.
	planBrowserLoadInFlight  bool
	planBrowserLoadSessionID string

	// planDetailLoadsInFlight tracks the refs of running detail-opening
	// reads, so repeated open requests for the same plan cannot pile up
	// redundant Gets or stack duplicate detail dialogs. Refs are registered
	// in handleOpenPlanDetail and always cleared in handlePlanDetailLoaded;
	// the map is touched exclusively from Update.
	planDetailLoadsInFlight map[plans.Ref]struct{}

	// planExportsInFlight tracks the destination paths of running plan
	// export commands, so a duplicate export cannot race the no-overwrite
	// pre-check against the write. Keys are registered in handleExportPlan
	// and always cleared in handlePlanExportResult; the map is touched
	// exclusively from Update.
	planExportsInFlight map[string]struct{}

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

	// ctx preserves values from the root TUI context (trace context,
	// baggage, log attrs) without inheriting cancellation. Bubble Tea
	// handlers have no ctx parameter and many calls are persistence/cleanup
	// work that must survive shutdown cancellation.
	ctx func() context.Context

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

	// Exact root view cache. Unchanged accepted ticks return this complete value,
	// preserving metadata and function fields as well as content.
	viewCache      tea.View
	viewCacheValid bool
	hasPointer     bool

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

	// themeWatcher hot-reloads the active custom theme: it watches the
	// current theme's backing file and reports edits as ThemeFileChangedMsg.
	// Created in SetProgram (the callback needs the program to inject
	// messages into the event loop) and re-targeted after every theme
	// change via watchCurrentTheme.
	themeWatcher themeFileWatcher

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

	// lightDarkModeSet is true while DEC mode 2031 (terminal color-scheme
	// reports) is enabled. Set when the auto theme turns the mode on, so the
	// quit path knows to reset it and leave no stray reports in the shell.
	lightDarkModeSet bool

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

	// tour is the interactive getting-started overlay. Always non-nil;
	// inactive until started via an option, /getting-started, or the
	// first-run offer.
	tour *tour.Model

	// tourMode selects what happens at startup: nothing, offer the tour,
	// or start it immediately.
	tourMode tourMode

	// tourShowTelemetryNotice folds the telemetry notice into the tour
	// offer dialog when telemetry is enabled.
	tourShowTelemetryNotice bool

	// hideSidebar hides the sidebar and disables the ctrl+b toggle.
	hideSidebar bool

	// defaultNewSessionDir, when non-empty, is the directory generic
	// new-session actions (/new, Ctrl+T, the tab-bar and status-bar "+")
	// spawn in instead of opening the working-directory picker. Set only
	// when --working-dir was explicitly supplied on the CLI; a directory
	// carried by the spawn request still wins.
	defaultNewSessionDir string

	// layoutSettings is the active TUI layout customization (sidebar position
	// and section visibility). Shared by every tab and managed via /settings.
	layoutSettings messages.LayoutSettings

	// sendMode is what happens to messages sent while the agent is working:
	// steer into the ongoing stream (default) or queue until the turn ends.
	// Shared by every tab and managed via /settings.
	sendMode messages.SendMode

	// interruptMode is how Esc interrupts a running stream (confirmation
	// dialog, double-tap, or immediate). Shared by every tab and managed
	// via /settings.
	interruptMode messages.InterruptMode

	// showBanner displays the ASCII-art startup banner on an empty
	// conversation. Shared by every tab and managed via /settings.
	showBanner bool

	// buildCommandCategories is a function that returns the list of command categories.
	buildCommandCategories func(context.Context, tea.Model) []commands.Category

	appName    string
	appVersion string

	// disabledCommands holds slash commands to hide and disable.
	// Normalized to start with "/".
	disabledCommands map[string]bool

	imageWriter *tuiimage.Writer
}

// themeFileWatcher is the subset of *styles.ThemeWatcher the model drives.
// It is an interface so tests can record Watch/Stop calls.
type themeFileWatcher interface {
	Watch(themeRef string) error
	Stop()
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

// WithHideSidebar hides the chat sidebar. Unlike lean mode, the rest of
// the chrome (tab bar, status bar, dialogs) remains visible. The user
// cannot bring the sidebar back via the TUI.
func WithHideSidebar() Option {
	return func(m *appModel) {
		m.hideSidebar = true
	}
}

// WithImageWriter enables image overlays for Bubble Tea's cell-based renderer.
func WithImageWriter(writer *tuiimage.Writer) Option {
	return func(m *appModel) {
		m.imageWriter = writer
	}
}

// WithDefaultWorkingDir makes generic new-session actions (/new, Ctrl+T,
// the tab-bar and status-bar "+") spawn in dir instead of opening the
// working-directory picker. A directory carried by the spawn request still
// wins. Used when --working-dir was explicitly supplied on the CLI.
func WithDefaultWorkingDir(dir string) Option {
	return func(m *appModel) {
		m.defaultNewSessionDir = dir
	}
}

// tourMode selects the getting-started tour behavior at startup.
type tourMode int

const (
	tourModeNone tourMode = iota
	tourModeOffer
	tourModeStart
)

// WithTourStart starts the interactive getting-started tour as soon as the
// TUI launches. Ignored in lean mode, which has no overlay support.
func WithTourStart() Option {
	return func(m *appModel) {
		m.tourMode = tourModeStart
	}
}

// WithTourOffer shows the first-run dialog offering the getting-started
// tour when the TUI launches. showTelemetryNotice folds the telemetry
// notice into the offer so it never stacks with the stderr banner. Ignored
// in lean mode, which has no overlay support.
func WithTourOffer(showTelemetryNotice bool) Option {
	return func(m *appModel) {
		m.tourMode = tourModeOffer
		m.tourShowTelemetryNotice = showTelemetryNotice
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

// WithDisabledCommands hides and disables the given slash commands so they
// are stripped from the command palette, the slash-command parser, and
// completion. Each entry is normalized to start with "/" (so "cost" and
// "/cost" are equivalent) and lower-cased to match the registered slash
// command names (so "/Cost" and "/cost" are equivalent).
func WithDisabledCommands(slashCommands []string) Option {
	return func(m *appModel) {
		if len(slashCommands) == 0 {
			return
		}
		if m.disabledCommands == nil {
			m.disabledCommands = make(map[string]bool, len(slashCommands))
		}
		for _, c := range slashCommands {
			c = strings.ToLower(strings.TrimSpace(c))
			if c == "" {
				continue
			}
			if !strings.HasPrefix(c, "/") {
				c = "/" + c
			}
			m.disabledCommands[c] = true
		}
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

// WithToolRenderers registers custom tool-call renderers, keyed by tool name
// (e.g. "add") or a "category:<name>" key (e.g. "category:compute"). Registered
// renderers take precedence over the built-in ones, letting an embedder customize
// how specific tools are displayed — e.g. typesetting a calculator's result as
// math instead of the generic view.
//
// See pkg/tui/components/tool for the Builder contract; a renderer is typically
// a thin wrapper around toolcommon.NewBase.
func WithToolRenderers(renderers map[string]tool.Builder) Option {
	return func(*appModel) {
		for key, b := range renderers {
			tool.Register(key, b)
		}
	}
}

// New creates a new Model.
func New(ctx context.Context, spawner SessionSpawner, initialApp *app.App, initialWorkingDir string, cleanup func(), opts ...Option) tea.Model {
	tuiCtx := func() context.Context { return context.WithoutCancel(ctx) }

	ar := animation.NewRuntime()

	// Initialize supervisor
	sv := supervisor.New(spawner)

	// Initialize tab bar with configurable title length from user settings
	userSettings := userconfig.Get()
	tb := tabbar.New(ar, userSettings.GetTabTitleMaxLength())

	// Initialize tab store
	var ts *tuistate.Store
	var tsErr error
	ts, tsErr = tuistate.New(tuiCtx())
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
		ar:           ar,
		shutdownDone: ctx.Done(),
		buildCommandCategories: func(ctx context.Context, _ tea.Model) []commands.Category {
			return commands.BuildCommandCategories(ctx, initialApp)
		},
		supervisor:                    sv,
		tabBar:                        tb,
		tuiStore:                      ts,
		chatPages:                     map[string]chat.Page{},
		editors:                       map[string]editor.Editor{},
		sessionStates:                 map[string]*service.SessionState{sessID: initialSessionState},
		application:                   initialApp,
		ctx:                           tuiCtx,
		sessionState:                  initialSessionState,
		history:                       historyStore,
		pendingRestores:               make(map[string]string),
		pendingSidebarCollapsed:       make(map[string]bool),
		stashedDialogs:                make(map[string]stashedDialog),
		notification:                  notification.New(),
		dialogMgr:                     dialog.New(),
		completions:                   completion.New(),
		tour:                          tour.New(),
		transcriber:                   transcribe.New(os.Getenv("OPENAI_API_KEY")),
		workingSpinner:                spinner.New(ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle),
		focusedPanel:                  PanelEditor,
		editorLines:                   3,
		layoutSettings:                layoutSettingsFromConfig(userSettings.GetLayout()),
		sendMode:                      messages.ParseSendMode(userSettings.GetBusySendMode()),
		interruptMode:                 messages.ParseInterruptMode(userSettings.GetInterruptConfirmation()),
		showBanner:                    userSettings.GetShowBanner(),
		keyboardEnhancementsSupported: termfeatures.SupportsModifiedEnter(os.Getenv),
		dockerDesktop:                 os.Getenv("TERM_PROGRAM") == "docker_desktop",
		appName:                       "docker agent",
		appVersion:                    version.Version,
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
	initialChatPage := chat.New(m.ar, m.ctx(), initialApp, initialSessionState, m.chatPageOpts()...)
	initialChatPage.SetRoutingID(sessID)
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
// It also starts the theme file watcher, whose callback needs the program to
// inject hot-reload messages into the event loop.
func (m *appModel) SetProgram(p *tea.Program) {
	m.program = p
	m.supervisor.SetProgram(p)

	if m.themeWatcher != nil {
		m.themeWatcher.Stop()
	}
	m.themeWatcher = styles.NewThemeWatcher(func(themeRef string) {
		p.Send(messages.ThemeFileChangedMsg{ThemeRef: themeRef})
	})
	m.watchCurrentTheme()
}

// watchCurrentTheme points the theme watcher at the currently applied theme
// so edits to its backing file hot-reload it. The watcher no-ops for themes
// without a user theme file (e.g. built-ins).
func (m *appModel) watchCurrentTheme() {
	if m.themeWatcher == nil {
		return
	}
	theme := styles.CurrentTheme()
	if theme.Ref == "" {
		return
	}
	if err := m.themeWatcher.Watch(theme.Ref); err != nil {
		slog.Warn("Failed to watch theme file", "theme", theme.Ref, "error", err)
	}
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
	categories := m.buildCommandCategories(m.ctx(), m)
	if len(m.disabledCommands) == 0 {
		return categories
	}
	filtered := make([]commands.Category, 0, len(categories))
	for _, cat := range categories {
		items := make([]commands.Item, 0, len(cat.Commands))
		for _, item := range cat.Commands {
			if m.disabledCommands[item.SlashCommand] {
				continue
			}
			items = append(items, item)
		}
		if len(items) == 0 {
			continue
		}
		cat.Commands = items
		filtered = append(filtered, cat)
	}
	return filtered
}

// chatPageOpts returns the chat.PageOption slice derived from the current
// appModel configuration (e.g. lean mode).
func (m *appModel) chatPageOpts() []chat.PageOption {
	opts := []chat.PageOption{
		chat.WithCommandParser(commands.NewParser(m.commandCategories()...)),
		chat.WithLayoutSettings(m.layoutSettings),
		chat.WithSendMode(m.sendMode),
		chat.WithInterruptMode(m.interruptMode),
		chat.WithShowBanner(m.showBanner),
	}

	if m.leanMode {
		opts = append(opts, chat.WithLeanMode())
	}
	if m.hideSidebar {
		opts = append(opts, chat.WithHideSidebar())
	}
	return opts
}

// editorOpts returns the editor.Option slice derived from the current appModel.
func (m *appModel) editorOpts() []editor.Option {
	opts := []editor.Option{
		editor.WithCompletions(
			completions.NewCommandCompletion(m.commandCategories()),
			completions.NewFileCompletion(m.ctx()),
		),
	}
	if m.application.IsReadOnly() {
		opts = append(opts, editor.WithReadOnly())
	}
	return opts
}

// initSessionComponents creates a new chat page, session state, and editor for
// the given app and stores them in the per-session maps under tabID. The active
// convenience pointers (m.chatPage, m.sessionState, m.editor) are also updated.
func (m *appModel) initSessionComponents(tabID string, a *app.App, sess *session.Session) {
	ss := service.NewSessionState(sess)
	cp := chat.New(m.ar, m.ctx(), a, ss, m.chatPageOpts()...)
	cp.SetRoutingID(tabID)
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

func (m *appModel) contextShutdownCmd() tea.Cmd {
	return func() tea.Msg {
		go func() {
			<-m.shutdownDone
			m.cleanupManagedResources()
		}()
		return nil
	}
}

// Init initializes the model.
func (m *appModel) Init() tea.Cmd {
	return tea.Batch(m.init(), m.tourStartupCmd(), m.autoThemeInitCmd())
}

// autoThemeInitCmd enables DEC mode 2031 (terminal color-scheme reports) so
// terminals that support it push light/dark changes live while the auto
// theme is active. Terminals without the mode ignore the sequence. When the
// auto theme is off nothing is emitted, keeping default runs byte-identical.
func (m *appModel) autoThemeInitCmd() tea.Cmd {
	if !styles.AutoThemeEnabled() {
		return nil
	}
	m.lightDarkModeSet = true
	return tea.Raw(ansi.SetModeLightDark)
}

// quitCmd returns tea.Quit, first resetting DEC mode 2031 when the auto
// theme enabled it, so the terminal stops sending color-scheme reports
// after exit.
func (m *appModel) quitCmd() tea.Cmd {
	if !m.lightDarkModeSet {
		return tea.Quit
	}
	m.lightDarkModeSet = false
	return tea.Sequence(tea.Raw(ansi.ResetModeLightDark), tea.Quit)
}

// tourStartupCmd applies the configured startup tour mode: start the tour
// right away or open the first-run offer dialog. Lean mode has no overlay
// support, so the tour is disabled there.
func (m *appModel) tourStartupCmd() tea.Cmd {
	if m.leanMode {
		return nil
	}
	switch m.tourMode {
	case tourModeStart:
		return core.CmdHandler(messages.StartTourMsg{})
	case tourModeOffer:
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewTourOfferDialog(m.tourShowTelemetryNotice),
		})
	default:
		return nil
	}
}

func (m *appModel) init() tea.Cmd {
	shutdownCmd := m.contextShutdownCmd()
	// If a different tab should be active on startup, switch to it directly.
	// The initial tab's pending restore stays lazy — it will be loaded via
	// handleSwitchTab when the user eventually opens it, just like every
	// other non-active restored tab.
	if m.pendingActiveTab != "" {
		tabID := m.pendingActiveTab
		m.pendingActiveTab = ""
		_, switchCmd := m.handleSwitchTab(tabID)
		return tea.Batch(m.dialogMgr.Init(), switchCmd, shutdownCmd)
	}

	// If the initial tab has a pending session restore, go through
	// replaceActiveSession — the same code path as the /sessions command.
	activeID := m.supervisor.ActiveID()
	if oldSessionID, ok := m.pendingRestores[activeID]; ok {
		delete(m.pendingRestores, activeID)
		if store := m.application.SessionStore(); store != nil {
			if sess, err := store.GetSession(m.ctx(), oldSessionID); err == nil {
				_, cmd := m.replaceActiveSession(m.ctx(), sess)

				if m.tuiStore != nil && sess.WorkingDir != "" {
					if err := m.tuiStore.UpdateTabWorkingDir(m.ctx(), oldSessionID, sess.WorkingDir); err != nil {
						slog.Warn("Failed to update persisted working dir", "error", err)
					}
				}

				cmd = tea.Batch(cmd, m.applySidebarCollapsed(activeID))
				m.persistActiveTab(sess.ID)

				return tea.Batch(m.dialogMgr.Init(), cmd, shutdownCmd)
			}
		}
	}

	return tea.Batch(
		shutdownCmd,
		m.dialogMgr.Init(),
		m.chatPage.Init(),
		m.editor.Init(),
		m.editor.Focus(),
		m.application.SendFirstMessage(),
	)
}

// Update handles messages. It wraps update so the getting-started tour can
// observe every message that flows through the TUI (to detect completed
// steps) without ever consuming it.
func (m *appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	if obs := m.tour.Observe(msg); obs != nil {
		cmd = tea.Batch(cmd, obs)
	}
	return model, cmd
}

func tabVisualGeneration(tabBar *tabbar.TabBar) uint64 {
	if tabBar == nil {
		return 0
	}
	return tabBar.VisualGeneration()
}

func (m *appModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	beforeVisual := m.chatPage.VisualGeneration()
	beforeSidebarVisual := sidebarVisualGeneration(m.chatPage)
	beforeTabVisual := tabVisualGeneration(m.tabBar)
	beforeResizeHover := m.isHoveringHandle
	wasCached := m.viewCacheValid
	cached := m.viewCache
	canRestore := m.canRestorePointerCache(msg)
	defer func() {
		if canRestore && wasCached && m.chatPage.VisualGeneration() == beforeVisual &&
			sidebarVisualGeneration(m.chatPage) == beforeSidebarVisual &&
			tabVisualGeneration(m.tabBar) == beforeTabVisual &&
			m.isHoveringHandle == beforeResizeHover {
			m.viewCache, m.viewCacheValid = cached, true
		}
	}()
	if _, ok := msg.(tea.MouseMotionMsg); !ok {
		m.hasPointer = false
	}
	if _, isTick := msg.(animation.TickMsg); !isTick {
		m.viewCacheValid = false
	}
	// In lean mode, silently drop messages for features that don't exist.
	if m.leanMode {
		switch msg.(type) {
		case messages.SpawnSessionMsg, messages.SwitchTabMsg,
			messages.CloseTabMsg, messages.ReorderTabMsg,
			messages.ToggleSidebarMsg, messages.OpenSettingsDialogMsg,
			messages.ShowPlanBrowserMsg:
			return m, nil
		}
	}

	switch msg := msg.(type) {
	// --- Routing & Animation ---

	case messages.RoutedMsg:
		return m.handleRoutedMsg(msg)

	case animation.TickMsg:
		accepted, ok := m.ar.Accept(msg)
		if !ok {
			return m, nil
		}
		msg = accepted
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
		// Root-owned title and tab indicators are time-based rather than stateful
		// children, so include their rendered frame boundaries in the shared
		// dirty decision.
		before, after := msg.ElapsedBounds()
		if m.chatPage.IsWorking() && animation.Chat.FrameIndexAt(before) != animation.Chat.FrameIndexAt(after) {
			msg.MarkDirty()
		}
		if m.supervisor != nil {
			if tabs, _ := m.supervisor.GetTabs(); anyRunningTab(tabs) && animation.TabBusy.FrameIndexAt(before) != animation.TabBusy.FrameIndexAt(after) {
				msg.MarkDirty()
			}
		}
		cmds = append(cmds, m.ar.Continue())
		if msg.Dirty() {
			m.viewCacheValid = false
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
		m.handleReorderTab(msg)
		return m, nil

	case messages.ToggleSidebarMsg:
		if m.hideSidebar {
			return m, nil
		}
		if m.tuiStore != nil {
			persistedID := m.persistedSessionID(m.supervisor.ActiveID())
			if err := m.tuiStore.ToggleSidebarCollapsed(m.ctx(), persistedID); err != nil {
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
		if m.imageWriter != nil {
			m.imageWriter.Invalidate()
		}
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
			cmds = append(cmds, m.ar.EnsureRunning())
		}
		if styles.AutoThemeEnabled() {
			// Terminals without mode 2031 can still flip their appearance
			// while we're in the background; re-query on focus so the auto
			// theme catches up.
			cmds = append(cmds, tea.RequestBackgroundColor)
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

	case tea.BackgroundColorMsg:
		return m.handleColorSchemeChange(msg.IsDark())

	// DEC mode 2031 color-scheme reports. bubbletea passes these ultraviolet
	// events through untyped, so they are matched here directly.
	case uv.DarkColorSchemeEvent:
		return m.handleColorSchemeChange(true)

	case uv.LightColorSchemeEvent:
		return m.handleColorSchemeChange(false)

	case tea.KeyboardEnhancementsMsg:
		m.keyboardEnhancements = &msg
		m.keyboardEnhancementsSupported = msg.Flags != 0 || termfeatures.SupportsModifiedEnter(os.Getenv)
		m.statusBar.InvalidateCache()
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

	case dialog.OpenDialogMsg, dialog.CloseDialogMsg, dialog.ClosePlanDetailMsg:
		return m.forwardDialog(msg)

	case dialog.ExitConfirmedMsg:
		m.cleanupAll()
		quit := m.quitCmd()
		return m, quit

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

	// --- Getting-started tour ---

	case messages.StartTourMsg:
		return m.handleStartTour()

	case messages.TourFinishedMsg:
		return m.handleTourFinished(msg.Completed)

	case dialog.TourOfferResultMsg:
		return m.handleTourOfferResult(msg.Choice)

	// --- Terminal bell ---

	case messages.BellMsg:
		// Ring the terminal bell to alert the user that an inactive tab needs attention.
		// The BEL character (\a) is written to stderr which is typically the terminal.
		_, _ = fmt.Fprint(os.Stderr, "\a")
		return m, nil

	// --- Notifications ---

	case notificationCopiedMsg:
		m.notification = m.notification.MarkCopied(msg.ID)
		return m, nil

	case notification.ShowMsg, notification.HideMsg, notification.DismissMsg, notification.AutoHideMsg:
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

	case *runtime.SessionPlanUpdatedEvent:
		return m.handleSessionPlanUpdatedEvent(msg)

	case *runtime.PlanChangedEvent:
		return m.handlePlanChangedEvent(msg)

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
		quit := m.quitCmd()
		return m, quit

	case messages.ExitAfterFirstResponseMsg:
		m.cleanupAll()
		quit := m.quitCmd()
		return m, quit

	// --- SendMsg from editor ---

	case messages.SendMsg:
		// Forward send messages to the active content view
		if m.history != nil && !msg.BypassQueue {
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

	case messages.ShowAgentDetailsMsg:
		return m.handleShowAgentDetails(msg.AgentName)

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
		return m.handleCompactSession(msg)

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

	case messages.ShowContextDialogMsg:
		return m.handleShowContextDialog()

	case messages.DropAttachedFileMsg:
		return m.handleDropAttachedFile(msg.Path)

	case messages.ShowPermissionsDialogMsg:
		return m.handleShowPermissionsDialog()

	case messages.ShowToolsDialogMsg:
		return m.handleShowToolsDialog()

	case messages.ShowSkillsDialogMsg:
		return m.handleShowSkillsDialog()

	// --- Plan browser (/plans) ---

	case messages.ShowPlanBrowserMsg:
		return m.handleShowPlanBrowser()

	case messages.RefreshPlansMsg:
		return m.handleRefreshPlans()

	case messages.OpenPlanDetailMsg:
		return m.handleOpenPlanDetail(msg.Ref)

	case messages.ExportPlanMsg:
		return m.handleExportPlan(msg.Ref)

	case messages.SetPlanStatusMsg:
		return m.handleSetPlanStatus(msg)

	case messages.DeletePlanMsg:
		return m.handleDeletePlan(msg)

	case messages.CreatePlanMsg:
		return m.handleCreatePlan(msg.Name)

	case messages.EditPlanMsg:
		return m.handleEditPlan(msg)

	case planEditorClosedMsg:
		return m.handlePlanEditorClosed(msg)

	// Outcomes of the asynchronous plan persistence commands.
	case planStatusResultMsg:
		return m.handlePlanStatusResult(msg)

	case planDeleteResultMsg:
		return m.handlePlanDeleteResult(msg)

	case planWriteResultMsg:
		return m.handlePlanWriteResult(msg)

	// Outcomes of the asynchronous plan read commands.
	case planBrowserLoadedMsg:
		return m.handlePlanBrowserLoaded(msg)

	case planRefreshedMsg:
		return m.handlePlanRefreshed(msg)

	case planDetailLoadedMsg:
		return m.handlePlanDetailLoaded(msg)

	case planEditReadyMsg:
		return m.handlePlanEditReady(msg)

	case planExportResultMsg:
		return m.handlePlanExportResult(msg)

	case dialog.PlanBrowserDataMsg, dialog.PlanDetailDataMsg:
		return m.forwardDialog(msg)

	case messages.RestartToolsetMsg:
		return m.handleRestartToolset(msg.Name)

	case messages.AgentCommandMsg:
		return m.handleAgentCommand(msg.Command)

	case messages.StartShellMsg:
		return m.startShell()

	// --- Model picker ---

	case messages.OpenModelPickerMsg:
		return m.handleOpenModelPicker()

	case messages.RefreshModelPickerMsg:
		_, closeCmd := m.dialogMgr.Update(dialog.CloseDialogMsg{})
		model, refreshCmd := m.handleRefreshModelPicker(msg.Query)
		return model, tea.Batch(closeCmd, refreshCmd)

	case messages.ModelPickerRefreshedMsg:
		return m.handleModelPickerRefreshed(msg)

	case messages.ChangeModelMsg:
		return m.handleChangeModel(msg.ModelRef)

	case messages.SetThinkingLevelMsg:
		return m.handleSetThinkingLevel(msg.Level)

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

	// --- Settings (/settings) ---

	case messages.OpenSettingsDialogMsg:
		return m.handleOpenSettingsDialog()

	case messages.PreviewLayoutMsg:
		return m.applyLayoutSettings(msg.Layout)

	case messages.ApplySettingsMsg:
		return m.handleApplySettings(msg)

	case messages.CancelLayoutPreviewMsg:
		return m.applyLayoutSettings(msg.Original)

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
		if m.application.IsReadOnly() {
			return m, notification.WarningCmd("Session is read-only. No new messages can be sent.")
		}
		m.application.RunWithMessage(m.ctx(), nil, msg.Content)
		return m, nil

	// --- URL opening ---

	case messages.OpenURLMsg:
		return m.handleOpenURL(msg.URL)

	// --- Elicitation ---

	case messages.ElicitationResponseMsg:
		return m.handleElicitationResponse(msg.Action, msg.Content, msg.ElicitationID)

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
			m.applyPauseEvent(m.sessionState, msg)
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
			// Token-usage events are accounting, not agent-switch signals: a
			// background agent task's usage can arrive while the tab is idle
			// and must not move the current-agent marker to that agent.
			if _, isUsage := msg.Inner.(*runtime.TokenUsageEvent); !isUsage {
				if agentName := event.GetAgentName(); agentName != "" {
					sessionState.SetCurrentAgentName(agentName)
				}
			}
			m.applyPauseEvent(sessionState, msg.Inner)
		}
	}

	// Update the inactive chat page (discard cmds — UI effects aren't needed for hidden pages),
	// except its routed one-shot timers: presentation deadlines (e.g. the sidebar's transfer box)
	// must keep running while the tab is hidden, and their expiry lands back here as a RoutedMsg
	// for this page. Applying such a timer arms no new ones, so this cannot loop.
	updated, _ := chatPage.Update(msg.Inner)
	page := updated.(chat.Page)
	m.chatPages[msg.SessionID] = page

	// Shared plans are scope-global: a mutation from a background tab's agent
	// must still live-refresh the plan dialogs open on the active tab.
	if _, isPlanChange := msg.Inner.(*runtime.PlanChangedEvent); isPlanChange && m.planDialogOpen() {
		return m, tea.Batch(page.TakeRoutedTimers(), m.planRefreshCmd(false))
	}
	return m, page.TakeRoutedTimers()
}

// applyPauseEvent advances a session's pause indicator in response to runtime
// events. The runtime emits a PausedEvent when the loop reaches an
// iteration boundary and blocks, which flips a pending "Pausing…" state to
// "Paused". A stream that stops while still "Pausing…" (e.g. the agent
// happened to finish its final turn before re-entering the loop) is likewise
// resolved to "Paused", since /pause stays armed for the next run. The arm/
// disarm transitions themselves are driven by handleTogglePause.
func (m *appModel) applyPauseEvent(ss *service.SessionState, msg tea.Msg) {
	if ss == nil {
		return
	}
	switch msg.(type) {
	case *runtime.PausedEvent:
		if ss.PauseState() != service.PauseNone {
			ss.SetPauseState(service.PausePaused)
		}
	case *runtime.StreamStoppedEvent:
		if ss.PauseState() == service.PausePausing {
			ss.SetPauseState(service.PausePaused)
		}
	}
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
	sessions, err := store.GetSessionSummaries(m.ctx())
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load sessions: %v", err))
	}
	if len(sessions) == 0 {
		return m, notification.InfoCmd("No previous sessions found")
	}

	// Resolve the active workspace so the browser can group sessions started
	// in the current directory. This mirrors where a restore would land: the
	// same WorkingDir field drives replaceActiveSession.
	var workspaceDir string
	if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil {
		workspaceDir = runner.WorkingDir
	}
	if workspaceDir == "" {
		workspaceDir, _ = os.Getwd()
	}

	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSessionBrowserDialog(sessions, workspaceDir),
	})
}

// handleLoadSession loads a saved session into the current tab (if empty) or a new tab.
func (m *appModel) handleLoadSession(sessionID string) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}
	sess, err := store.GetSession(m.ctx(), sessionID)
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
	ctx := m.ctx()

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
		ctx := m.ctx()
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
	// A generic request (no directory) inherits the explicit --working-dir
	// default when one is configured; otherwise ask via the picker.
	if workingDir == "" {
		workingDir = m.defaultNewSessionDir
	}
	if workingDir == "" {
		return m.openWorkingDirPicker()
	}

	// Spawn the new session
	ctx := m.ctx()
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
		recentDirs, _ = m.tuiStore.GetRecentDirs(m.ctx(), 10)
		favoriteDirs, _ = m.tuiStore.GetFavoriteDirs(m.ctx())
	}

	// Use the active session's working directory so the picker reflects it
	// instead of the process CWD.
	var sessionWorkingDir string
	if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil {
		sessionWorkingDir = runner.WorkingDir
	}

	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewWorkingDirPickerDialog(m.ctx(), recentDirs, favoriteDirs, m.tuiStore, sessionWorkingDir),
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
			if sess, err := store.GetSession(m.ctx(), oldSessionID); err == nil {
				m.persistActiveTab(sess.ID)
				model, cmd := m.replaceActiveSession(m.ctx(), sess)

				if m.tuiStore != nil && sess.WorkingDir != "" {
					if err := m.tuiStore.UpdateTabWorkingDir(m.ctx(), oldSessionID, sess.WorkingDir); err != nil {
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
	m.workingSpinner = spinner.New(m.ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)

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

// replayPendingEvent checks if a session has pending attention events (e.g.
// tool confirmation, max iterations, elicitation) that were received while
// the tab was inactive. Every queued event is replayed, in arrival order, so
// concurrent attention events (e.g. two background-job elicitations) all
// reopen as stacked dialogs instead of only the most recent one (#3584). Each
// event was already processed by the chat page (updating the message list),
// but the dialog command was discarded for inactive sessions.
//
// If a stashed dialog instance is available for this session and its
// associated event still matches the first pending one, the same instance is
// re-opened so any in-progress input survives the round trip (issue #2770).
// Otherwise the stash is discarded and a fresh dialog is built.
func (m *appModel) replayPendingEvent(sessionID string) tea.Cmd {
	sessionState, ok := m.sessionStates[sessionID]
	if !ok {
		delete(m.stashedDialogs, sessionID)
		return nil
	}

	var cmds []tea.Cmd
	for first := true; ; first = false {
		pendingEvent := m.supervisor.ConsumePendingEvent(sessionID)
		if pendingEvent == nil {
			if first {
				// No pending event at all: any stash is stale (e.g. the agent finished).
				delete(m.stashedDialogs, sessionID)
			}
			break
		}

		// Only the first (oldest) event can match a stashed live dialog
		// instance: the stash holds exactly the one dialog that was on
		// screen when the user left the tab.
		if first {
			if stash, ok := m.stashedDialogs[sessionID]; ok {
				delete(m.stashedDialogs, sessionID)
				if stash.event == pendingEvent && stash.dialog != nil {
					cmds = append(cmds, core.CmdHandler(dialog.OpenDialogMsg{
						Model:            stash.dialog,
						OriginatingEvent: pendingEvent,
					}))
					continue
				}
			}
		}

		if cmd := m.dialogCmdForPendingEvent(pendingEvent, sessionState); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	if len(cmds) == 0 {
		return nil
	}
	// tea.Sequence (not tea.Batch) is required for deterministic ordering:
	// tea.Batch runs commands concurrently with no ordering guarantee on
	// which resulting Msg reaches Update first, so two concurrent attention
	// events (e.g. two background-job elicitations queued while this tab was
	// inactive) could stack in a random order on every replay even though
	// they were popped off the FIFO queue above in arrival order (#3584
	// should-fix: FIFO replay). tea.Sequence guarantees each OpenDialogMsg is
	// delivered to Update in the order the commands were built, so the
	// dialog stack's bottom-to-top order matches arrival order every time.
	return tea.Sequence(cmds...)
}

// dialogCmdForPendingEvent builds the OpenDialogMsg command for a single
// replayed attention event. Shared by every event replayPendingEvent pops off
// the queue after the first (stash-eligible) one.
func (m *appModel) dialogCmdForPendingEvent(pendingEvent tea.Msg, sessionState *service.SessionState) tea.Cmd {
	switch ev := pendingEvent.(type) {
	case *runtime.ToolCallConfirmationEvent:
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewToolConfirmationDialog(m.ar, ev, sessionState),
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
		if elicitationType, ok := ev.Meta["docker-agent/type"].(string); ok && elicitationType == "oauth_flow" {
			var serverURL string
			if url, ok := ev.Meta["docker-agent/server_url"].(string); ok {
				serverURL = url
			}
			return core.CmdHandler(dialog.OpenDialogMsg{
				Model:            dialog.NewOAuthAuthorizationDialog(m.ctx(), serverURL, m.application, ev.ElicitationID),
				OriginatingEvent: ev,
			})
		}
	}

	switch ev.Mode {
	case "url":
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewURLElicitationDialog(m.ctx(), ev.Message, ev.URL, ev.ElicitationID),
			OriginatingEvent: ev,
		})
	default:
		return core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewElicitationDialog(ev.Message, ev.Schema, ev.Meta, ev.ElicitationID),
			OriginatingEvent: ev,
		})
	}
}

// handleReorderTab moves a tab from one position to another.
func (m *appModel) handleReorderTab(msg messages.ReorderTabMsg) {
	m.supervisor.ReorderTab(msg.FromIdx, msg.ToIdx)

	if m.tuiStore != nil {
		tabs, _ := m.supervisor.GetTabs()
		ids := make([]string, len(tabs))
		for i, tab := range tabs {
			ids[i] = m.persistedSessionID(tab.SessionID)
		}
		if err := m.tuiStore.ReorderTab(m.ctx(), ids); err != nil {
			slog.Warn("Failed to persist tab reorder", "error", err)
		}
	}
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
		ctx, cancel := context.WithTimeout(m.ctx(), 1*time.Second)
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
			workingDir = "."
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
		if m.chatPage.IsWorking() || m.sessionState.PauseState() != service.PauseNone {
			chromeHeight = 1 // working/pause indicator line
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

	m.tour.SetSize(width, height, m.contentHeight)

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
	keys := core.GetKeys()
	quitBinding := keys.Quit

	if m.leanMode {
		return []key.Binding{quitBinding}
	}

	tabBinding := keys.SwitchFocus

	bindings := []key.Binding{quitBinding, tabBinding}
	bindings = append(bindings, m.tabBar.Bindings()...)

	// Additional global shortcuts. shift+tab is not user-configurable.
	bindings = append(bindings,
		keys.Commands,
		keys.Help,
		keys.ToggleYolo,
		keys.ToggleHideToolResults,
		keys.CycleAgent,
		keys.ModelPicker,
		keys.ClearQueue,
		keys.Suspend,
		key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("Shift+Tab", "cycle thinking level"),
		),
	)

	// leanMode already returned above, so only hideSidebar matters here.
	if !m.hideSidebar {
		bindings = append(bindings, keys.ToggleSidebar)
	}

	// Show newline help based on keyboard enhancement support. shift+enter is
	// detected at runtime; otherwise fall back to the configured newline key.
	if m.keyboardEnhancementsSupported {
		bindings = append(bindings, key.NewBinding(
			key.WithKeys("shift+enter"),
			key.WithHelp("Shift+Enter", "newline"),
		))
	} else {
		nl := keys.EditorNewline
		bindings = append(bindings, key.NewBinding(
			key.WithKeys(nl.Keys()...),
			key.WithHelp(nl.Help().Key, "newline"),
		))
	}

	if m.focusedPanel == PanelContent {
		bindings = append(bindings, m.chatPage.Bindings()...)
	} else {
		editorName := editorname.FromEnv(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
		editExternal := keys.EditExternal
		// Keep the binding's capitalized key label; only swap the description.
		editExternal.SetHelp(editExternal.Help().Key, "edit in "+editorName)
		bindings = append(bindings,
			editExternal,
			keys.HistorySearch,
		)
	}
	return bindings
}

// Bindings returns the key bindings shown in the status bar (a curated subset).
// This filters AllBindings() to show only the most essential commands.
func (m *appModel) Bindings() []key.Binding {
	all := m.AllBindings()

	// Define which keys should appear in the status bar
	keys := core.GetKeys()
	statusBarKeys := map[string]bool{
		keys.Quit.Keys()[0]:          true, // quit
		keys.SwitchFocus.Keys()[0]:   true, // switch focus
		"ctrl+t":                     true, // new tab (from tabBar)
		"ctrl+w":                     true, // close tab (from tabBar)
		"ctrl+p":                     true, // prev tab (from tabBar)
		"ctrl+n":                     true, // next tab (from tabBar)
		keys.Commands.Keys()[0]:      true, // commands
		keys.Help.Keys()[0]:          true, // help
		"shift+enter":                true, // newline
		keys.EditorNewline.Keys()[0]: true, // newline fallback
		keys.EditExternal.Keys()[0]:  true, // edit in external editor (editor context)
		keys.HistorySearch.Keys()[0]: true, // history search (editor context)
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

	keys := core.GetKeys()

	// The quit key is intercepted before any dialog handling so that every
	// dialog reacts to it consistently:
	//   - With no dialog open: open the exit confirmation dialog.
	//   - With any other dialog open: stack the exit confirmation on top so
	//     that the user can confirm exit (a second quit key or Y exits) or
	//     cancel it (N/Esc) and return to the original dialog.
	//   - With the exit confirmation already on top: forward the key so it
	//     can exit the program via its own Yes binding.
	if key.Matches(msg, keys.Quit) {
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
	case key.Matches(msg, keys.Suspend):
		return m, tea.Suspend

	case key.Matches(msg, keys.Commands):
		categories := m.commandCategories()
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewCommandPaletteDialog(categories),
		})

	case key.Matches(msg, keys.ToggleYolo):
		return m, core.CmdHandler(messages.ToggleYoloMsg{})

	case key.Matches(msg, keys.ToggleHideToolResults):
		return m, core.CmdHandler(messages.ToggleHideToolResultsMsg{})

	case key.Matches(msg, keys.CycleAgent):
		return m.handleCycleAgent()

	case key.Matches(msg, keys.ModelPicker):
		return m.handleOpenModelPicker()

	case key.Matches(msg, keys.ClearQueue):
		return m, core.CmdHandler(messages.ClearQueueMsg{})

	case key.Matches(msg, keys.Help):
		// Show contextual help dialog with ALL available key bindings
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewHelpDialog(m.AllBindings()),
		})
	}

	// History search is a modal state — capture all remaining keys before normal routing
	if m.focusedPanel == PanelEditor && m.editor.IsHistorySearchActive() {
		return m.forwardEditor(msg)
	}

	// Getting-started tour controls (Esc to quit, Enter on an empty editor
	// to advance) sit below dialogs and completions but above normal routing.
	if cmd, handled := m.handleTourKey(msg); handled {
		return m, cmd
	}

	switch {
	case key.Matches(msg, keys.EditExternal):
		return m.openExternalEditor()

	case key.Matches(msg, keys.HistorySearch):
		if m.focusedPanel == PanelEditor && !m.editor.IsRecording() {
			model, cmd := m.editor.EnterHistorySearch()
			m.editor = model.(editor.Editor)
			return m, cmd
		}

	// Toggle sidebar (propagates to content view regardless of focus)
	case key.Matches(msg, keys.ToggleSidebar):
		if m.leanMode || m.hideSidebar {
			return m, nil
		}
		return m.forwardChat(msg)

	// Shift+Tab cycles the current model's thinking-effort level
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
		return m.handleCycleThinkingLevel()

	// Focus switching: Tab key toggles between content and editor
	case key.Matches(msg, keys.SwitchFocus):
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

// lockModifiers are the keyboard lock states (Caps/Num/Scroll Lock). They ride
// along in a key event's modifier set under the Kitty protocol but must be
// ignored when matching shortcuts, since whether Caps Lock is on has no bearing
// on whether the user pressed ctrl+1.
const lockModifiers = tea.ModCapsLock | tea.ModNumLock | tea.ModScrollLock

// parseCtrlNumberKey checks if msg is ctrl+1 through ctrl+9 and returns the index (0-8), or -1 if not matched.
//
// It inspects the key's modifiers and code directly rather than msg.String():
// terminals with keyboard enhancements (e.g. the Kitty protocol) populate the
// key's Text field, which makes String() return the bare digit ("1") instead of
// "ctrl+1", silently breaking string-based matching.
func parseCtrlNumberKey(msg tea.KeyPressMsg) int {
	// Require Ctrl and no other active modifier. Lock states (Caps/Num/Scroll)
	// are masked out first so the shortcut still fires when, e.g., Caps Lock is on.
	if msg.Mod&^lockModifiers != tea.ModCtrl {
		return -1
	}
	// Prefer BaseCode (the PC-101 layout key) when present so the shortcut
	// works on international keyboards; fall back to Code otherwise.
	code := msg.Code
	if msg.BaseCode != 0 {
		code = msg.BaseCode
	}
	if code >= '1' && code <= '9' {
		return int(code - '1')
	}
	return -1
}

// switchFocus toggles between content and editor panels.
func (m *appModel) switchFocus() (tea.Model, tea.Cmd) {
	switch m.focusedPanel {
	case PanelEditor:
		// Tab-triggered argument completion (e.g. "/toolset-restart ") takes
		// priority over both suggestion-acceptance and switching focus; it's a
		// no-op when not applicable. Checked first so a stale ghost suggestion
		// left over from history (e.g. a previous "/toolset-restart <toolset>")
		// never gets silently accepted in place of opening a fresh, current
		// candidate popup - see regression test for the bug this guards against.
		if cmd := m.editor.TryStartArgumentCompletion(); cmd != nil {
			return m, cmd
		}
		// Otherwise, accept a pending suggestion if there is one.
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

func sidebarVisualGeneration(page chat.Page) uint64 {
	if page, ok := page.(interface{ SidebarVisualGeneration() uint64 }); ok {
		return page.SidebarVisualGeneration()
	}
	return 0
}

func (m *appModel) canRestorePointerCache(msg tea.Msg) bool {
	if !m.viewCacheValid || m.tabBar == nil || m.chatPage == nil || m.isDragging || m.dialogMgr == nil || m.dialogMgr.Open() ||
		m.chatPage.IsSelecting() || m.notification.Open() {
		return false
	}
	var x, y int
	switch msg := msg.(type) {
	case tea.MouseMotionMsg:
		if msg.Button == tea.MouseLeft && !m.tabBar.IsDragging() {
			return false
		}
		x, y = msg.X, msg.Y
	case messages.WheelCoalescedMsg:
		x, y = msg.X, msg.Y
	case *runtime.AgentChoiceEvent:
		return true
	case messages.RoutedMsg:
		_, ok := msg.Inner.(*runtime.AgentChoiceEvent)
		return ok
	default:
		return false
	}
	if m.hitTestRegion(y) != regionContent {
		switch msg.(type) {
		case tea.MouseMotionMsg:
			return true
		case messages.WheelCoalescedMsg:
			return true
		}
		return false
	}
	_, motion := msg.(tea.MouseMotionMsg)
	if motion {
		return true
	}
	if _, wheel := msg.(messages.WheelCoalescedMsg); wheel {
		return true
	}
	page, ok := m.chatPage.(interface{ PointerTargetsMessages(x, y int) bool })
	return ok && page.PointerTargetsMessages(x, y)
}

// handleMouseClick routes mouse clicks to the appropriate component based on Y coordinate.
func (m *appModel) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	// Check if click hits a notification close button before handling body clicks.
	if cmd := m.notification.HandleClick(msg.X, msg.Y); cmd != nil {
		return m, cmd
	}
	if id, text, ok := m.notification.CopyHit(msg.X, msg.Y); ok {
		return m, copyNotificationToClipboard(id, text)
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
	var cmds []tea.Cmd
	batchWith := func(cmd tea.Cmd) tea.Cmd {
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...)
	}

	if !m.leanMode {
		updated, cmd := m.notification.HandleMouseMotion(msg.X, msg.Y)
		m.notification = updated
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	if m.isDragging {
		cmd := m.handleEditorResize(msg.Y)
		return m, batchWith(cmd)
	}

	// Forward drag motion to tab bar when a tab drag is active.
	if m.tabBar.IsDragging() {
		adjustedMsg := msg
		adjustedMsg.X = msg.X - styles.AppPadding
		if cmd := m.tabBar.Update(adjustedMsg); cmd != nil {
			return m, batchWith(cmd)
		}
		return m, tea.Batch(cmds...)
	}

	if m.dialogMgr.Open() {
		model, cmd := m.forwardDialog(msg)
		return model, batchWith(cmd)
	}

	// A text-selection drag must keep receiving motion wherever the cursor
	// goes (editor, tab bar, status bar), otherwise the selection freezes
	// and the release-copy pair is lost.
	if m.chatPage.IsSelecting() {
		model, cmd := m.forwardChat(msg)
		return model, batchWith(cmd)
	}

	// Update hover state for resize handle
	region := m.hitTestRegion(msg.Y)
	m.isHoveringHandle = region == regionResizeHandle
	switch region {
	case regionContent:
		model, cmd := m.forwardChat(msg)
		return model, batchWith(cmd)
	case regionEditor:
		// bubbles/textarea has no motion behavior; clicks, releases, wheel and
		// keyboard input still take their normal routed paths.
		return m, tea.Batch(cmds...)
	}

	return m, tea.Batch(cmds...)
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

	// Finish a text-selection drag in the chat no matter where the button
	// was released; this is what triggers the selection copy.
	if m.chatPage.IsSelecting() {
		return m.forwardChat(msg)
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

// renderLeanWorkingIndicator renders a single-line working/pause indicator for
// lean mode.
func (m *appModel) renderLeanWorkingIndicator() string {
	innerWidth := m.width - appPaddingHorizontal
	var line string
	switch m.sessionState.PauseState() {
	case service.PausePaused:
		line = styles.WarningStyle.Render("⏸ Paused") + " " + styles.MutedStyle.Render("(/pause to resume)")
	case service.PausePausing:
		line = m.workingSpinner.View() + " " + styles.WarningStyle.Render("Pausing… (finishing current request)")
	default:
		workingText := "Working\u2026"
		if queueLen := m.chatPage.QueueLength(); queueLen > 0 {
			workingText = fmt.Sprintf("Working\u2026 (%d queued)", queueLen)
		}
		line = m.workingSpinner.View() + " " + styles.SpinnerDotsHighlightStyle.Render(workingText)
	}
	return lipgloss.NewStyle().Padding(0, styles.AppPadding).Width(innerWidth + appPaddingHorizontal).Render(line)
}

// renderResizeHandle renders the draggable separator between content and bottom panel.
func (m *appModel) renderResizeHandle(width int) string {
	// The terminal can report degenerate sizes (0x0, 1x1) where the padded
	// inner width goes negative; there is nothing to draw a handle on.
	innerWidth := width - appPaddingHorizontal
	if innerWidth <= 0 {
		return ""
	}

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
		innerWidth, lipgloss.Center, handle,
		lipgloss.WithWhitespaceChars("─"),
		lipgloss.WithWhitespaceStyle(styles.ResizeHandleStyle),
	)

	var result string
	switch {
	case m.sessionState.PauseState() == service.PausePaused:
		// Static indicator: the loop is idle until the user resumes.
		resumeKey := styles.HighlightWhiteStyle.Render("/pause")
		suffix := " " + styles.WarningStyle.Render("⏸ Paused") + " (" + resumeKey + " to resume)"
		result = lineWithSuffix(fullLine, suffix, innerWidth)

	case m.sessionState.PauseState() == service.PausePausing:
		// The agent is finishing the in-flight request before it pauses.
		suffix := " " + m.workingSpinner.View() + " " + styles.WarningStyle.Render("Pausing…") + " (finishing current request)"
		result = lineWithSuffix(fullLine, suffix, innerWidth)

	case m.chatPage.IsWorking():
		// Truncate right side and append spinner (handle stays centered)
		workingText := "Working…"
		if queueLen := m.chatPage.QueueLength(); queueLen > 0 {
			workingText = fmt.Sprintf("Working… (%d queued)", queueLen)
		}
		suffix := " " + m.workingSpinner.View() + " " + styles.SpinnerDotsHighlightStyle.Render(workingText)
		cancelKeyPart := styles.HighlightWhiteStyle.Render("Esc")
		suffix += " (" + cancelKeyPart + " to interrupt)"
		result = lineWithSuffix(fullLine, suffix, innerWidth)

	case m.chatPage.QueueLength() > 0:
		queueText := fmt.Sprintf("%d queued", m.chatPage.QueueLength())
		suffix := " " + styles.WarningStyle.Render(queueText) + " "
		result = lineWithSuffix(fullLine, suffix, innerWidth)

	default:
		result = fullLine
	}

	return lipgloss.NewStyle().Padding(0, styles.AppPadding).Render(result)
}

// lineWithSuffix appends a status suffix to the centered handle line,
// truncating so the total never exceeds width. lipgloss ignores non-positive
// MaxWidth values, so a suffix wider than the line must be truncated itself
// or the row would overflow on narrow terminals.
func lineWithSuffix(line, suffix string, width int) string {
	suffixWidth := lipgloss.Width(suffix)
	if suffixWidth >= width {
		return lipgloss.NewStyle().MaxWidth(width).Render(suffix)
	}
	return lipgloss.NewStyle().MaxWidth(width-suffixWidth).Render(line) + suffix
}

func anyRunningTab(tabs []messages.TabInfo) bool {
	for _, tab := range tabs {
		if tab.IsRunning {
			return true
		}
	}
	return false
}

// View renders the model.
func (m *appModel) View() tea.View {
	if m.viewCacheValid {
		return m.viewCache
	}
	view := m.composeView()
	m.viewCache = view
	m.viewCacheValid = true
	return view
}

func (m *appModel) composeView() tea.View {
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
		if m.chatPage.IsWorking() || m.sessionState.PauseState() != service.PauseNone {
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
	hasOverlays := m.dialogMgr.Open() || m.notification.Open() || m.completions.Open() || m.tour.Active()

	if hasOverlays {
		baseLayer := lipgloss.NewLayer(baseView)
		var allLayers []*lipgloss.Layer
		allLayers = append(allLayers, baseLayer)

		// The tour card sits above the base UI but below dialogs, so the
		// step it teaches (palette, tool approval…) is never hidden by it.
		if tourLayer := m.tour.Layer(); tourLayer != nil {
			allLayers = append(allLayers, tourLayer)
		}

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
		return m.fullscreenView(compositor.Render(), windowTitle)
	}

	return m.fullscreenView(baseView, windowTitle)
}

func (m *appModel) fullscreenView(content, windowTitle string) tea.View {
	if m.imageWriter != nil {
		content = m.imageWriter.SetContent(content)
	}
	return toFullscreenView(content, windowTitle, m.chatPage.IsWorking(), m.leanMode)
}

// windowTitle returns the terminal window title for the current model state.
// When the agent is working, a rotating spinner character is prepended so that
// terminal multiplexers (tmux) can detect activity in the pane.
func (m *appModel) windowTitle() string {
	return formatWindowTitle(m.ar.Now(), m.appName, m.sessionState.SessionTitle(), m.chatPage.IsWorking())
}

// formatWindowTitle assembles the terminal window title string from the
// individual inputs that contribute to it. Pure function — extracted from the
// windowTitle method so that it can be unit-tested without constructing a
// full appModel.
func formatWindowTitle(elapsed time.Duration, appName, sessionTitle string, working bool) string {
	title := appName
	if sessionTitle != "" {
		title = sessionTitle + " - " + appName
	}
	if working {
		title = animation.Chat.FrameAt(elapsed) + " " + title
	}
	return title
}

// defaultExitFunc is the process-exit function used by the shutdown safety
// net when the graceful exit times out. It is a package var (not a const)
// only so the os.Exit indirection is testable at the package level; per-model
// overrides go through appModel.exitFunc.
var defaultExitFunc = os.Exit

const defaultShutdownTimeout = 5 * time.Second

// exitFn returns the exit function for this model, falling back to the
// package default when unset.
func (m *appModel) exitFn() func(int) {
	if m.exitFunc != nil {
		return m.exitFunc
	}
	return defaultExitFunc
}

// shutdownTimeoutOrDefault returns the shutdown grace period for this model,
// falling back to the package default when unset.
func (m *appModel) shutdownTimeoutOrDefault() time.Duration {
	if m.shutdownTimeout > 0 {
		return m.shutdownTimeout
	}
	return defaultShutdownTimeout
}

// cleanupManagedResources shuts down resources owned by the top-level TUI
// lifecycle. It is safe to call from both the Bubble Tea event loop (normal
// exit) and the context watcher (external cancellation).
func (m *appModel) cleanupManagedResources() {
	m.cleanupOnce.Do(func() {
		if m.themeWatcher != nil {
			m.themeWatcher.Stop()
		}
		if m.tuiStore != nil {
			_ = m.tuiStore.Close()
		}
		if m.supervisor != nil {
			m.supervisor.Shutdown()
		}
	})
}

// CleanupForTesting releases the model-managed resources (theme watcher, TUI
// state store, session supervisor) without going through the interactive exit
// flow. It must only be called after the Bubble Tea program has stopped —
// running it against a live program races the event loop — and it is not a
// production shutdown entrypoint. It covers only the model-managed resources
// above, not the full interactive cleanupAll set (transcriber, editors, exit
// safety net). External test harnesses call it — tuitest does so automatically
// once the run loop has exited — so the SQLite state store's handle on
// tui_state.db is released before t.TempDir removal, which otherwise fails on
// Windows. Idempotent: it shares the once-guarded shutdown path.
func (m *appModel) CleanupForTesting() {
	m.cleanupManagedResources()
}

// cleanupAll cleans up all sessions, editors, and resources. It is invoked
// from several message handlers (ExitSessionMsg, ExitConfirmedMsg, …) and may
// be called more than once on the same model; the entire shutdown sequence is
// once-guarded so repeat calls are no-ops and cannot pile up goroutines on a
// wedged cleanup or arm parallel safety nets.
func (m *appModel) cleanupAll() {
	m.cleanupAllOnce.Do(func() {
		m.transcriber.Stop()
		m.closeTranscriptCh()
		for _, ed := range m.editors {
			ed.Cleanup()
		}

		// Shut down managed resources (supervisor, TUI state store) in the
		// background: supervisor.Shutdown stops every session's toolsets, which
		// can block indefinitely (e.g. an MCP toolset wedged behind a broken
		// Docker daemon). Running it synchronously here would freeze the Update
		// loop before tea.Quit is ever returned, leaving the exit confirmation
		// apparently ignored.
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			m.cleanupManagedResources()
		}()

		// Safety net: bubbletea's renderer can deadlock on shutdown if stdout
		// is wedged — the final flush re-acquires the mutex that the still
		// blocked previous flush is holding — and the resource cleanup above can
		// likewise stall forever. Race both against a deadline and force-exit if
		// shutdown stalls. Snapshot the program and package globals so they
		// can't race with t.Cleanup. Without a program (tests, exit before
		// SetProgram) there is no renderer to deadlock and no UI left to
		// unblock, so no safety net is armed; the background cleanup still runs.
		program := m.program
		if program == nil {
			return
		}
		m.program = nil
		timeout := m.shutdownTimeoutOrDefault()
		exit := m.exitFn()
		go func() {
			done := make(chan struct{})
			go func() {
				program.Wait()
				close(done)
			}()

			deadline := time.After(timeout)
			for _, ch := range []<-chan struct{}{done, cleanupDone} {
				select {
				case <-ch:
				case <-deadline:
					slog.Warn("Graceful shutdown timed out, forcing exit")
					// ReleaseTerminal grabs the same mutex that's stuck, so
					// fire-and-forget; exit either way.
					go func() { _ = program.ReleaseTerminal() }()
					exit(0)
					return
				}
			}
		}()
	})
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

	cmd := editorname.Command(tmpPath)

	ed := m.editor
	return m, tea.ExecProcess(cmd, externalEditorCallback(ed, tmpPath))
}

// externalEditorCallback builds the tea.ExecProcess callback that reads the
// edited temp file back into the editor once the external editor exits.
func externalEditorCallback(ed editor.Editor, tmpPath string) func(error) tea.Msg {
	return func(err error) tea.Msg {
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

		// Ctrl+g works from any panel, so make sure the editor has focus:
		// otherwise Enter goes to the content panel and the edited text is
		// never sent (or queued while the agent is working).
		return messages.RequestFocusMsg{Target: messages.PanelEditor}
	}
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
