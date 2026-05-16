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
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
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
	// sidebarVertical: wide window, sidebar on right side
	sidebarVertical sidebarLayoutMode = iota
	// sidebarCollapsed: wide window but user collapsed sidebar, shown at top with toggle
	sidebarCollapsed
	// sidebarCollapsedNarrow: narrow window, shown at top without toggle
	sidebarCollapsedNarrow
)

// sidebarLayout holds computed layout values for the current frame.
// Computing this once per update avoids repeating calculations across View, SetSize, and input handlers.
type sidebarLayout struct {
	mode          sidebarLayoutMode
	innerWidth    int // window width minus app padding
	chatWidth     int // width available for chat/messages
	sidebarWidth  int // actual sidebar width (varies by mode)
	sidebarStartX int // X coordinate where sidebar content starts (relative to innerWidth)
	handleX       int // X coordinate of resize handle column (only valid in vertical mode)
	chatHeight    int // height available for chat area
	sidebarHeight int // height of sidebar
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
	return adjustedX >= l.sidebarStartX
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
}

// queuedMessage represents a message waiting to be sent to the agent
type queuedMessage struct {
	content     string
	attachments []msgtypes.Attachment
}

// maxQueuedMessages is the maximum number of messages that can be queued
const maxQueuedMessages = 5

// chatPage implements Page
type chatPage struct {
	width, height int

	// Components
	sidebar  sidebar.Model
	messages messages.Model

	sessionState *service.SessionState

	// State
	working  bool
	leanMode bool

	msgCancel       context.CancelFunc
	streamCancelled bool
	streamDepth     int // nesting depth of active streams (incremented on StreamStarted, decremented on StreamStopped)
	streamStartTime time.Time

	// Track whether we've received content from an assistant response
	// Used by --exit-after-response to ensure we don't exit before receiving content
	hasReceivedAssistantContent bool

	// Message queue for enqueuing messages while agent is working
	messageQueue []queuedMessage

	// Editing state for branching sessions
	editing          bool
	branchAtPosition int
	editAttachments  []msgtypes.Attachment // Preserved attachments from original message

	// Key map
	keyMap KeyMap

	app *app.App

	// Command parser for handling slash commands in the editor
	commandParser *commands.Parser

	// Sidebar drag state
	isDraggingSidebar     bool // True while dragging the sidebar resize handle
	sidebarDragStartX     int  // X position when drag started
	sidebarDragStartWidth int  // Sidebar preferred width when drag started
	sidebarDragMoved      bool // True if mouse moved beyond threshold during drag
}

// computeSidebarLayout calculates the layout based on current state.
func (p *chatPage) computeSidebarLayout() sidebarLayout {
	innerWidth := p.width - appPaddingHorizontal

	// Lean mode: no sidebar at all
	if p.leanMode {
		return sidebarLayout{
			mode:       sidebarCollapsedNarrow,
			innerWidth: innerWidth,
			chatWidth:  innerWidth,
			chatHeight: max(1, p.height),
		}
	}

	var mode sidebarLayoutMode
	switch {
	case p.width >= minWindowWidth && !p.sidebar.IsCollapsed():
		mode = sidebarVertical
	case p.width >= minWindowWidth:
		mode = sidebarCollapsed
	default:
		mode = sidebarCollapsedNarrow
	}

	l := sidebarLayout{
		mode:       mode,
		innerWidth: innerWidth,
	}

	switch mode {
	case sidebarVertical:
		l.sidebarWidth = p.sidebar.ClampWidth(p.sidebar.GetPreferredWidth(), innerWidth)
		l.chatWidth = max(1, innerWidth-l.sidebarWidth)
		l.handleX = l.chatWidth
		l.sidebarStartX = l.chatWidth + toggleColumnWidth
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
func New(a *app.App, sessionState *service.SessionState, opts ...PageOption) Page {
	p := &chatPage{
		sidebar:       sidebar.New(sessionState),
		messages:      messages.New(sessionState),
		app:           a,
		keyMap:        defaultKeyMap(),
		commandParser: commands.NewParser(),
		sessionState:  sessionState,
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

// WithCommandParser injects a command parser for handling slash commands in the editor.
func WithCommandParser(p *commands.Parser) PageOption {
	return func(cp *chatPage) {
		cp.commandParser = p
	}
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
		return p.messages.AddAssistantMessage()
	}
	p.messages.RemoveSpinner()
	return nil
}

// renderCollapsedSidebar renders the sidebar in collapsed mode (at top of screen).
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

	// Replace the last line with a subtle divider
	divider := styles.FadingStyle.Render(strings.Repeat("─", width))
	if len(sidebarLines) >= height {
		sidebarLines[height-1] = divider
	} else {
		sidebarLines = append(sidebarLines, divider)
	}

	sidebarWithDivider := strings.Join(sidebarLines, "\n")

	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		Align(lipgloss.Left, lipgloss.Top).
		Render(sidebarWithDivider)
}

// View renders the chat page (messages + sidebar only, no editor or resize handle)
func (p *chatPage) View() string {
	sl := p.computeSidebarLayout()

	messagesView := p.messages.View()

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

		bodyContent = lipgloss.JoinHorizontal(lipgloss.Left, chatView, toggleCol, sidebarView)

	case sidebarCollapsed, sidebarCollapsedNarrow:
		if p.leanMode {
			// Lean mode: no sidebar header, no fixed height
			bodyContent = styles.ChatStyle.
				Width(sl.innerWidth).
				Render(messagesView)
		} else {
			sidebarRendered := p.renderCollapsedSidebar(sl)
			chatView := styles.ChatStyle.
				Height(sl.chatHeight).
				Width(sl.innerWidth).
				Render(messagesView)
			bodyContent = lipgloss.JoinVertical(lipgloss.Top, sidebarRendered, chatView)
		}
	}

	appStyle := styles.AppStyle
	if !p.leanMode {
		appStyle = appStyle.Height(p.height)
	}
	return appStyle.Render(bodyContent)
}

// renderSidebarHandle renders the sidebar toggle/resize handle.
// When collapsed: shows just « at top.
// When expanded: shows » at top, rest is empty space (draggable for resize).
func (p *chatPage) renderSidebarHandle(height int) string {
	lines := make([]string, height)

	if p.sidebar.IsCollapsed() {
		// Collapsed: just the toggle glyph, no vertical line
		lines[0] = styles.MutedStyle.Render("«")
		for i := 1; i < height; i++ {
			lines[i] = " "
		}
	} else {
		// Expanded: just the toggle at top, rest is empty space (still draggable)
		lines[0] = styles.MutedStyle.Render("»")
		for i := 1; i < height; i++ {
			lines[i] = " "
		}
	}

	return strings.Join(lines, "\n")
}

func (p *chatPage) SetSize(width, height int) tea.Cmd {
	p.width = width
	p.height = height

	var cmds []tea.Cmd

	// Compute layout once and use it for all sizing
	sl := p.computeSidebarLayout()

	switch sl.mode {
	case sidebarVertical:
		p.sidebar.SetMode(sidebar.ModeVertical)
		cmds = append(cmds,
			p.sidebar.SetSize(sl.sidebarWidth-toggleColumnWidth, sl.chatHeight),
			p.sidebar.SetPosition(styles.AppPadding+sl.sidebarStartX, 0),
			p.messages.SetPosition(styles.AppPadding, 0),
		)
	case sidebarCollapsed, sidebarCollapsedNarrow:
		p.sidebar.SetMode(sidebar.ModeCollapsed)
		cmds = append(cmds,
			p.sidebar.SetSize(sl.sidebarWidth, sl.sidebarHeight),
			p.sidebar.SetPosition(styles.AppPadding, 0),
			p.messages.SetPosition(styles.AppPadding, sl.sidebarHeight),
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
	p.setPendingResponse(false)
	// Send StreamCancelledMsg to all components to handle cleanup
	return tea.Batch(
		core.CmdHandler(msgtypes.StreamCancelledMsg{ShowMessage: showCancelMessage}),
		p.setWorking(false),
	)
}

// handleSendMsg handles incoming messages from the editor, either processing
// them immediately or queuing them if the agent is busy.
func (p *chatPage) handleSendMsg(msg msgtypes.SendMsg) (layout.Model, tea.Cmd) {
	// Handle "exit", "quit", and ":q" as special keywords to quit the session
	// immediately, equivalent to the /exit slash command.
	switch strings.TrimSpace(msg.Content) {
	case "exit", "quit", ":q":
		return p, core.CmdHandler(msgtypes.ExitSessionMsg{})
	}

	// Predefined slash commands (e.g., /yolo, /exit, /compact) execute immediately
	// even while the agent is working - they're UI commands that don't interrupt the stream.
	// Custom agent commands (defined in config) should still be queued.
	if p.commandParser.Parse(msg.Content) != nil {
		cmd := p.processMessage(msg)
		return p, cmd
	}

	// If not working, process immediately
	if !p.working {
		cmd := p.processMessage(msg)
		return p, cmd
	}

	// If queue is full, reject the message
	if len(p.messageQueue) >= maxQueuedMessages {
		return p, notification.WarningCmd(fmt.Sprintf("Queue full (max %d messages). Please wait.", maxQueuedMessages))
	}

	// Add to queue
	p.messageQueue = append(p.messageQueue, queuedMessage{
		content:     msg.Content,
		attachments: msg.Attachments,
	})
	p.syncQueueToSidebar()

	queueLen := len(p.messageQueue)
	notifyMsg := fmt.Sprintf("Message queued (%d waiting) · Ctrl+X to clear", queueLen)

	return p, notification.InfoCmd(notifyMsg)
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
// Attachments are stored as text parts in MultiContent with format "Contents of <filename>: <dataURL>".
// TODO(krisetto): meh we can store and retrieve attachments better in the session itself
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
	const prefix = "Contents of "

	// Skip the first part (main text content), look for attachment parts
	for i := 1; i < len(msg.MultiContent); i++ {
		part := msg.MultiContent[i]
		if part.Type != chat.MessagePartTypeText {
			continue
		}
		text := part.Text
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		// Parse "Contents of <filename>: <dataURL>"
		rest := text[len(prefix):]
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
	if cmd := p.commandParser.Parse(msg.Content); cmd != nil {
		return cmd
	}

	if p.msgCancel != nil {
		p.msgCancel()
	}

	p.streamDepth = 0

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(context.Background())

	if strings.HasPrefix(msg.Content, "!") {
		p.app.RunBangCommand(ctx, msg.Content[1:])
		return p.messages.ScrollToBottom()
	}

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
		p.app.Run(ctx, p.msgCancel, p.app.ResolveInput(ctx, msg.Content), msg.Attachments)
	}()

	return tea.Batch(p.messages.ScrollToBottom(), spinnerCmd, loadingCmd)
}

// CompactSession generates a summary and compacts the session history
func (p *chatPage) CompactSession(additionalPrompt string) tea.Cmd {
	// Cancel any active stream without showing cancellation message
	cancelCmd := p.cancelStream(false)

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(context.Background())
	p.app.CompactSession(ctx, additionalPrompt)

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

// handleSidebarClickType checks what was clicked in the sidebar area.
// Returns the click type and, for ClickAgent, the agent name.
func (p *chatPage) handleSidebarClickType(x, y int) (sidebar.ClickResult, string) {
	adjustedX := x - styles.AppPadding
	sl := p.computeSidebarLayout()

	switch sl.mode {
	case sidebarCollapsedNarrow, sidebarCollapsed:
		return p.sidebar.HandleClickType(adjustedX, y)
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
