package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// minWindowWidth is the threshold below which sidebar switches to horizontal mode
	minWindowWidth = 120
	// dragThreshold is pixels of movement needed to distinguish click from drag
	dragThreshold = 3
	// toggleColumnWidth is the width of the sidebar toggle/resize handle column
	toggleColumnWidth = 1
	// appPaddingHorizontal is total horizontal padding from AppStyle (left + right)
	appPaddingHorizontal = 2 * styles.AppPadding
)

// sidebarLayoutMode represents how the sidebar is displayed
type sidebarLayoutMode int

const (
	// sidebarVertical: wide window, sidebar beside the chat (left or right)
	sidebarVertical sidebarLayoutMode = iota
	// sidebarCollapsed: wide window but user collapsed sidebar, shown at top with toggle
	sidebarCollapsed
	// sidebarCollapsedNarrow: narrow window or forced band position, shown without toggle
	sidebarCollapsedNarrow
)

// sidebarLayout holds computed layout values for the current frame.
// Computing this once per update avoids repeating calculations across View, SetSize, and input handlers.
type sidebarLayout struct {
	mode          sidebarLayoutMode
	sidebarOnLeft bool // vertical mode: sidebar sits left of the chat
	bandAtBottom  bool // collapsed modes: band sits below the chat instead of above
	innerWidth    int  // window width minus app padding
	chatWidth     int  // width available for chat/messages
	chatStartX    int  // X coordinate where the chat area starts (relative to innerWidth)
	sidebarWidth  int  // actual sidebar width (varies by mode)
	sidebarStartX int  // X coordinate where sidebar content starts (relative to innerWidth)
	handleX       int  // X coordinate of resize handle column (only valid in vertical mode)
	chatHeight    int  // height available for chat area
	sidebarHeight int  // height of sidebar
}

// isOnHandle returns true if adjustedX (already adjusted for app padding) is on the resize handle.
func (l sidebarLayout) isOnHandle(adjustedX int) bool {
	return l.mode == sidebarVertical && adjustedX == l.handleX
}

// isInSidebar returns true if adjustedX is within the sidebar area.
func (l sidebarLayout) isInSidebar(adjustedX int) bool {
	if l.mode != sidebarVertical {
		return false
	}
	if l.sidebarOnLeft {
		return adjustedX < l.handleX
	}
	return adjustedX >= l.sidebarStartX
}

// bandY returns the Y coordinate where the collapsed band starts.
func (l sidebarLayout) bandY() int {
	if l.bandAtBottom {
		return l.chatHeight
	}
	return 0
}

// isInBand returns true if y falls within the collapsed sidebar band.
func (l sidebarLayout) isInBand(y int) bool {
	if l.mode == sidebarVertical {
		return false
	}
	return y >= l.bandY() && y < l.bandY()+l.sidebarHeight
}

// bandContentY converts a screen Y coordinate to a Y coordinate relative to
// the band content. A bottom band renders its divider on the first line, so
// the content starts one line lower.
func (l sidebarLayout) bandContentY(y int) int {
	contentY := y - l.bandY()
	if l.bandAtBottom {
		contentY--
	}
	return contentY
}

// showToggle returns true if a toggle glyph should be shown.
func (l sidebarLayout) showToggle() bool {
	return l.mode == sidebarVertical || l.mode == sidebarCollapsed
}

// SidebarSettings holds the sidebar display settings that should persist across session changes.
type SidebarSettings struct {
	Collapsed      bool
	PreferredWidth int
}

// Page represents the main chat content area (messages + sidebar).
// The editor and resize handle are owned by the parent (tui.Model).
type Page interface {
	layout.Model
	layout.Sizeable
	layout.Help
	CompactSession(additionalPrompt string) tea.Cmd
	// SetSessionStarred updates the sidebar star indicator
	SetSessionStarred(starred bool)
	// SetTitleRegenerating sets the title regenerating state on the sidebar
	SetTitleRegenerating(regenerating bool) tea.Cmd
	// ScrollToBottom scrolls the messages viewport to the bottom if auto-scroll is active.
	ScrollToBottom() tea.Cmd
	// IsWorking returns whether the agent is currently working
	IsWorking() bool
	// IsInlineEditing returns true if a past user message is being edited inline
	IsInlineEditing() bool
	// IsSelecting returns true while a text-selection drag is active in the messages panel
	IsSelecting() bool
	// QueueLength returns the number of queued messages
	QueueLength() int
	// FocusMessages gives focus to the messages panel for keyboard scrolling
	FocusMessages() tea.Cmd
	// FocusMessageAt gives focus and selects the message at the given screen coordinates
	FocusMessageAt(x, y int) tea.Cmd
	// BlurMessages removes focus from the messages panel
	BlurMessages()
	// GetSidebarSettings returns the current sidebar display settings
	GetSidebarSettings() SidebarSettings
	// SetSidebarSettings applies sidebar display settings
	SetSidebarSettings(settings SidebarSettings)
	// SetLayoutSettings applies layout customization (sidebar position,
	// section spacing, section visibility, and agent info mode) and
	// relayouts the page.
	SetLayoutSettings(settings msgtypes.LayoutSettings) tea.Cmd
	// SetSendMode sets what happens to messages sent while the agent is
	// working: steer into the ongoing stream or queue until the turn ends.
	SetSendMode(mode msgtypes.SendMode)
	// SetInterruptMode sets how Esc interrupts a running stream.
	SetInterruptMode(mode msgtypes.InterruptMode)
	// SetShowBanner controls whether the ASCII-art startup banner is drawn
	// on an empty conversation.
	SetShowBanner(show bool)
	// SetRoutingID records the tab identity used to address this page's
	// one-shot UI timers back to it (messages.RoutedMsg.SessionID). The
	// appModel keys its chat pages — and the supervisor its event routing —
	// by this ID, which is the tab's initial session ID and may diverge from
	// the current app.Session().ID after a session restore or in-place
	// replace.
	SetRoutingID(id string)
	// TakeRoutedTimers returns and clears the routed one-shot timer commands
	// armed by the most recent Update. The active page's Update already
	// returns them inside its regular command; the appModel calls this for
	// background pages — whose regular commands are discarded — so
	// presentation deadlines keep running while a tab is hidden.
	TakeRoutedTimers() tea.Cmd
	VisualGeneration() uint64
}

func (p *chatPage) VisualGeneration() uint64 { return p.messages.VisualGeneration() }
func (p *chatPage) SidebarVisualGeneration() uint64 {
	return p.sidebar.VisualGeneration()
}

type queuedMessage struct {
	content     string
	attachments []msgtypes.Attachment
}

// maxQueuedMessages is the maximum number of messages that can be queued
const maxQueuedMessages = 5

// chatPage implements Page
//
//nolint:gocritic // Kept near its supporting queued-message declarations.
type chatPage struct {
	ar            *animation.Runtime
	width, height int

	// Components
	sidebar  sidebar.Model
	messages messages.Model

	sessionState *service.SessionState

	// State
	working        bool
	leanMode       bool
	hideSidebar    bool
	layoutSettings msgtypes.LayoutSettings
	sendMode       msgtypes.SendMode

	msgCancel       context.CancelFunc
	streamCancelled bool
	// streamDepth is the nesting depth of active streams (StreamStarted++,
	// StreamStopped--). >0 during a root compaction marks it as automatic
	// (nested mid-run); standalone /compact emits no StreamStarted.
	streamDepth     int
	agentStack      []string // agent per active stream level; len(agentStack)==streamDepth
	streamStartTime time.Time

	// routingID is the tab identity this page's routed UI timers are
	// addressed to; empty for standalone pages (timers then fire unrouted,
	// which is correct when this is the only page).
	routingID string
	// pendingTimers holds the routed timer commands armed by the current
	// Update, so they can be re-collected via TakeRoutedTimers when the
	// regular command is discarded (background tabs).
	pendingTimers []tea.Cmd

	// Track whether we've received content from an assistant response
	// Used by --exit-after-response to ensure we don't exit before receiving content
	hasReceivedAssistantContent bool
	showStartupBanner           bool
	// hideBanner mirrors the user's show_banner setting; the zero value
	// keeps the banner so page literals stay banner-enabled.
	hideBanner bool

	// Message queue for enqueuing messages while agent is working
	messageQueue []queuedMessage

	// Editing state for branching sessions
	editing          bool
	branchAtPosition int
	editAttachments  []msgtypes.Attachment // Preserved attachments from original message

	// Key map
	keyMap KeyMap

	// interruptMode controls how Esc interrupts a running stream.
	interruptMode msgtypes.InterruptMode
	// lastInterruptTime tracks when the last Esc was pressed for double-tap mode.
	lastInterruptTime time.Time
	// waitingForDoubleTap indicates the user pressed Esc once and is waiting
	// for a second press to confirm the interrupt.
	waitingForDoubleTap bool

	ctx func() context.Context

	app *app.App

	// Command parser for handling slash commands in the editor
	commandParser *commands.Parser

	// Sidebar drag state
	isDraggingSidebar     bool // True while dragging the sidebar resize handle
	sidebarDragStartX     int  // X position when drag started
	sidebarDragStartWidth int  // Sidebar preferred width when drag started
	sidebarDragMoved      bool // True if mouse moved beyond threshold during drag

	// appliedLayout is the layout geometry last pushed to child components by
	// SetSize. Update compares it against the live layout to catch silent
	// shifts (e.g. the collapsed sidebar band growing when async startup info
	// arrives) that would otherwise leave mouse hit-testing offset.
	appliedLayout sidebarLayout
}

// sidebarHidden reports whether the sidebar should be omitted entirely from
// layout and rendering (lean mode or explicit --sidebar=false).
func (p *chatPage) sidebarHidden() bool {
	return p.leanMode || p.hideSidebar
}

// computeSidebarLayout calculates the layout based on current state.
func (p *chatPage) computeSidebarLayout() sidebarLayout {
	innerWidth := p.width - appPaddingHorizontal

	// No sidebar at all (lean mode or hideSidebar): chat fills the area.
	if p.sidebarHidden() {
		return sidebarLayout{
			mode:       sidebarCollapsedNarrow,
			innerWidth: innerWidth,
			chatWidth:  innerWidth,
			chatHeight: max(1, p.height),
		}
	}

	position := p.layoutSettings.SidebarPosition
	sideBySide := position == msgtypes.SidebarLeft || position == "" || position == msgtypes.SidebarRight

	var mode sidebarLayoutMode
	switch {
	case sideBySide && p.width >= minWindowWidth && !p.sidebar.IsCollapsed():
		mode = sidebarVertical
	case sideBySide && p.width >= minWindowWidth:
		mode = sidebarCollapsed
	default:
		mode = sidebarCollapsedNarrow
	}

	l := sidebarLayout{
		mode:          mode,
		innerWidth:    innerWidth,
		sidebarOnLeft: position == msgtypes.SidebarLeft,
		bandAtBottom:  position == msgtypes.SidebarBottom,
	}

	switch mode {
	case sidebarVertical:
		l.sidebarWidth = p.sidebar.ClampWidth(p.sidebar.GetPreferredWidth(), innerWidth)
		l.chatWidth = max(1, innerWidth-l.sidebarWidth)
		if l.sidebarOnLeft {
			l.sidebarStartX = 0
			l.handleX = l.sidebarWidth - toggleColumnWidth
			l.chatStartX = l.sidebarWidth
		} else {
			l.handleX = l.chatWidth
			l.sidebarStartX = l.chatWidth + toggleColumnWidth
		}
		l.chatHeight = max(1, p.height)
		l.sidebarHeight = l.chatHeight

	case sidebarCollapsed:
		l.sidebarWidth = innerWidth - toggleColumnWidth
		l.chatWidth = innerWidth
		l.sidebarHeight = p.sidebar.CollapsedHeight(l.sidebarWidth)
		l.chatHeight = max(1, p.height-l.sidebarHeight)

	case sidebarCollapsedNarrow:
		l.sidebarWidth = innerWidth
		l.chatWidth = innerWidth
		l.sidebarHeight = p.sidebar.CollapsedHeight(l.sidebarWidth)
		l.chatHeight = max(1, p.height-l.sidebarHeight)
	}

	return l
}

// KeyMap defines key bindings for the chat page
type KeyMap struct {
	Cancel          key.Binding
	ToggleSplitDiff key.Binding
	ToggleSidebar   key.Binding
}

// defaultKeyMap returns the default key bindings.
// ctrl+t is reserved for "new tab" in the tab bar,
// so ToggleSplitDiff is disabled (available via /split-diff command instead).
func defaultKeyMap() KeyMap {
	splitDiff := key.NewBinding(
		key.WithKeys("ctrl+t"),
		key.WithHelp("Ctrl+t", "toggle split diff"),
	)
	splitDiff.SetEnabled(false)

	return KeyMap{
		Cancel: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("Esc", "interrupt"),
		),
		ToggleSplitDiff: splitDiff,
		ToggleSidebar: key.NewBinding(
			key.WithKeys("ctrl+b"),
			key.WithHelp("Ctrl+b", "toggle sidebar"),
		),
	}
}

// New creates a new chat page
func New(ar *animation.Runtime, ctx context.Context, a *app.App, sessionState *service.SessionState, opts ...PageOption) Page {
	p := &chatPage{
		ar:                ar,
		ctx:               func() context.Context { return context.WithoutCancel(ctx) },
		sidebar:           sidebar.New(ar, ctx, sessionState),
		messages:          messages.New(ar, sessionState),
		app:               a,
		keyMap:            defaultKeyMap(),
		commandParser:     commands.NewParser(),
		sessionState:      sessionState,
		showStartupBanner: true,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// PageOption configures a chat page.
type PageOption func(*chatPage)

// WithLeanMode creates a lean chat page with no sidebar.
func WithLeanMode() PageOption {
	return func(p *chatPage) {
		p.leanMode = true
	}
}

// WithHideSidebar hides the sidebar without enabling lean mode.
// The sidebar cannot be re-shown via the TUI.
func WithHideSidebar() PageOption {
	return func(p *chatPage) {
		p.hideSidebar = true
		p.keyMap.ToggleSidebar.SetEnabled(false)
	}
}

// WithShowBanner controls whether the ASCII-art startup banner is drawn on
// an empty conversation.
func WithShowBanner(show bool) PageOption {
	return func(p *chatPage) {
		p.hideBanner = !show
	}
}

// WithCommandParser injects a command parser for handling slash commands in the editor.
func WithCommandParser(p *commands.Parser) PageOption {
	return func(cp *chatPage) {
		cp.commandParser = p
	}
}

// WithLayoutSettings applies initial layout customization (sidebar position,
// section spacing, section visibility, agent info mode, and agent filtering).
func WithLayoutSettings(settings msgtypes.LayoutSettings) PageOption {
	return func(p *chatPage) {
		p.layoutSettings = settings
		p.sidebar.SetSectionVisibility(sectionVisibility(settings))
		p.sidebar.SetSectionGap(settings.SectionSpacing.BlankLines())
		p.sidebar.SetAgentInfoMode(agentInfoMode(settings.SidebarInfoMode))
		p.sidebar.SetActiveAgentsOnly(settings.ActiveAgentsOnly)
	}
}

// WithSendMode sets the initial behavior of messages sent while the agent
// is working: steer into the ongoing stream or queue until the turn ends.
func WithSendMode(mode msgtypes.SendMode) PageOption {
	return func(p *chatPage) {
		p.sendMode = mode
	}
}

func WithInterruptMode(mode msgtypes.InterruptMode) PageOption {
	return func(p *chatPage) {
		p.interruptMode = mode
	}
}

// sectionVisibility maps layout settings to the sidebar's visibility config.
func sectionVisibility(settings msgtypes.LayoutSettings) sidebar.SectionVisibility {
	return sidebar.SectionVisibility{
		HideSessionPath: settings.HideSessionPath,
		HideUsage:       settings.HideUsage,
		HideAgents:      settings.HideAgents,
		HideTools:       settings.HideTools,
		HideTodos:       settings.HideTodos,
	}
}

// agentInfoMode maps the layout's sidebar info mode to the sidebar's
// agent-roster renderer selection; empty/unknown values fall back to compact.
func agentInfoMode(mode msgtypes.SidebarInfoMode) sidebar.AgentInfoMode {
	if msgtypes.ParseSidebarInfoMode(string(mode)) == msgtypes.InfoModeDetailed {
		return sidebar.AgentInfoDetailed
	}
	return sidebar.AgentInfoCompact
}

// Init initializes the chat page
func (p *chatPage) Init() tea.Cmd {
	var cmds []tea.Cmd

	cmds = append(cmds,
		p.sidebar.Init(),
		p.messages.Init(),
	)

	// Load state from existing session (for session restore and branching)
	if sess := p.app.Session(); sess != nil {
		p.sidebar.LoadFromSession(sess)
		if len(sess.Messages) > 0 {
			cmds = append(cmds, p.messages.LoadFromSession(sess))
		}
	}

	return tea.Batch(cmds...)
}

// Update handles messages and updates the page state
func (p *chatPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	model, cmd := p.update(msg)
	// State changes (async sidebar updates, streaming indicators) can move
	// child components without any resize. Child positions are only applied
	// in SetSize, so reapply the geometry when the live layout drifted from
	// the last applied one; otherwise mouse hit-testing stays offset until
	// the next window resize.
	if relayout := p.relayoutIfNeeded(); relayout != nil {
		cmd = tea.Batch(cmd, relayout)
	}
	return model, cmd
}

// relayoutIfNeeded reapplies the current geometry when the computed layout no
// longer matches the one last pushed to child components.
func (p *chatPage) relayoutIfNeeded() tea.Cmd {
	if p.width <= 0 || p.height <= 0 {
		return nil
	}
	if p.computeSidebarLayout() == p.appliedLayout {
		return nil
	}
	return p.SetSize(p.width, p.height)
}

func (p *chatPage) update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Timers armed by a previous Update were dispatched by its caller (either
	// through the returned command or via TakeRoutedTimers); only this
	// update's timers may be collected after it.
	p.pendingTimers = nil
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := p.SetSize(msg.Width, msg.Height)
		return p, cmd

	case tea.KeyPressMsg:
		return p.handleKeyPress(msg)

	case tea.MouseClickMsg:
		return p.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return p.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return p.handleMouseRelease(msg)

	case msgtypes.WheelCoalescedMsg:
		return p.handleWheelCoalesced(msg)

	case msgtypes.StreamCancelledMsg:
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)

		// Forward to sidebar to stop its spinners
		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)

		var cmds []tea.Cmd
		cmds = append(cmds, cmd, sidebarCmd)

		if msg.ShowMessage {
			cmds = append(cmds, p.messages.AddCancelledMessage())
		}
		cmds = append(cmds, p.messages.ScrollToBottom())

		// Process next queued message after cancel (queue is preserved)
		if queueCmd := p.processNextQueuedMessage(); queueCmd != nil {
			cmds = append(cmds, queueCmd)
		}

		return p, tea.Batch(cmds...)

	case msgtypes.EditUserMessageMsg:
		return p.handleEditUserMessage(msg)

	case messages.InlineEditCommittedMsg:
		return p.handleInlineEditCommitted(msg)

	case messages.InlineEditCancelledMsg:
		return p.handleInlineEditCancelled(msg)

	case msgtypes.SendMsg:
		slog.Debug(msg.Content)
		return p.handleSendMsg(msg)

	case steerSentMsg:
		return p, notification.InfoCmd("Message sent to the working agent · /settings to queue instead")

	case steerFailedMsg:
		// The steer queue rejected the message (full, or the stream just
		// stopped). Fall back to the explicit queue path, which also runs
		// the message immediately when the agent is no longer working.
		msg.original.Queue = true
		model, cmd := p.handleSendMsg(msg.original)
		return model, tea.Batch(
			notification.WarningCmd("Could not attach the message to the running stream"),
			cmd,
		)

	case followUpSentMsg:
		return p, notification.InfoCmd("Follow-up queued for the next turn")

	case followUpFailedMsg:
		msg.original.FollowUp = false
		msg.original.Queue = true
		model, cmd := p.handleSendMsg(msg.original)
		return model, tea.Batch(
			notification.WarningCmd("Could not enqueue the follow-up"),
			cmd,
		)

	case msgtypes.RetryMsg:
		return p.handleRetry()

	case msgtypes.ToggleHideToolResultsMsg:
		// Forward to messages component to invalidate cache and trigger redraw
		model, cmd := p.messages.Update(messages.ToggleHideToolResultsMsg{})
		p.messages = model.(messages.Model)
		return p, cmd

	case msgtypes.ClearQueueMsg:
		return p.handleClearQueue()

	case msgtypes.ThemeChangedMsg:
		// Theme changed - forward to all child components to invalidate caches
		var cmds []tea.Cmd

		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		cmds = append(cmds, cmd)

		// Forward to sidebar to ensure it picks up new theme colors
		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)
		cmds = append(cmds, sidebarCmd)

		return p, tea.Batch(cmds...)

	case dialog.InterruptConfirmedMsg:
		cmd := p.cancelStream(true)
		return p, cmd

	default:
		// Try to handle as a runtime event
		if handled, cmd := p.handleRuntimeEvent(msg); handled {
			return p, cmd
		}
	}

	sidebarModel, sidebarCmd := p.sidebar.Update(msg)
	p.sidebar = sidebarModel.(sidebar.Model)

	chatModel, chatCmd := p.messages.Update(msg)
	p.messages = chatModel.(messages.Model)

	return p, tea.Batch(sidebarCmd, chatCmd)
}

func (p *chatPage) setWorking(working bool) tea.Cmd {
	wasWorking := p.working
	p.working = working

	if working != wasWorking {
		return core.CmdHandler(msgtypes.WorkingStateChangedMsg{
			Working:     working,
			QueueLength: len(p.messageQueue),
		})
	}

	return nil
}

// setPendingResponse adds or removes the pending-response spinner message
// inside the messages component. When starting, it adds a spinner message to
// the scrollable list; when stopping, it explicitly removes any lingering spinner.
func (p *chatPage) setPendingResponse(pending bool) tea.Cmd {
	if pending {
		sender, label := p.pendingSpinnerContext()
		return p.messages.AddAssistantMessage(sender, label)
	}
	p.messages.RemoveSpinner()
	return nil
}

// pendingSpinnerContext labels the waiting spinner during delegation only.
// Depth < 2 → empty (default playful spinner); nested → child + "parent → child".
func (p *chatPage) pendingSpinnerContext() (sender, label string) {
	n := len(p.agentStack)
	if n < 2 {
		return "", ""
	}
	child := p.agentStack[n-1]
	return child, p.agentStack[n-2] + " → " + child
}

// renderCollapsedSidebar renders the sidebar in collapsed/band mode.
// A top band carries its divider on the last line; a bottom band on the first.
func (p *chatPage) renderCollapsedSidebar(sl sidebarLayout) string {
	// Guard against unset/invalid layout (can happen before WindowSizeMsg is received).
	width := max(0, sl.innerWidth)
	height := max(0, sl.sidebarHeight)
	if width == 0 || height == 0 {
		return ""
	}

	sidebarView := p.sidebar.View()
	sidebarLines := strings.Split(sidebarView, "\n")

	// Place toggle glyph at the far right of the first line
	if sl.showToggle() && sl.mode != sidebarVertical && len(sidebarLines) > 0 {
		toggleGlyph := styles.MutedStyle.Render("«")
		glyphW := lipgloss.Width(toggleGlyph)
		padded := lipgloss.NewStyle().Width(width - glyphW).Render(sidebarLines[0])
		sidebarLines[0] = padded + toggleGlyph
	}

	divider := styles.FadingStyle.Render(strings.Repeat("─", width))
	switch {
	case sl.bandAtBottom:
		sidebarLines = append([]string{divider}, sidebarLines...)
		if len(sidebarLines) > height {
			sidebarLines = sidebarLines[:height]
		}
	case len(sidebarLines) >= height:
		sidebarLines[height-1] = divider
	default:
		sidebarLines = append(sidebarLines, divider)
	}

	sidebarWithDivider := strings.Join(sidebarLines, "\n")

	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		Align(lipgloss.Left, lipgloss.Top).
		Render(sidebarWithDivider)
}

func (p *chatPage) messagesView(sl sidebarLayout) string {
	messagesView := p.messages.View()
	if messagesView != "" || !p.showStartupBanner || p.hideBanner {
		return messagesView
	}
	if sl.chatWidth < tuibanner.Width || sl.chatHeight < tuibanner.Height {
		return ""
	}

	banner := styles.BaseStyle.Foreground(styles.Accent).Render(strings.Join(tuibanner.Lines, "\n"))
	return lipgloss.Place(
		sl.chatWidth,
		sl.chatHeight,
		lipgloss.Center,
		lipgloss.Center,
		banner,
	)
}

// View renders the chat page (messages + sidebar only, no editor or resize handle)
func (p *chatPage) View() string {
	sl := p.computeSidebarLayout()

	messagesView := p.messagesView(sl)

	var bodyContent string

	switch sl.mode {
	case sidebarVertical:
		chatView := styles.ChatStyle.
			Height(sl.chatHeight).
			Width(sl.chatWidth).
			Render(messagesView)

		toggleCol := p.renderSidebarHandle(sl.chatHeight)

		sidebarView := lipgloss.NewStyle().
			Width(sl.sidebarWidth-toggleColumnWidth).
			Height(sl.chatHeight).
			Align(lipgloss.Left, lipgloss.Top).
			Render(p.sidebar.View())

		if sl.sidebarOnLeft {
			bodyContent = lipgloss.JoinHorizontal(lipgloss.Left, sidebarView, toggleCol, chatView)
		} else {
			bodyContent = lipgloss.JoinHorizontal(lipgloss.Left, chatView, toggleCol, sidebarView)
		}

	case sidebarCollapsed, sidebarCollapsedNarrow:
		switch {
		case p.leanMode:
			// Lean mode: no sidebar header, no fixed height
			bodyContent = styles.ChatStyle.
				Width(sl.innerWidth).
				Render(messagesView)
		case p.hideSidebar:
			// Sidebar hidden: chat fills the full height, no sidebar header.
			bodyContent = styles.ChatStyle.
				Height(sl.chatHeight).
				Width(sl.innerWidth).
				Render(messagesView)
		default:
			sidebarRendered := p.renderCollapsedSidebar(sl)
			chatView := styles.ChatStyle.
				Height(sl.chatHeight).
				Width(sl.innerWidth).
				Render(messagesView)
			if sl.bandAtBottom {
				bodyContent = lipgloss.JoinVertical(lipgloss.Top, chatView, sidebarRendered)
			} else {
				bodyContent = lipgloss.JoinVertical(lipgloss.Top, sidebarRendered, chatView)
			}
		}
	}

	appStyle := styles.AppStyle
	if !p.leanMode {
		appStyle = appStyle.Height(p.height)
	}
	return appStyle.Render(bodyContent)
}

// renderSidebarHandle renders the sidebar toggle/resize handle.
// The glyph points toward the edge the sidebar collapses to and flips when
// the sidebar sits on the left.
func (p *chatPage) renderSidebarHandle(height int) string {
	lines := make([]string, height)

	expandGlyph, collapseGlyph := "«", "»"
	if p.layoutSettings.SidebarPosition == msgtypes.SidebarLeft {
		expandGlyph, collapseGlyph = "»", "«"
	}

	glyph := collapseGlyph
	if p.sidebar.IsCollapsed() {
		glyph = expandGlyph
	}
	lines[0] = styles.MutedStyle.Render(glyph)
	for i := 1; i < height; i++ {
		lines[i] = " "
	}

	return strings.Join(lines, "\n")
}

func (p *chatPage) SetSize(width, height int) tea.Cmd {
	p.width = width
	p.height = height

	var cmds []tea.Cmd

	// Compute layout once and use it for all sizing
	sl := p.computeSidebarLayout()
	p.appliedLayout = sl

	switch sl.mode {
	case sidebarVertical:
		p.sidebar.SetMode(sidebar.ModeVertical)
		p.sidebar.SetMirroredPadding(sl.sidebarOnLeft)
		cmds = append(cmds,
			p.sidebar.SetSize(sl.sidebarWidth-toggleColumnWidth, sl.chatHeight),
			p.sidebar.SetPosition(styles.AppPadding+sl.sidebarStartX, 0),
			p.messages.SetPosition(styles.AppPadding+sl.chatStartX, 0),
		)
	case sidebarCollapsed, sidebarCollapsedNarrow:
		p.sidebar.SetMode(sidebar.ModeCollapsed)
		p.sidebar.SetMirroredPadding(false)
		messagesY := sl.sidebarHeight
		if sl.bandAtBottom {
			messagesY = 0
		}
		cmds = append(cmds,
			p.sidebar.SetSize(sl.sidebarWidth, sl.sidebarHeight),
			p.sidebar.SetPosition(styles.AppPadding, sl.bandY()),
			p.messages.SetPosition(styles.AppPadding, messagesY),
		)
	}

	cmds = append(cmds, p.messages.SetSize(sl.chatWidth, sl.chatHeight))

	return tea.Batch(cmds...)
}

// GetSize returns the current dimensions
func (p *chatPage) GetSize() (width, height int) {
	return p.width, p.height
}

// Bindings returns key bindings for the chat page
func (p *chatPage) Bindings() []key.Binding {
	return p.messages.Bindings()
}

// Help returns help information
func (p *chatPage) Help() help.KeyMap {
	return core.NewSimpleHelp(p.Bindings())
}

// cancelStream cancels the current stream and cleans up associated state
func (p *chatPage) cancelStream(showCancelMessage bool) tea.Cmd {
	if p.msgCancel == nil {
		return nil
	}

	p.msgCancel()
	p.msgCancel = nil
	p.streamCancelled = true
	p.streamDepth = 0
	p.agentStack = nil
	p.waitingForDoubleTap = false
	p.setPendingResponse(false)
	// Send StreamCancelledMsg to all components to handle cleanup
	return tea.Batch(
		core.CmdHandler(msgtypes.StreamCancelledMsg{ShowMessage: showCancelMessage}),
		p.setWorking(false),
	)
}

func isBangCommand(content string) bool {
	return strings.HasPrefix(content, "!")
}

// handleInterrupt processes an Esc key press during a running stream.
// The behavior depends on interruptMode:
//   - "always": opens a confirmation dialog
//   - "double-tap": requires two Esc presses within 500ms
//   - "none": cancels immediately
func (p *chatPage) handleInterrupt() tea.Cmd {
	switch p.interruptMode {
	case "double-tap":
		now := time.Now()
		if !p.lastInterruptTime.IsZero() && now.Sub(p.lastInterruptTime) <= time.Second {
			p.lastInterruptTime = time.Time{}
			p.waitingForDoubleTap = false
			return p.cancelStream(true)
		}
		p.lastInterruptTime = now
		p.waitingForDoubleTap = true
		return nil
	case "none":
		return p.cancelStream(true)
	default:
		return func() tea.Msg {
			return dialog.OpenDialogMsg{Model: dialog.NewInterruptConfirmationDialog()}
		}
	}
}

func (p *chatPage) parseImmediateCommand(content string) tea.Cmd {
	if p.commandParser == nil {
		return nil
	}
	return p.commandParser.Parse(content)
}

// handleSendMsg handles incoming messages from the editor. Depending on
// state they are processed immediately, steered into the ongoing stream, or
// queued until the current turn ends.
func (p *chatPage) handleSendMsg(msg msgtypes.SendMsg) (layout.Model, tea.Cmd) {
	// Handle "exit", "quit", and ":q" as special keywords to quit the session
	// immediately, equivalent to the /exit slash command.
	switch strings.TrimSpace(msg.Content) {
	case "exit", "quit", ":q":
		return p, core.CmdHandler(msgtypes.ExitSessionMsg{})
	}

	// Immediate UI slash commands (e.g. /exit, /compact) run even in read-only
	// mode. A BypassQueue message has already been resolved (e.g. an agent
	// command or fork-mode skill re-dispatching itself) and must skip parsing:
	// re-parsing would match the same command again and loop forever.
	if !msg.BypassQueue {
		if cmd := p.parseImmediateCommand(msg.Content); cmd != nil {
			return p, cmd
		}
	}

	// Everything below hands work to the model, which read-only sessions must
	// reject: normal input, bang commands, and resolved agent/skill commands
	// flagged BypassQueue.
	if p.app != nil && p.app.IsReadOnly() {
		return p, notification.WarningCmd("Session is read-only. No new messages can be sent.")
	}

	if msg.BypassQueue || isBangCommand(msg.Content) {
		cmd := p.processMessage(msg)
		return p, cmd
	}

	// If not working, process immediately
	if !p.working {
		cmd := p.processMessage(msg)
		return p, cmd
	}

	// Alt+Enter explicitly requests a separate end-of-turn follow-up. When the
	// agent is idle there is no active turn to follow, so process it normally.
	if msg.FollowUp && p.working && p.app != nil {
		cmd := p.followUpMessage(msg)
		return p, cmd
	}

	// While the agent is working, the configured send mode decides the
	// default: steer injects the message into the ongoing stream so the
	// agent picks it up mid-turn without breaking the stream (issue #3547);
	// queue holds it until the turn ends. Queue-flagged messages (internal
	// fallbacks) always queue, and so do fork-mode skills — they spawn
	// their own stream, which cannot attach to the running one.
	if msg.Queue || p.app == nil || p.sendMode == msgtypes.SendModeQueue || p.isForkSkillCommand(msg.Content) {
		cmd := p.enqueueMessage(msg)
		return p, cmd
	}

	cmd := p.steerMessage(msg)
	return p, cmd
}

// isForkSkillCommand reports whether content invokes a fork-mode skill.
func (p *chatPage) isForkSkillCommand(content string) bool {
	_, _, ok := p.app.SkillCommandFork(p.ctx(), content)
	return ok
}

// enqueueMessage appends the message to the local queue consumed when the
// current stream stops, and reports the result to the user.
func (p *chatPage) enqueueMessage(msg msgtypes.SendMsg) tea.Cmd {
	// If queue is full, reject the message
	if len(p.messageQueue) >= maxQueuedMessages {
		return notification.WarningCmd(fmt.Sprintf("Queue full (max %d messages). Please wait.", maxQueuedMessages))
	}

	// Add to queue
	p.messageQueue = append(p.messageQueue, queuedMessage{
		content:     msg.Content,
		attachments: msg.Attachments,
	})
	p.syncQueueToSidebar()

	queueLen := len(p.messageQueue)
	notifyMsg := fmt.Sprintf("Message queued (%d waiting) · Ctrl+X to clear", queueLen)

	return notification.InfoCmd(notifyMsg)
}

// steerSentMsg reports that a message was handed to the runtime's steer
// queue; steerFailedMsg carries the message back for local queueing when
// steering was rejected (e.g. steer queue full).
type (
	steerSentMsg      struct{}
	steerFailedMsg    struct{ original msgtypes.SendMsg }
	followUpSentMsg   struct{}
	followUpFailedMsg struct{ original msgtypes.SendMsg }
)

// steerMessage injects the message into the ongoing stream via the runtime's
// steer queue. Command resolution runs inside the command goroutine so slow
// skill/agent command expansion never blocks the UI. The transcript bubble is
// added when the runtime drains the message and emits its UserMessageEvent,
// which is the moment the agent actually sees it.
func (p *chatPage) steerMessage(msg msgtypes.SendMsg) tea.Cmd {
	ctx := p.ctx()
	return func() tea.Msg {
		content := p.app.ResolveInput(ctx, msg.Content)
		if err := p.app.SteerMessage(ctx, content, msg.Attachments); err != nil {
			slog.Warn("Failed to steer message; falling back to queue", "error", err)
			return steerFailedMsg{original: msg}
		}
		return steerSentMsg{}
	}
}

func (p *chatPage) followUpMessage(msg msgtypes.SendMsg) tea.Cmd {
	ctx := p.ctx()
	return func() tea.Msg {
		content := p.app.ResolveInput(ctx, msg.Content)
		if err := p.app.FollowUpMessage(ctx, content, msg.Attachments); err != nil {
			slog.Warn("Failed to enqueue follow-up; falling back to local queue", "error", err)
			return followUpFailedMsg{original: msg}
		}
		return followUpSentMsg{}
	}
}

func (p *chatPage) handleEditUserMessage(msg msgtypes.EditUserMessageMsg) (layout.Model, tea.Cmd) {
	if msg.SessionPosition < 0 || msg.MsgIndex < 0 {
		return p, nil
	}

	p.editing = true
	p.branchAtPosition = msg.SessionPosition

	// Extract any attachments from the original session message
	p.editAttachments = p.extractAttachmentsFromSession(msg.SessionPosition)

	// Start inline editing in the messages component.
	// Request focus switch to messages panel so the parent blurs the editor.
	editCmd := p.messages.StartInlineEdit(msg.MsgIndex, msg.SessionPosition, msg.OriginalContent)
	focusCmd := core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelMessages})

	return p, tea.Batch(editCmd, focusCmd)
}

// handleInlineEditCommitted handles the commit of an inline edit, triggering a branch.
func (p *chatPage) handleInlineEditCommitted(msg messages.InlineEditCommittedMsg) (layout.Model, tea.Cmd) {
	if !p.editing {
		return p, nil
	}

	p.editing = false
	branchPosition := p.branchAtPosition
	p.branchAtPosition = 0
	attachments := p.editAttachments
	p.editAttachments = nil

	var cancelCmd tea.Cmd
	if p.msgCancel != nil {
		cancelCmd = p.cancelStream(false)
	}

	p.messageQueue = nil
	p.syncQueueToSidebar()

	parentID := ""
	if sess := p.app.Session(); sess != nil {
		parentID = sess.ID
	}

	branchCmd := core.CmdHandler(msgtypes.BranchFromEditMsg{
		ParentSessionID:  parentID,
		BranchAtPosition: branchPosition,
		Content:          msg.Content,
		Attachments:      attachments,
	})

	return p, tea.Batch(cancelCmd, branchCmd)
}

// handleInlineEditCancelled handles cancellation of an inline edit.
func (p *chatPage) handleInlineEditCancelled(msg messages.InlineEditCancelledMsg) (layout.Model, tea.Cmd) {
	p.editing = false
	p.branchAtPosition = 0
	p.editAttachments = nil

	if msg.WasInSelectionMode {
		// We were in keyboard selection mode before editing, stay in the messages panel.
		// The messages component already restored its selection state.
		return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelMessages})
	}
	// We weren't in selection mode, return focus to the editor.
	return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor})
}

// extractAttachmentsFromSession extracts attachments from a session message at the given position.
// Legacy attachments are stored as text parts in MultiContent with format "Contents of <filename>: <dataURL>".
// New attachments are stored as Document parts.
func (p *chatPage) extractAttachmentsFromSession(position int) []msgtypes.Attachment {
	sess := p.app.Session()
	if sess == nil || position < 0 || position >= len(sess.Messages) {
		return nil
	}

	item := sess.Messages[position]
	if !item.IsMessage() || item.Message == nil {
		return nil
	}

	msg := item.Message.Message
	if len(msg.MultiContent) <= 1 {
		// No attachments - only the main text content or nothing
		return nil
	}

	var attachments []msgtypes.Attachment
	const legacyPrefix = "Contents of "

	// Skip the first part (main text content), look for attachment parts
	for i := 1; i < len(msg.MultiContent); i++ {
		part := msg.MultiContent[i]

		if part.Type == chat.MessagePartTypeDocument && part.Document != nil {
			content := part.Document.Source.InlineText
			data := part.Document.Source.InlineData
			mimeType := part.Document.MimeType

			if content != "" || len(data) > 0 {
				attachments = append(attachments, msgtypes.Attachment{
					Name:     part.Document.Name,
					Content:  content,
					MimeType: mimeType,
					Data:     data,
				})
			}
			continue
		}

		if part.Type != chat.MessagePartTypeText {
			continue
		}
		text := part.Text
		if !strings.HasPrefix(text, legacyPrefix) {
			continue
		}
		// Parse "Contents of <filename>: <dataURL>"
		rest := text[len(legacyPrefix):]
		before, after, ok := strings.Cut(rest, ": ")
		if !ok {
			continue
		}
		filename := before
		content := after
		if filename != "" && content != "" {
			attachments = append(attachments, msgtypes.Attachment{
				Name:    filename,
				Content: content,
			})
		}
	}

	return attachments
}

// processNextQueuedMessage pops the next message from the queue and processes it.
// Returns nil if the queue is empty.
func (p *chatPage) processNextQueuedMessage() tea.Cmd {
	if len(p.messageQueue) == 0 {
		return nil
	}

	// Pop the first message from the queue
	queued := p.messageQueue[0]
	p.messageQueue[0] = queuedMessage{} // zero out to allow GC
	p.messageQueue = p.messageQueue[1:]
	p.syncQueueToSidebar()

	msg := msgtypes.SendMsg{
		Content:     queued.content,
		Attachments: queued.attachments,
	}

	return p.processMessage(msg)
}

// handleClearQueue clears all queued messages and shows a notification.
func (p *chatPage) handleClearQueue() (layout.Model, tea.Cmd) {
	count := len(p.messageQueue)
	if count == 0 {
		return p, notification.InfoCmd("No messages queued")
	}

	p.messageQueue = nil
	p.syncQueueToSidebar()

	var msg string
	if count == 1 {
		msg = "Cleared 1 queued message"
	} else {
		msg = fmt.Sprintf("Cleared %d queued messages", count)
	}
	return p, notification.SuccessCmd(msg)
}

// syncQueueToSidebar updates the sidebar with truncated previews of queued messages.
func (p *chatPage) syncQueueToSidebar() {
	previews := make([]string, len(p.messageQueue))
	for i, qm := range p.messageQueue {
		// Take first line and limit length for preview
		content := strings.TrimSpace(qm.content)
		if idx := strings.IndexAny(content, "\n\r"); idx != -1 {
			content = content[:idx]
		}
		previews[i] = content
	}
	p.sidebar.SetQueuedMessages(previews...)
}

// processMessage processes a message with the runtime
func (p *chatPage) processMessage(msg msgtypes.SendMsg) tea.Cmd {
	// Handle slash commands (e.g., /eval, /compact, /exit) BEFORE cancelling any ongoing stream.
	// These are UI commands that shouldn't interrupt the running agent.
	if !msg.BypassQueue {
		if cmd := p.parseImmediateCommand(msg.Content); cmd != nil {
			return cmd
		}
	}

	if isBangCommand(msg.Content) {
		p.app.RunBangCommand(p.ctx(), msg.Content[1:])
		return p.messages.ScrollToBottom()
	}

	if p.msgCancel != nil {
		p.msgCancel()
	}

	p.streamDepth = 0
	p.agentStack = nil
	p.sidebar.ResetStreamTracking()

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(p.ctx())

	// Start working state immediately to show the user something is happening.
	// This provides visual feedback while the runtime loads tools and prepares the stream.
	spinnerCmd := p.setWorking(true)
	// Check if this is an agent command that needs resolution
	// If so, show a loading message with the command description
	var loadingCmd tea.Cmd
	if strings.HasPrefix(msg.Content, "/") {
		cmdName, _, _ := strings.Cut(msg.Content[1:], " ")
		if cmd, found := p.app.CurrentAgentCommands(ctx)[cmdName]; found {
			loadingCmd = p.messages.AddLoadingMessage(cmd.DisplayText())
		}
	}

	// Run command resolution and agent execution in a goroutine
	// so the UI stays responsive while skill/agent commands are resolved.
	go func() {
		if skillName, task, ok := p.app.SkillCommandFork(ctx, msg.Content); ok {
			// Fork-mode skill: run in an isolated sub-session.
			p.app.RunSkillFork(ctx, p.msgCancel, skillName, task, msg.Attachments)
			return
		}
		p.app.Run(ctx, p.msgCancel, p.app.ResolveInput(ctx, msg.Content), msg.Attachments)
	}()

	return tea.Batch(p.messages.ScrollToBottom(), spinnerCmd, loadingCmd)
}

// handleRetry re-runs the agent turn after an error, resuming the conversation
// from the current session state without adding a new user message.
func (p *chatPage) handleRetry() (layout.Model, tea.Cmd) {
	if p.app == nil || p.app.IsReadOnly() {
		return p, notification.WarningCmd("Session is read-only. No new messages can be sent.")
	}

	// Ignore retry requests while a turn is already in flight.
	if p.working {
		return p, nil
	}

	if p.msgCancel != nil {
		p.msgCancel()
	}

	p.streamDepth = 0
	p.agentStack = nil
	p.sidebar.ResetStreamTracking()

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(p.ctx())

	spinnerCmd := p.setWorking(true)
	p.app.Retry(ctx, p.msgCancel)

	return p, tea.Batch(p.messages.ScrollToBottom(), spinnerCmd)
}

// CompactSession generates a summary and compacts the session history
func (p *chatPage) CompactSession(additionalPrompt string) tea.Cmd {
	// Cancel any active stream without showing cancellation message
	cancelCmd := p.cancelStream(false)

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(p.ctx())
	p.app.CompactSession(ctx, p.msgCancel, additionalPrompt)

	return tea.Batch(
		cancelCmd,
		p.setWorking(true),
		p.setPendingResponse(true),
		p.messages.ScrollToBottom(),
	)
}

// SetSessionStarred updates the sidebar star indicator
func (p *chatPage) SetSessionStarred(starred bool) {
	p.sidebar.SetSessionStarred(starred)
}

func (p *chatPage) SetTitleRegenerating(regenerating bool) tea.Cmd {
	return p.sidebar.SetTitleRegenerating(regenerating)
}

// GetSidebarSettings returns the current sidebar display settings.
func (p *chatPage) GetSidebarSettings() SidebarSettings {
	return SidebarSettings{
		Collapsed:      p.sidebar.IsCollapsed(),
		PreferredWidth: p.sidebar.GetPreferredWidth(),
	}
}

// SetSidebarSettings applies sidebar display settings.
func (p *chatPage) SetSidebarSettings(settings SidebarSettings) {
	p.sidebar.SetCollapsed(settings.Collapsed)
	p.sidebar.SetPreferredWidth(settings.PreferredWidth)
}

// SetLayoutSettings applies layout customization and relayouts the page.
func (p *chatPage) SetLayoutSettings(settings msgtypes.LayoutSettings) tea.Cmd {
	p.layoutSettings = settings
	p.sidebar.SetSectionVisibility(sectionVisibility(settings))
	p.sidebar.SetSectionGap(settings.SectionSpacing.BlankLines())
	p.sidebar.SetAgentInfoMode(agentInfoMode(settings.SidebarInfoMode))
	p.sidebar.SetActiveAgentsOnly(settings.ActiveAgentsOnly)
	if p.width <= 0 || p.height <= 0 {
		return nil
	}
	return p.SetSize(p.width, p.height)
}

// SetSendMode sets the behavior of messages sent while the agent is working.
func (p *chatPage) SetSendMode(mode msgtypes.SendMode) {
	p.sendMode = mode
}

func (p *chatPage) SetInterruptMode(mode msgtypes.InterruptMode) {
	p.interruptMode = mode
}

func (p *chatPage) SetShowBanner(show bool) {
	p.hideBanner = !show
}

// SetRoutingID records the tab identity this page's routed UI timers are
// addressed to. See Page.SetRoutingID.
func (p *chatPage) SetRoutingID(id string) {
	p.routingID = id
}

// TakeRoutedTimers returns and clears the routed timer commands armed by the
// most recent Update. See Page.TakeRoutedTimers.
func (p *chatPage) TakeRoutedTimers() tea.Cmd {
	if len(p.pendingTimers) == 0 {
		return nil
	}
	cmd := tea.Batch(p.pendingTimers...)
	p.pendingTimers = nil
	return cmd
}

// scheduleTransferTimers arms the sidebar's one-shot presentation timers,
// addressed to this page: with a routing identity each expiry is wrapped in
// a messages.RoutedMsg so it lands on this page's tab even when another tab
// is active (or this one is hidden) by then; without one (standalone pages)
// the raw payload goes to the single active page. The commands are also
// recorded for TakeRoutedTimers so an update on a hidden page keeps its
// deadlines armed.
func (p *chatPage) scheduleTransferTimers(timers []sidebar.TransferTimer) tea.Cmd {
	if len(timers) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(timers))
	for _, timer := range timers {
		cmds = append(cmds, p.routedTimerCmd(timer))
	}
	cmd := tea.Batch(cmds...)
	p.pendingTimers = append(p.pendingTimers, cmd)
	return cmd
}

// routedTimerCmd schedules one timer, wrapping its payload in the page's
// routing envelope. The routing ID is captured by value: the command runs on
// a background goroutine and must not read page state later.
func (p *chatPage) routedTimerCmd(timer sidebar.TransferTimer) tea.Cmd {
	routingID := p.routingID
	if routingID == "" {
		return timer.Cmd()
	}
	return tea.Tick(timer.Duration, func(time.Time) tea.Msg {
		return msgtypes.RoutedMsg{SessionID: routingID, Inner: timer.Msg}
	})
}

func (p *chatPage) PointerTargetsMessages(x, _ int) bool {
	sl := p.computeSidebarLayout()
	return sl.mode != sidebarVertical || p.sidebar.IsCollapsed() || !sl.isInSidebar(x-styles.AppPadding)
}

// handleSidebarClickType checks what was clicked in the sidebar area.
// Returns the click type and, for ClickAgent, the agent name.
func (p *chatPage) handleSidebarClickType(x, y int) (sidebar.ClickResult, string) {
	adjustedX := x - styles.AppPadding
	sl := p.computeSidebarLayout()

	switch sl.mode {
	case sidebarCollapsedNarrow, sidebarCollapsed:
		return p.sidebar.HandleClickType(adjustedX, sl.bandContentY(y))
	case sidebarVertical:
		if sl.isInSidebar(adjustedX) {
			return p.sidebar.HandleClickType(adjustedX-sl.sidebarStartX, y)
		}
	}

	return sidebar.ClickNone, ""
}

// routeMouseEvent routes mouse events to the appropriate component based on coordinates.
func (p *chatPage) routeMouseEvent(msg tea.Msg, _ int) tea.Cmd {
	sl := p.computeSidebarLayout()

	if sl.mode == sidebarVertical && !p.sidebar.IsCollapsed() {
		var x int
		switch m := msg.(type) {
		case tea.MouseClickMsg:
			x = m.X
		case tea.MouseMotionMsg:
			x = m.X
		case tea.MouseReleaseMsg:
			x = m.X
		}

		adjustedX := x - styles.AppPadding
		if sl.isInSidebar(adjustedX) {
			model, cmd := p.sidebar.Update(msg)
			p.sidebar = model.(sidebar.Model)
			return cmd
		}
	}

	model, cmd := p.messages.Update(msg)
	p.messages = model.(messages.Model)
	return cmd
}

// IsWorking returns whether the agent is currently working
func (p *chatPage) IsWorking() bool {
	return p.working
}

// IsInlineEditing returns true if a past user message is being edited inline.
func (p *chatPage) IsInlineEditing() bool {
	return p.messages.IsInlineEditing()
}

// IsSelecting returns true while a text-selection drag is active in the
// messages panel.
func (p *chatPage) IsSelecting() bool {
	return p.messages.IsSelecting()
}

// QueueLength returns the number of queued messages
func (p *chatPage) QueueLength() int {
	return len(p.messageQueue)
}

// FocusMessages gives focus to the messages panel
func (p *chatPage) FocusMessages() tea.Cmd {
	return p.messages.Focus()
}

// FocusMessageAt gives focus and selects the message at the given screen coordinates.
func (p *chatPage) FocusMessageAt(x, y int) tea.Cmd {
	return p.messages.FocusAt(x, y)
}

// BlurMessages removes focus from the messages panel
func (p *chatPage) BlurMessages() {
	p.messages.Blur()
}

// ScrollToBottom scrolls the messages viewport to the bottom if auto-scroll is active.
func (p *chatPage) ScrollToBottom() tea.Cmd {
	return p.messages.ScrollToBottom()
}
