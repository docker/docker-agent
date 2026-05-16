package messages

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// ToggleHideToolResultsMsg triggers hiding/showing tool results
type ToggleHideToolResultsMsg struct{}

type toggleableView interface {
	IsToggleLine(lineIdx int) bool
	Toggle()
}

// Model represents a chat message list component
type Model interface {
	layout.Model
	layout.Sizeable
	layout.Focusable
	layout.Help
	layout.Positionable

	AddUserMessage(content string) tea.Cmd
	AddLoadingMessage(description string) tea.Cmd
	ReplaceLoadingWithUser(content string, sessionPos int) tea.Cmd
	AddErrorMessage(content string) tea.Cmd
	AddAssistantMessage() tea.Cmd
	AddCancelledMessage() tea.Cmd
	AddWelcomeMessage(content string) tea.Cmd
	AddOrUpdateToolCall(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status types.ToolStatus) tea.Cmd
	AddToolResult(msg *runtime.ToolCallResponseEvent, status types.ToolStatus) tea.Cmd
	AppendToLastMessage(agentName, content string) tea.Cmd
	AppendReasoning(agentName, content string) tea.Cmd
	AddShellOutputMessage(content string) tea.Cmd
	LoadFromSession(sess *session.Session) tea.Cmd

	RemoveSpinner()
	ScrollToBottom() tea.Cmd
	AdjustBottomSlack(delta int)

	// IsScrollbarDragging returns true when the scrollbar thumb is being dragged.
	IsScrollbarDragging() bool

	// IsMouseOnScrollbar returns true when the given screen coordinates are on the scrollbar.
	IsMouseOnScrollbar(x, y int) bool

	// Inline editing methods
	StartInlineEdit(msgIndex, sessionPosition int, content string) tea.Cmd
	CancelInlineEdit() tea.Cmd
	IsInlineEditing() bool

	// FocusAt gives focus and selects the message at the given screen coordinates.
	// Falls back to the default Focus behavior if no message is found at that position.
	FocusAt(x, y int) tea.Cmd
}

// renderedItem represents a cached rendered message with position information
type renderedItem struct {
	view   string   // Cached rendered content
	lines  []string // Pre-split lines (avoids re-splitting on every rebuild)
	height int      // Height in lines
}

// blockIDCounter generates unique IDs for reasoning blocks.
var blockIDCounter atomic.Uint64

func nextBlockID() string {
	id := blockIDCounter.Add(1)
	return "block-" + strconv.FormatUint(id, 10)
}

// model implements Model
type model struct {
	messages []*types.Message
	views    []layout.Model
	width    int // Full width including scrollbar space
	height   int

	// Height tracking system fields
	scrollOffset      int                    // Current scroll position in lines
	bottomSlack       int                    // Extra blank lines added after content shrinks
	slackAnimationSub animation.Subscription // Subscription to animation ticks while slack > 0
	renderedLines     []string               // Cached rendered content as lines (avoids split/join per frame)
	renderedItems     map[int]renderedItem   // Cache of rendered items with positions
	urlSpans          *urlSpanCache          // Cached URL spans per rendered line
	lineOffsets       []int                  // Prefix-sum: lineOffsets[i] = starting global line of view i
	totalHeight       int                    // Total height of all content in lines
	renderDirty       bool                   // True when rendered content needs rebuild

	selection selectionState

	sessionState *service.SessionState
	scrollview   *scrollview.Model

	xPos, yPos int

	// User scroll state
	userHasScrolled bool // True when user manually scrolls away from bottom

	// Message selection state
	selectedMessageIndex int  // Index of selected message (-1 = no selection)
	focused              bool // Whether the messages component is focused

	// Inline editing state
	inlineEditMsgIndex      int            // Index of message being edited (-1 = not editing)
	inlineEditSessionPos    int            // Session position for branching
	inlineEditTextarea      textarea.Model // Textarea for inline editing
	inlineEditOriginal      string         // Original content (for cancel)
	inlineEditPrevSelection int            // Previous selection index before entering inline edit (-1 = was not in selection mode)

	// Hover state for showing copy button on assistant messages
	hoveredMessageIndex int // Index of message under mouse (-1 = none)

	// Hovered URL for underline-on-hover effect (nil = no URL hovered)
	hoveredURL *hoveredURL
}

// New creates a new message list component
func New(sessionState *service.SessionState) Model {
	return newModel(120, 24, sessionState)
}

// NewScrollableView creates a simple scrollable view for displaying messages in dialogs
// This is a lightweight version that doesn't require app or session state management
func NewScrollableView(width, height int, sessionState *service.SessionState) Model {
	return newModel(width, height, sessionState)
}

func newModel(width, height int, sessionState *service.SessionState) *model {
	sv := scrollview.New(
		scrollview.WithReserveScrollbarSpace(true),
	)
	sv.SetSize(width, height)
	return &model{
		width:                width,
		height:               height,
		renderedItems:        make(map[int]renderedItem),
		urlSpans:             newURLSpanCache(),
		sessionState:         sessionState,
		scrollview:           sv,
		selectedMessageIndex: -1,
		inlineEditMsgIndex:   -1,
		hoveredMessageIndex:  -1,
		renderDirty:          true,
	}
}

// Init initializes the component
func (m *model) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, view := range m.views {
		if cmd := view.Init(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// Update handles messages and updates the component state
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case messages.StreamCancelledMsg:
		m.removeSpinner()
		m.removePendingToolCallMessages()
		m.stopReasoningBlockAnimations()
		return m, nil

	case tea.WindowSizeMsg:
		cmds = append(cmds, m.SetSize(msg.Width, msg.Height))

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)

	case messages.WheelCoalescedMsg:
		m.scrollByWheel(msg.Delta)
		return m, nil

	case AutoScrollTickMsg:
		if m.selection.mouseButtonDown && m.selection.active {
			cmd := m.autoScroll()
			return m, cmd
		}
		return m, nil

	case DebouncedCopyMsg:
		cmd := m.handleDebouncedCopy(msg)
		return m, cmd

	case editfile.ToggleDiffViewMsg:
		m.invalidateAllItems()
		return m, nil

	case ToggleHideToolResultsMsg:
		m.sessionState.ToggleHideToolResults()
		m.invalidateAllItems()
		return m, nil

	case messages.ThemeChangedMsg:
		// Theme changed - invalidate all render caches
		m.invalidateAllItems()
		editfile.InvalidateCaches()
		for i, view := range m.views {
			updatedView, cmd := view.Update(msg)
			m.views[i] = updatedView
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case animation.TickMsg:
		// Invalidate render cache if there's animated content that needs redrawing.
		// This ensures fades, spinners, etc. actually update visually on each tick.
		if m.hasAnimatedContent() {
			m.renderDirty = true
		}
		// Fall through to forward tick to all views

	case tea.PasteMsg:
		// Insert paste content into the inline edit textarea
		if m.inlineEditMsgIndex >= 0 {
			m.inlineEditTextarea.InsertString(msg.Content)
			m.invalidateItem(m.inlineEditMsgIndex)
			m.renderDirty = true
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)
	}

	// Forward updates to all message views
	for i, view := range m.views {
		updatedView, cmd := view.Update(msg)
		m.views[i] = updatedView
		if cmd != nil {
			cmds = append(cmds, cmd)
			// Child state changed (e.g., spinner tick), invalidate render cache
			m.renderDirty = true
		}
	}

	// On animation ticks, decay leftover bottom slack and keep the slack
	// subscription in sync so empty lines don't persist after thinking text
	// fades out. Must run after children update so reasoning blocks have
	// applied their fade state, and before tui.go's HasActive() check so the
	// subscription is registered when the next tick is scheduled.
	if _, ok := msg.(animation.TickMsg); ok {
		cmds = append(cmds, m.handleAnimationTick())
	}

	return m, tea.Batch(cmds...)
}

func (m *model) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	if m.isMouseOnScrollbar(msg.X, msg.Y) {
		return m.handleScrollviewUpdate(msg)
	}

	if msg.Button != tea.MouseLeft {
		return m, nil
	}

	line, col := m.mouseToLineCol(msg.X, msg.Y)

	if msgIdx, localLine := m.globalLineToMessageLine(line); msgIdx >= 0 {
		// Check for toggleable blocks (e.g. reasoning block, collapsed long messages)
		if t, ok := m.views[msgIdx].(toggleableView); ok {
			if t.IsToggleLine(localLine) {
				t.Toggle()
				m.bottomSlack = 0
				m.invalidateItem(msgIdx)
				return m, nil
			}
		}

		if clicked, msg := m.isEditLabelClick(msgIdx, localLine, col); clicked {
			return m, core.CmdHandler(messages.EditUserMessageMsg{
				MsgIndex:        msgIdx,
				SessionPosition: *msg.SessionPosition,
				OriginalContent: msg.Content,
			})
		}

		if content, ok := m.codeBlockAt(msgIdx, localLine, col); ok {
			return m, copyTextToClipboard(content)
		}

		if m.isCopyLabelClick(msgIdx, localLine, col) {
			cmd := m.copyMessageToClipboard(msgIdx)
			return m, cmd
		}
	}

	clickCount := m.selection.detectClickType(line, col)

	switch clickCount {
	case 3: // Triple-click: select line
		m.selectLineAt(line)
		m.selection.pendingCopyID++ // Cancel any pending double-click copy
		cmd := m.copySelectionToClipboard()
		return m, cmd
	case 2: // Double-click: select word with debounced copy
		m.selectWordAt(line, col)
		cmd := m.scheduleDebouncedCopy()
		return m, cmd
	default: // Single click: start drag selection
		m.selection.start(line, col)
		m.selection.mouseY = msg.Y
		return m, nil
	}
}

// globalLineToMessageLine maps a global line index to (message index, local line within message).
// Returns (-1, -1) if the line doesn't correspond to any message.
func (m *model) globalLineToMessageLine(globalLine int) (msgIdx, localLine int) {
	m.ensureAllItemsRendered()

	if len(m.lineOffsets) == 0 || globalLine < 0 || globalLine >= m.totalHeight {
		return -1, -1
	}

	// Binary search: find the last view whose offset <= globalLine
	i := sort.Search(len(m.lineOffsets), func(i int) bool {
		return m.lineOffsets[i] > globalLine
	}) - 1

	if i < 0 || i >= len(m.views) {
		return -1, -1
	}

	item := m.renderItem(i, m.views[i])
	local := globalLine - m.lineOffsets[i]
	if local < item.height {
		return i, local
	}

	// globalLine falls in a separator gap between messages
	return -1, -1
}

func (m *model) handleMouseMotion(msg tea.MouseMotionMsg) (layout.Model, tea.Cmd) {
	if m.scrollview.IsDragging() {
		return m.handleScrollviewUpdate(msg)
	}

	if m.selection.mouseButtonDown && m.selection.active {
		line, col := m.mouseToLineCol(msg.X, msg.Y)
		m.selection.update(line, col)
		m.selection.mouseY = msg.Y
		cmd := m.autoScroll()
		return m, cmd
	}

	// Track hovered message for showing copy button on assistant messages
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	newHovered := -1
	if msgIdx, _ := m.globalLineToMessageLine(line); msgIdx >= 0 && msgIdx < len(m.messages) {
		if m.messages[msgIdx].Type == types.MessageTypeAssistant {
			newHovered = msgIdx
		}
	}
	if newHovered != m.hoveredMessageIndex {
		oldHovered := m.hoveredMessageIndex
		m.hoveredMessageIndex = newHovered
		if oldHovered >= 0 {
			m.invalidateItem(oldHovered)
		}
		if newHovered >= 0 {
			m.invalidateItem(newHovered)
		}
		m.renderDirty = true
	}

	// Track hovered URL for underline effect
	m.updateHoveredURL(line, col)

	return m, nil
}

func (m *model) handleMouseRelease(msg tea.MouseReleaseMsg) (layout.Model, tea.Cmd) {
	if updated, cmd := m.handleScrollviewUpdate(msg); cmd != nil {
		return updated, cmd
	}

	if msg.Button == tea.MouseLeft && m.selection.mouseButtonDown {
		if m.selection.active {
			line, col := m.mouseToLineCol(msg.X, msg.Y)
			m.selection.update(line, col)

			// If the mouse didn't move, this was a plain click — open URL if any
			if line == m.selection.startLine && col == m.selection.startCol {
				m.selection.clear()
				if url := m.urlAt(line, col); url != "" {
					return m, core.CmdHandler(messages.OpenURLMsg{URL: url})
				}
				return m, nil
			}

			m.selection.end()
			cmd := m.copySelectionToClipboard()
			return m, cmd
		}
		m.selection.end()
	}
	return m, nil
}

func (m *model) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	// Handle inline editing keys first
	if m.inlineEditMsgIndex >= 0 {
		// Check for newline insertion using key.Matches against the textarea's InsertNewline binding
		// This properly handles shift+enter and ctrl+j based on the configured keymap
		if key.Matches(msg, m.inlineEditTextarea.KeyMap.InsertNewline) {
			// Forward to textarea for newline insertion
			var cmd tea.Cmd
			m.inlineEditTextarea, cmd = m.inlineEditTextarea.Update(msg)
			m.invalidateItem(m.inlineEditMsgIndex)
			m.renderDirty = true
			return m, cmd
		}

		switch msg.Key().Code {
		case tea.KeyEnter:
			// Plain Enter commits the edit
			cmd := m.commitInlineEdit()
			return m, cmd
		case tea.KeyEscape:
			// Esc cancels the edit
			cmd := m.CancelInlineEdit()
			return m, cmd
		default:
			// Forward all other keys to the textarea
			var cmd tea.Cmd
			m.inlineEditTextarea, cmd = m.inlineEditTextarea.Update(msg)
			m.invalidateItem(m.inlineEditMsgIndex)
			m.renderDirty = true
			return m, cmd
		}
	}

	switch msg.String() {
	case "esc":
		m.clearSelection()
		return m, nil
	case "up", "k":
		if m.focused {
			cmd := m.selectPreviousMessage()
			return m, cmd
		} else {
			m.scrollUp()
		}
		return m, nil
	case "down", "j":
		if m.focused {
			cmd := m.selectNextMessage()
			return m, cmd
		} else {
			m.scrollDown()
		}
		return m, nil
	case "c":
		if m.focused && m.selectedMessageIndex >= 0 {
			cmd := m.copySelectedMessageToClipboard()
			return m, cmd
		}
		return m, nil
	case "e":
		if m.focused && m.selectedMessageIndex >= 0 {
			msg := m.messages[m.selectedMessageIndex]
			if msg.Type == types.MessageTypeUser && msg.SessionPosition != nil {
				return m, func() tea.Msg {
					return messages.EditUserMessageMsg{
						MsgIndex:        m.selectedMessageIndex,
						SessionPosition: *msg.SessionPosition,
						OriginalContent: msg.Content,
					}
				}
			}
		}
		return m, nil
	case "pgup":
		m.scrollPageUp()
		return m, nil
	case "pgdown":
		m.scrollPageDown()
		return m, nil
	case "home", "g":
		m.scrollToTop()
		return m, nil
	case "end", "G":
		m.scrollToBottom()
		return m, nil
	}
	return m, nil
}

func (m *model) View() string {
	if len(m.messages) == 0 {
		return ""
	}

	m.updateScrollState()
	// Release the slack subscription once it's no longer needed. Starting it
	// is only done from Update via handleAnimationTick, where the returned
	// tea.Cmd can be propagated to actually schedule the next tick.
	if m.bottomSlack == 0 {
		m.slackAnimationSub.Stop()
	}

	if m.totalHeight == 0 {
		return ""
	}

	// Use cached lines directly - O(1) instead of O(totalHeight) split
	totalLines := len(m.renderedLines) + m.bottomSlack
	if totalLines == 0 {
		return ""
	}

	startLine := m.scrollOffset
	endLine := min(startLine+m.height, totalLines)

	if startLine >= endLine {
		return ""
	}

	// Copy only the visible window to avoid mutating cached lines
	// This is O(viewportHeight) instead of O(totalHeight)
	visibleLines := make([]string, endLine-startLine)
	for i := startLine; i < endLine; i++ {
		if i < len(m.renderedLines) {
			visibleLines[i-startLine] = m.renderedLines[i]
		}
		// Lines beyond renderedLines are bottom slack (empty strings), already zero-valued
	}

	if m.selection.active {
		visibleLines = m.applySelectionHighlight(visibleLines, startLine)
	}

	visibleLines = m.applyURLUnderline(visibleLines, startLine)

	// Sync scroll state and delegate rendering to scrollview which guarantees
	// fixed-width padding, pinned scrollbar, and exact height.
	m.scrollview.SetContent(m.renderedLines, m.totalScrollableHeight())
	m.scrollview.SetScrollOffset(m.scrollOffset)
	return m.scrollview.ViewWithLines(visibleLines)
}

// updateScrollState recomputes rendered content, bottom slack and scroll
// offset from the current state of the message list. Called both from View()
// and from Update() on animation ticks so that the slack subscription is
// registered before tui.go schedules the next tick.
func (m *model) updateScrollState() {
	prevTotalHeight := m.totalHeight
	prevScrollableHeight := m.totalHeight + m.bottomSlack
	m.ensureAllItemsRendered()

	if m.userHasScrolled {
		m.bottomSlack = 0
	} else {
		delta := m.totalHeight - prevTotalHeight
		switch {
		case delta < 0:
			// Cap so the viewport is never mostly empty after a large
			// shrinkage (e.g., several tool calls fading out at once).
			m.bottomSlack = min(m.bottomSlack-delta, m.maxBottomSlack())
		case delta > 0 && m.bottomSlack > 0:
			m.bottomSlack = max(0, m.bottomSlack-delta)
		}
	}

	scrollableHeight := m.totalHeight + m.bottomSlack
	maxScrollOffset := max(0, scrollableHeight-m.height)

	// Auto-scroll when content grows beyond any slack.
	if !m.userHasScrolled && scrollableHeight > prevScrollableHeight {
		m.scrollOffset = maxScrollOffset
	} else {
		m.scrollOffset = max(0, min(m.scrollOffset, maxScrollOffset))
	}
}

// maxBottomSlack returns the maximum blank lines added after content shrinks.
// Small enough that the viewport never feels empty, large enough to absorb a
// typical tool fade-out (~2 lines) without a visible jump.
func (m *model) maxBottomSlack() int {
	return max(1, min(5, m.height/3))
}

// handleAnimationTick refreshes scroll state, decays any leftover slack by
// one line, and keeps the slack subscription alive while slack > 0 so
// further ticks fire even after fade animations finish. Returns the command
// to schedule the next tick when the subscription transitions to active.
func (m *model) handleAnimationTick() tea.Cmd {
	m.updateScrollState()
	if !m.userHasScrolled && m.bottomSlack > 0 {
		m.bottomSlack--
	}
	if m.bottomSlack > 0 {
		return m.slackAnimationSub.Start()
	}
	m.slackAnimationSub.Stop()
	return nil
}

// SetSize sets the dimensions of the component
func (m *model) SetSize(width, height int) tea.Cmd {
	if m.width == width && m.height == height {
		return nil // Dimensions unchanged — skip expensive cache invalidation
	}
	m.width = width
	m.height = height

	m.scrollview.SetSize(width, height)
	contentWidth := m.contentWidth()
	for _, view := range m.views {
		view.SetSize(contentWidth, 0)
	}

	m.invalidateAllItems()
	return nil
}

func (m *model) SetPosition(x, y int) tea.Cmd {
	m.xPos = x
	m.yPos = y
	m.scrollview.SetPosition(x, y)
	return nil
}

// GetSize returns the current dimensions
func (m *model) GetSize() (width, height int) {
	return m.width, m.height
}

// Focus gives focus to the component.
func (m *model) Focus() tea.Cmd {
	m.focused = true
	// Start selection on the last assistant message for better UX
	m.selectedMessageIndex = m.findLastAssistantMessage()
	if m.selectedMessageIndex < 0 {
		// Fall back to last selectable if no assistant messages
		m.selectedMessageIndex = m.findLastSelectableMessage()
	}
	// Only invalidate the newly selected message
	if m.selectedMessageIndex >= 0 {
		m.invalidateItem(m.selectedMessageIndex)
	}
	m.renderDirty = true
	return nil
}

// Blur removes focus from the component
func (m *model) Blur() tea.Cmd {
	oldIndex := m.selectedMessageIndex
	m.focused = false
	m.selectedMessageIndex = -1
	// Only invalidate the previously selected message
	if oldIndex >= 0 {
		m.invalidateItem(oldIndex)
	}
	m.renderDirty = true
	return nil
}

// FocusAt gives focus and selects the message at the given screen coordinates.
func (m *model) FocusAt(x, y int) tea.Cmd {
	m.focused = true

	oldIndex := m.selectedMessageIndex

	line, _ := m.mouseToLineCol(x, y)
	if msgIdx, _ := m.globalLineToMessageLine(line); msgIdx >= 0 && m.isSelectableMessage(msgIdx) {
		m.selectedMessageIndex = msgIdx
	} else {
		m.selectedMessageIndex = m.findLastAssistantMessage()
		if m.selectedMessageIndex < 0 {
			m.selectedMessageIndex = m.findLastSelectableMessage()
		}
	}

	// Only invalidate the old and new selected messages
	if oldIndex >= 0 {
		m.invalidateItem(oldIndex)
	}
	if m.selectedMessageIndex >= 0 && m.selectedMessageIndex != oldIndex {
		m.invalidateItem(m.selectedMessageIndex)
	}
	m.renderDirty = true

	if m.messageTypeChanged(oldIndex, m.selectedMessageIndex) {
		return core.CmdHandler(messages.InvalidateStatusBarMsg{})
	}
	return nil
}

// Bindings returns key bindings for the component
func (m *model) Bindings() []key.Binding {
	// Return editing bindings when inline editing is active
	if m.inlineEditMsgIndex >= 0 {
		return m.InlineEditBindings()
	}

	bindings := []key.Binding{
		key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "select prev")),
		key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "select next")),
		key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy message")),
	}

	// Only show edit binding when a user message with session position is selected
	if m.selectedMessageIndex >= 0 && m.selectedMessageIndex < len(m.messages) {
		msg := m.messages[m.selectedMessageIndex]
		if msg.Type == types.MessageTypeUser && msg.SessionPosition != nil {
			bindings = append(bindings, key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit message")))
		}
	}

	return bindings
}

// InlineEditBindings returns key bindings for inline edit mode
func (m *model) InlineEditBindings() []key.Binding {
	// Get the newline key help based on the configured keymap
	newlineKeys := m.inlineEditTextarea.KeyMap.InsertNewline.Keys()
	newlineHelp := "Ctrl+j"
	if slices.Contains(newlineKeys, "shift+enter") {
		newlineHelp = "Shift+Enter"
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "save")),
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "cancel")),
		key.NewBinding(key.WithKeys(newlineKeys...), key.WithHelp(newlineHelp, "newline")),
	}
}

// Help returns the help information
func (m *model) Help() help.KeyMap {
	return core.NewSimpleHelp(m.Bindings())
}

// Scrolling methods
const (
	defaultScrollAmount = 1
	wheelScrollAmount   = 2
)

func (m *model) scrollUp() {
	if m.scrollOffset > 0 {
		m.userHasScrolled = true
		m.bottomSlack = 0
		m.setScrollOffset(max(0, m.scrollOffset-defaultScrollAmount))
	}
}

func (m *model) scrollDown() {
	m.setScrollOffset(m.scrollOffset + defaultScrollAmount)
	if m.isAtBottom() {
		m.userHasScrolled = false
	}
}

func (m *model) scrollPageUp() {
	m.userHasScrolled = true
	m.bottomSlack = 0
	m.setScrollOffset(max(0, m.scrollOffset-m.height))
}

func (m *model) scrollPageDown() {
	m.setScrollOffset(m.scrollOffset + m.height)
	if m.isAtBottom() {
		m.userHasScrolled = false
	}
}

func (m *model) scrollToTop() {
	m.userHasScrolled = true
	m.bottomSlack = 0
	m.setScrollOffset(0)
}

func (m *model) scrollToBottom() {
	m.userHasScrolled = false
	m.setScrollOffset(9_999_999) // Will be clamped in View()
}

func (m *model) scrollByWheel(delta int) {
	if delta == 0 {
		return
	}

	prevOffset := m.scrollOffset
	m.setScrollOffset(m.scrollOffset + (delta * wheelScrollAmount * defaultScrollAmount))
	if m.scrollOffset == prevOffset {
		return
	}

	if delta < 0 {
		m.userHasScrolled = true
		m.bottomSlack = 0
	} else if m.isAtBottom() {
		m.userHasScrolled = false
	}
}

func (m *model) setScrollOffset(offset int) {
	maxOffset := max(0, m.totalScrollableHeight()-m.height)
	m.scrollOffset = max(0, min(offset, maxOffset))
	m.scrollview.SetScrollOffset(m.scrollOffset)
}

func (m *model) isAtBottom() bool {
	if len(m.messages) == 0 {
		return true
	}
	maxScrollOffset := max(0, m.totalScrollableHeight()-m.height)
	return m.scrollOffset >= maxScrollOffset
}

// Message selection methods
func (m *model) isSelectableMessage(index int) bool {
	if index < 0 || index >= len(m.messages) {
		return false
	}
	msg := m.messages[index]
	switch msg.Type {
	case types.MessageTypeAssistant, types.MessageTypeAssistantReasoningBlock:
		return true
	case types.MessageTypeUser:
		// User messages are selectable only if they have a session position (editable)
		return msg.SessionPosition != nil
	default:
		return false
	}
}

func (m *model) findLastSelectableMessage() int {
	for i := range slices.Backward(m.messages) {
		if m.isSelectableMessage(i) {
			return i
		}
	}
	return -1
}

// findLastAssistantMessage finds the last assistant or reasoning block message.
// Used for initial focus selection to start on assistant content.
func (m *model) findLastAssistantMessage() int {
	for i := range slices.Backward(m.messages) {
		msg := m.messages[i]
		if msg.Type == types.MessageTypeAssistant || msg.Type == types.MessageTypeAssistantReasoningBlock {
			return i
		}
	}
	return -1
}

func (m *model) findPreviousSelectableMessage(fromIndex int) int {
	for i := fromIndex - 1; i >= 0; i-- {
		if m.isSelectableMessage(i) {
			return i
		}
	}
	return -1
}

func (m *model) findNextSelectableMessage(fromIndex int) int {
	for i := fromIndex + 1; i < len(m.messages); i++ {
		if m.isSelectableMessage(i) {
			return i
		}
	}
	return -1
}

func (m *model) selectPreviousMessage() tea.Cmd {
	if len(m.messages) == 0 {
		return nil
	}
	if prevIndex := m.findPreviousSelectableMessage(m.selectedMessageIndex); prevIndex >= 0 {
		oldIndex := m.selectedMessageIndex
		m.selectedMessageIndex = prevIndex
		if oldIndex >= 0 {
			m.invalidateItem(oldIndex)
		}
		m.invalidateItem(prevIndex)
		m.renderDirty = true
		m.scrollToSelectedMessage()
		if m.messageTypeChanged(oldIndex, prevIndex) {
			return core.CmdHandler(messages.InvalidateStatusBarMsg{})
		}
	}
	return nil
}

func (m *model) selectNextMessage() tea.Cmd {
	if len(m.messages) == 0 {
		return nil
	}
	if nextIndex := m.findNextSelectableMessage(m.selectedMessageIndex); nextIndex >= 0 {
		oldIndex := m.selectedMessageIndex
		m.selectedMessageIndex = nextIndex
		if oldIndex >= 0 {
			m.invalidateItem(oldIndex)
		}
		m.invalidateItem(nextIndex)
		m.renderDirty = true
		m.scrollToSelectedMessage()
		if m.messageTypeChanged(oldIndex, nextIndex) {
			return core.CmdHandler(messages.InvalidateStatusBarMsg{})
		}
	}
	return nil
}

func (m *model) messageTypeChanged(oldIndex, newIndex int) bool {
	if oldIndex < 0 || newIndex < 0 {
		return true
	}
	if oldIndex >= len(m.messages) || newIndex >= len(m.messages) {
		return true
	}
	return m.messages[oldIndex].Type != m.messages[newIndex].Type
}

func (m *model) scrollToSelectedMessage() {
	if m.selectedMessageIndex < 0 || m.selectedMessageIndex >= len(m.messages) {
		return
	}

	// Ensure all items are rendered so lineOffsets and totalHeight are accurate
	m.ensureAllItemsRendered()

	if m.selectedMessageIndex >= len(m.lineOffsets) {
		return
	}

	startLine := m.lineOffsets[m.selectedMessageIndex]

	var selectedHeight int
	if m.selectedMessageIndex < len(m.views) {
		item := m.renderItem(m.selectedMessageIndex, m.views[m.selectedMessageIndex])
		selectedHeight = item.height
	}
	endLine := startLine + selectedHeight

	// Scroll to show the top of the selected message.
	// When messages are taller than the viewport, always anchor to the start
	// so the user sees the beginning of the message first.
	if startLine < m.scrollOffset || endLine > m.scrollOffset+m.height {
		m.setScrollOffset(startLine)
	}
}

// Caching methods
func (m *model) shouldCacheMessage(index int) bool {
	if index < 0 || index >= len(m.messages) {
		return false
	}

	msg := m.messages[index]
	switch msg.Type {
	case types.MessageTypeToolCall:
		return msg.ToolStatus == types.ToolStatusCompleted ||
			msg.ToolStatus == types.ToolStatusError ||
			msg.ToolStatus == types.ToolStatusConfirmation
	case types.MessageTypeToolResult:
		return true
	case types.MessageTypeAssistant:
		return strings.Trim(msg.Content, "\r\n\t ") != ""
	case types.MessageTypeAssistantReasoningBlock:
		// Don't cache reasoning blocks - they can have spinners for in-progress tools
		return false
	case types.MessageTypeUser:
		return true
	default:
		return false
	}
}

func (m *model) renderItem(index int, view layout.Model) renderedItem {
	// If this message is being inline edited, render the textarea instead
	if index == m.inlineEditMsgIndex {
		rendered := m.renderInlineEditTextarea()
		var lines []string
		if rendered != "" {
			lines = strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
		}
		return renderedItem{view: rendered, lines: lines, height: len(lines)}
	}

	isSelected := m.focused && index == m.selectedMessageIndex
	isHovered := index == m.hoveredMessageIndex

	switch v := view.(type) {
	case message.Model:
		v.SetSelected(isSelected)
		v.SetHovered(isHovered)
	case *reasoningblock.Model:
		v.SetSelected(isSelected)
	}

	shouldCache := !isSelected && !isHovered && m.shouldCacheMessage(index)
	if shouldCache {
		if cached, exists := m.renderedItems[index]; exists {
			return cached
		}
	}

	rendered := view.View()
	var lines []string
	if rendered != "" {
		lines = strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	}

	item := renderedItem{view: rendered, lines: lines, height: len(lines)}

	if shouldCache {
		m.renderedItems[index] = item
	}

	return item
}

// renderInlineEditTextarea renders the inline editing textarea with user message styling.
func (m *model) renderInlineEditTextarea() string {
	// Use the same style as user messages but with a highlight to indicate editing
	editStyle := styles.UserMessageStyle.
		BorderForeground(styles.Accent)

	innerWidth := m.contentWidth() - editStyle.GetHorizontalFrameSize()
	if innerWidth > 0 {
		m.inlineEditTextarea.SetWidth(innerWidth)
	}

	// The textarea is set to a large height to prevent internal viewport scrolling
	// which causes cursor positioning bugs in multi-line content. We trim the
	// end-of-buffer padding lines from the rendered output.
	view := m.inlineEditTextarea.View()
	view = trimEndOfBufferLines(view)

	// Add a minimal edit indicator at the bottom left with extra padding
	editHint := styles.MutedStyle.Render("[editing]")

	content := view + "\n\n" + editHint
	return editStyle.Width(m.contentWidth()).Render(content)
}

// trimEndOfBufferLines removes trailing end-of-buffer padding lines from a
// textarea's rendered View output. The textarea pads its view to fill its
// configured height; these padding lines contain only whitespace (after
// stripping ANSI sequences) and appear after the actual content.
func trimEndOfBufferLines(view string) string {
	lines := strings.Split(view, "\n")

	// Trim trailing lines that are visually empty (whitespace-only after ANSI strip).
	// Content lines always contain visible text or cursor escape sequences.
	// Always keep at least one line so that an empty textarea still renders
	// the cursor line instead of returning the full padded view.
	last := len(lines)
	for last > 1 && strings.TrimSpace(ansi.Strip(lines[last-1])) == "" {
		last--
	}

	return strings.Join(lines[:last], "\n")
}

func (m *model) needsSeparator(index int) bool {
	if index >= len(m.messages)-1 {
		return false
	}
	currentIsToolCall := m.messages[index].Type == types.MessageTypeToolCall
	nextIsToolCall := m.messages[index+1].Type == types.MessageTypeToolCall

	// Always add a separator before transfer_task, even between consecutive tool calls
	if nextIsToolCall && m.messages[index+1].ToolCall.Function.Name == transfertask.ToolNameTransferTask {
		return true
	}

	return !currentIsToolCall || !nextIsToolCall
}

func (m *model) ensureAllItemsRendered() {
	if !m.renderDirty && len(m.renderedLines) > 0 {
		return
	}

	if len(m.views) == 0 {
		m.renderedLines = nil
		m.totalHeight = 0
		m.renderDirty = false
		return
	}

	var allLines []string
	offsets := make([]int, len(m.views))

	for i, view := range m.views {
		offsets[i] = len(allLines)
		item := m.renderItem(i, view)
		if len(item.lines) == 0 {
			continue
		}

		allLines = append(allLines, item.lines...)

		if m.needsSeparator(i) {
			allLines = append(allLines, "")
		}
	}

	// Store lines directly - avoid join/split on every View() call
	m.renderedLines = allLines
	m.lineOffsets = offsets
	m.totalHeight = len(allLines)
	m.urlSpans.clear()
	m.renderDirty = false
}

func (m *model) invalidateItem(index int) {
	if m.shouldCacheMessage(index) {
		delete(m.renderedItems, index)
	}
	m.renderDirty = true
}

func (m *model) invalidateAllItems() {
	m.renderedItems = make(map[int]renderedItem)
	m.renderedLines = nil
	m.lineOffsets = nil
	m.totalHeight = 0
	m.urlSpans.clear()
	m.renderDirty = true
}

// Message management methods
func (m *model) AddUserMessage(content string) tea.Cmd {
	return m.addMessage(types.User(content))
}

func (m *model) AddLoadingMessage(description string) tea.Cmd {
	return m.addMessage(types.Loading(description))
}

func (m *model) ReplaceLoadingWithUser(content string, sessionPos int) tea.Cmd {
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeLoading {
			m.messages = slices.Delete(m.messages, i, i+1)
			if i < len(m.views) {
				m.views = slices.Delete(m.views, i, i+1)
			}
			m.invalidateAllItems()
			break
		}
	}
	msg := types.User(content)
	if sessionPos >= 0 {
		pos := sessionPos
		msg.SessionPosition = &pos
	}
	return m.addMessage(msg)
}

func (m *model) AddErrorMessage(content string) tea.Cmd {
	m.removeSpinner()
	return m.addMessage(types.Error(content))
}

func (m *model) AddShellOutputMessage(content string) tea.Cmd {
	return m.addMessage(types.ShellOutput(content))
}

func (m *model) AddAssistantMessage() tea.Cmd {
	return m.addMessage(types.Spinner())
}

func (m *model) AddCancelledMessage() tea.Cmd {
	msg := types.Cancelled()
	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	return view.Init()
}

func (m *model) AddWelcomeMessage(content string) tea.Cmd {
	if content == "" || len(m.views) > 0 {
		return nil
	}
	msg := types.Welcome(content)
	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	return view.Init()
}

func (m *model) addMessage(msg *types.Message) tea.Cmd {
	m.clearSelection()
	shouldAutoScroll := !m.userHasScrolled

	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.sessionState.SetPreviousMessage(msg)
	m.views = append(m.views, view)
	m.renderDirty = true

	var cmds []tea.Cmd
	if initCmd := view.Init(); initCmd != nil {
		cmds = append(cmds, initCmd)
	}
	if shouldAutoScroll {
		cmds = append(cmds, func() tea.Msg {
			m.scrollToBottom()
			return nil
		})
	}

	return tea.Batch(cmds...)
}

func (m *model) LoadFromSession(sess *session.Session) tea.Cmd {
	appendSessionMessage := func(msg *types.Message, view layout.Model) {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, view)
		m.sessionState.SetPreviousMessage(msg)
	}

	// getOrCreateReasoningBlock returns an existing reasoning block for the agent if the
	// last message is one, otherwise creates a new one. This combines consecutive
	// reasoning/tool messages from the same agent into a single block.
	getOrCreateReasoningBlock := func(agentName string) *reasoningblock.Model {
		if len(m.messages) > 0 {
			lastIdx := len(m.messages) - 1
			lastMsg := m.messages[lastIdx]
			if lastMsg.Type == types.MessageTypeAssistantReasoningBlock && lastMsg.Sender == agentName {
				if block, ok := m.views[lastIdx].(*reasoningblock.Model); ok {
					return block
				}
			}
		}

		// Create new reasoning block
		block := reasoningblock.New(nextBlockID(), agentName, m.sessionState)
		block.SetSize(m.contentWidth(), 0)

		blockMsg := &types.Message{
			Type:   types.MessageTypeAssistantReasoningBlock,
			Sender: agentName,
		}
		appendSessionMessage(blockMsg, block)
		return block
	}

	// addStandaloneToolCall adds a tool call as a standalone message (not in a reasoning block)
	addStandaloneToolCall := func(agentName string, tc tools.ToolCall, toolDef tools.Tool, toolResults map[string]string) {
		toolMsg := types.ToolCallMessage(agentName, tc, toolDef, types.ToolStatusCompleted)
		// Apply tool result if available
		if result, ok := toolResults[tc.ID]; ok {
			toolMsg.Content = strings.ReplaceAll(result, "\t", "    ")
		}
		view := m.createToolCallView(toolMsg)
		appendSessionMessage(toolMsg, view)
	}

	m.messages = nil
	m.views = nil
	m.renderedItems = make(map[int]renderedItem)
	m.renderedLines = nil
	m.scrollOffset = 0
	m.totalHeight = 0
	m.bottomSlack = 0
	m.selectedMessageIndex = -1
	m.hoveredMessageIndex = -1
	m.hoveredURL = nil

	var cmds []tea.Cmd

	// First pass: collect tool results by ToolCallID
	toolResults := make(map[string]string)
	for _, item := range sess.Messages {
		if !item.IsMessage() {
			continue
		}
		smsg := item.Message
		if smsg.Message.Role == chat.MessageRoleTool && smsg.Message.ToolCallID != "" {
			toolResults[smsg.Message.ToolCallID] = smsg.Message.Content
		}
	}

	for pos, item := range sess.Messages {
		if !item.IsMessage() {
			continue
		}

		smsg := item.Message
		if smsg.Implicit {
			continue
		}

		switch smsg.Message.Role {
		case chat.MessageRoleUser:
			msg := types.User(smsg.Message.Content)
			msgPos := pos
			msg.SessionPosition = &msgPos
			appendSessionMessage(msg, m.createMessageView(msg))
		case chat.MessageRoleAssistant:
			hasReasoning := smsg.Message.ReasoningContent != ""
			hasContent := smsg.Message.Content != ""
			hasToolCalls := len(smsg.Message.ToolCalls) > 0
			var reasoningBlock *reasoningblock.Model

			// Step 1: Handle reasoning content - only create/extend a reasoning block if there's actual reasoning
			if hasReasoning {
				reasoningBlock = getOrCreateReasoningBlock(smsg.AgentName)
				reasoningBlock.AppendReasoning(smsg.Message.ReasoningContent)
				// Update the message content for copying
				lastIdx := len(m.messages) - 1
				if m.messages[lastIdx].Content != "" {
					m.messages[lastIdx].Content += "\n\n"
				}
				m.messages[lastIdx].Content += smsg.Message.ReasoningContent
			}

			// Step 2: Handle assistant content - this breaks the reasoning block chain
			if hasContent {
				msg := types.Agent(types.MessageTypeAssistant, smsg.AgentName, smsg.Message.Content)
				appendSessionMessage(msg, m.createMessageView(msg))
			}

			// Step 3: Handle tool calls
			// Tool calls go into the reasoning block ONLY if there was reasoning content AND no regular content
			if hasToolCalls {
				attachToReasoning := reasoningBlock != nil && !hasContent
				for i, tc := range smsg.Message.ToolCalls {
					var toolDef tools.Tool
					if i < len(smsg.Message.ToolDefinitions) {
						toolDef = smsg.Message.ToolDefinitions[i]
					}

					if attachToReasoning {
						toolMsg := types.ToolCallMessage(smsg.AgentName, tc, toolDef, types.ToolStatusCompleted)
						reasoningBlock.AddToolCall(toolMsg)
						if result, ok := toolResults[tc.ID]; ok {
							reasoningBlock.UpdateToolResult(tc.ID, result, types.ToolStatusCompleted, nil)
						}
						continue
					}

					addStandaloneToolCall(smsg.AgentName, tc, toolDef, toolResults)
				}
			}
		case chat.MessageRoleTool:
			continue
		}
	}

	for _, view := range m.views {
		cmds = append(cmds, view.Init())
	}

	cmds = append(cmds, m.ScrollToBottom())
	return tea.Batch(cmds...)
}

func (m *model) AddOrUpdateToolCall(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status types.ToolStatus) tea.Cmd {
	// First check if this tool call exists in any reasoning block
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeAssistantReasoningBlock {
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				if block.HasToolCall(toolCall.ID) {
					block.UpdateToolCall(toolCall.ID, status, toolCall.Function.Arguments)
					m.invalidateItem(i)
					return nil
				}
			}
		}
	}

	// Then try to update existing standalone tool by ID
	for i := range slices.Backward(m.messages) {
		msg := m.messages[i]
		if msg.Type == types.MessageTypeToolCall && msg.ToolCall.ID == toolCall.ID {
			msg.ToolStatus = status
			if status == types.ToolStatusRunning && msg.StartedAt == nil {
				now := time.Now()
				msg.StartedAt = &now
			}
			if toolCall.Function.Arguments != "" {
				if status == types.ToolStatusPending {
					msg.ToolCall.Function.Arguments += toolCall.Function.Arguments
				} else {
					msg.ToolCall.Function.Arguments = toolCall.Function.Arguments
				}
			}
			m.invalidateItem(i)
			return nil
		}
	}

	m.removeSpinner()

	// If there's an active reasoning block, add the tool call to it
	if block, blockIdx := m.getActiveReasoningBlock(agentName); block != nil {
		msg := types.ToolCallMessage(agentName, toolCall, toolDef, status)
		cmd := block.AddToolCall(msg)
		m.invalidateItem(blockIdx)
		return cmd
	}

	// Otherwise create a standalone tool call message
	msg := types.ToolCallMessage(agentName, toolCall, toolDef, status)
	m.messages = append(m.messages, msg)
	view := m.createToolCallView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true

	return view.Init()
}

func (m *model) AddToolResult(msg *runtime.ToolCallResponseEvent, status types.ToolStatus) tea.Cmd {
	// First check reasoning blocks for the tool call
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeAssistantReasoningBlock {
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				if block.HasToolCall(msg.ToolCallID) {
					cmd := block.UpdateToolResult(msg.ToolCallID, msg.Response, status, msg.Result)
					m.invalidateItem(i)
					return cmd
				}
			}
		}
	}

	// Then check standalone tool call messages
	for i := range slices.Backward(m.messages) {
		toolMessage := m.messages[i]
		if toolMessage.Type == types.MessageTypeToolCall && toolMessage.ToolCall.ID == msg.ToolCallID {
			toolMessage.Content = strings.ReplaceAll(msg.Response, "\t", "    ")
			toolMessage.ToolStatus = status
			toolMessage.ToolResult = msg.Result
			m.invalidateItem(i)

			view := m.createToolCallView(toolMessage)
			m.views[i] = view
			return view.Init()
		}
	}
	return nil
}

func (m *model) AppendToLastMessage(agentName, content string) tea.Cmd {
	m.removeSpinner()

	if len(m.messages) == 0 {
		return nil
	}

	lastIdx := len(m.messages) - 1
	lastMsg := m.messages[lastIdx]

	// Append to existing assistant message from same agent
	if lastMsg.Type == types.MessageTypeAssistant && lastMsg.Sender == agentName {
		lastMsg.Content += content
		m.views[lastIdx].(message.Model).SetMessage(lastMsg)
		m.invalidateItem(lastIdx)
		return nil
	}

	return m.addMessage(types.Agent(types.MessageTypeAssistant, agentName, content))
}

func (m *model) AppendReasoning(agentName, content string) tea.Cmd {
	m.removeSpinner()

	if len(m.messages) == 0 {
		return m.addReasoningBlock(agentName, content)
	}

	lastIdx := len(m.messages) - 1
	lastMsg := m.messages[lastIdx]

	// Append to existing reasoning block for this agent
	if lastMsg.Type == types.MessageTypeAssistantReasoningBlock && lastMsg.Sender == agentName {
		if block, ok := m.views[lastIdx].(*reasoningblock.Model); ok {
			block.AppendReasoning(content)
			lastMsg.Content += content // Keep content in sync for copying
			m.invalidateItem(lastIdx)
			return nil
		}
	}

	// Create a new reasoning block
	return m.addReasoningBlock(agentName, content)
}

// addReasoningBlock creates a new reasoning block message.
func (m *model) addReasoningBlock(agentName, content string) tea.Cmd {
	m.clearSelection()
	shouldAutoScroll := !m.userHasScrolled

	msg := &types.Message{
		Type:    types.MessageTypeAssistantReasoningBlock,
		Sender:  agentName,
		Content: content,
	}

	block := reasoningblock.New(nextBlockID(), agentName, m.sessionState)
	block.SetReasoning(content)
	block.SetSize(m.contentWidth(), 0)

	m.messages = append(m.messages, msg)
	m.views = append(m.views, block)
	m.sessionState.SetPreviousMessage(msg)
	m.renderDirty = true

	var cmds []tea.Cmd
	if initCmd := block.Init(); initCmd != nil {
		cmds = append(cmds, initCmd)
	}
	if shouldAutoScroll {
		cmds = append(cmds, func() tea.Msg {
			m.scrollToBottom()
			return nil
		})
	}

	return tea.Batch(cmds...)
}

// getActiveReasoningBlock returns the active reasoning block for the given agent,
// or nil if the last message is not a reasoning block for that agent.
func (m *model) getActiveReasoningBlock(agentName string) (*reasoningblock.Model, int) {
	if len(m.messages) == 0 {
		return nil, -1
	}

	lastIdx := len(m.messages) - 1
	lastMsg := m.messages[lastIdx]

	if lastMsg.Type == types.MessageTypeAssistantReasoningBlock && lastMsg.Sender == agentName {
		if block, ok := m.views[lastIdx].(*reasoningblock.Model); ok {
			return block, lastIdx
		}
	}

	return nil, -1
}

func (m *model) ScrollToBottom() tea.Cmd {
	return func() tea.Msg {
		if !m.userHasScrolled {
			m.scrollToBottom()
		}
		return nil
	}
}

func (m *model) AdjustBottomSlack(delta int) {
	if delta == 0 {
		return
	}
	m.bottomSlack = max(0, min(m.bottomSlack+delta, m.maxBottomSlack()))
}

// contentWidth returns the width available for content.
// Always reserves space for scrollbar (gap + bar) to prevent layout shifts.
func (m *model) contentWidth() int {
	return m.scrollview.ContentWidth()
}

func (m *model) totalScrollableHeight() int {
	return m.totalHeight + m.bottomSlack
}

// Helper methods
func (m *model) createToolCallView(msg *types.Message) layout.Model {
	view := tool.New(msg, m.sessionState)
	view.SetSize(m.contentWidth(), 0)
	return view
}

func (m *model) createMessageView(msg *types.Message) layout.Model {
	view := message.New(msg, m.sessionState.PreviousMessage())
	view.SetSize(m.contentWidth(), 0)
	return view
}

func (m *model) RemoveSpinner() {
	m.removeSpinner()
}

// animationStopper is implemented by views that register with the animation coordinator.
// When a view is removed from the UI, StopAnimation must be called to unregister
// its animation subscription and prevent leaked ticks.
type animationStopper interface {
	StopAnimation()
}

// stopViewAnimation stops animation subscriptions for a view being removed.
// This prevents animation tick leaks when views with active spinners are
// removed from the message list (e.g., on stream cancellation via ESC).
func stopViewAnimation(view layout.Model) {
	if stopper, ok := view.(animationStopper); ok {
		stopper.StopAnimation()
	}
}

func (m *model) removeSpinner() {
	if len(m.messages) == 0 {
		return
	}

	lastIdx := len(m.messages) - 1
	if m.messages[lastIdx].Type == types.MessageTypeSpinner {
		// Stop any animation subscriptions before removing the view
		if lastIdx < len(m.views) {
			stopViewAnimation(m.views[lastIdx])
			m.views = m.views[:lastIdx]
		}
		m.messages = m.messages[:lastIdx]
		m.invalidateAllItems()
	}
}

func (m *model) removePendingToolCallMessages() {
	toolCallMessages := make([]*types.Message, 0, len(m.messages))
	views := make([]layout.Model, 0, len(m.views))

	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeToolCall &&
			(msg.ToolStatus == types.ToolStatusPending || msg.ToolStatus == types.ToolStatusRunning) {
			// Stop any animation subscriptions before removing the view
			if i < len(m.views) {
				stopViewAnimation(m.views[i])
			}
			continue
		}

		toolCallMessages = append(toolCallMessages, msg)
		if i < len(m.views) {
			views = append(views, m.views[i])
		}
	}

	if len(toolCallMessages) != len(m.messages) {
		m.messages = toolCallMessages
		m.views = views
		m.invalidateAllItems()
	}
}

// stopReasoningBlockAnimations stops spinner animations in reasoning blocks
// that have in-progress tool calls. Called on stream cancellation to prevent
// spinners from running indefinitely after ESC is pressed.
func (m *model) stopReasoningBlockAnimations() {
	for i, msg := range m.messages {
		if msg.Type != types.MessageTypeAssistantReasoningBlock || i >= len(m.views) {
			continue
		}
		block, ok := m.views[i].(*reasoningblock.Model)
		if !ok {
			continue
		}
		block.StopAnimation()
		m.invalidateItem(i)
	}
}

func (m *model) isEditLabelClick(msgIdx, localLine, col int) (bool, *types.Message) {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return false, nil
	}
	msg := m.messages[msgIdx]
	if msg.Type != types.MessageTypeUser || msg.SessionPosition == nil {
		return false, nil
	}
	if msgIdx >= len(m.views) {
		return false, nil
	}

	item := m.renderItem(msgIdx, m.views[msgIdx])
	if localLine < 0 || localLine >= len(item.lines) {
		return false, nil
	}

	plainLine := ansi.Strip(item.lines[localLine])
	before, _, ok := strings.Cut(plainLine, types.UserMessageEditLabel)
	if !ok {
		return false, nil
	}

	labelStart := ansi.StringWidth(before)
	labelEnd := labelStart + ansi.StringWidth(types.UserMessageEditLabel)
	if col >= labelStart && col < labelEnd {
		return true, msg
	}

	return false, nil
}

// codeBlockAt returns the raw code of the fenced code block whose copy label
// is at the given click position, if any.
func (m *model) codeBlockAt(msgIdx, localLine, col int) (string, bool) {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return "", false
	}
	if msgIdx >= len(m.views) {
		return "", false
	}
	mv, ok := m.views[msgIdx].(message.Model)
	if !ok {
		return "", false
	}
	blocks := mv.CodeBlocks()
	if len(blocks) == 0 {
		return "", false
	}
	var target *markdown.CodeBlock
	for i := range blocks {
		if blocks[i].Line == localLine {
			target = &blocks[i]
			break
		}
	}
	if target == nil {
		return "", false
	}

	item := m.renderItem(msgIdx, m.views[msgIdx])
	if localLine < 0 || localLine >= len(item.lines) {
		return "", false
	}
	plainLine := ansi.Strip(item.lines[localLine])
	before, _, found := strings.Cut(plainLine, markdown.CodeBlockCopyIcon)
	if !found {
		return "", false
	}
	iconStart := ansi.StringWidth(before)
	iconEnd := iconStart + ansi.StringWidth(markdown.CodeBlockCopyIcon)
	if col < iconStart || col >= iconEnd {
		return "", false
	}
	return target.Content, true
}

// isCopyLabelClick checks if the click is on the copy label of an assistant message.
func (m *model) isCopyLabelClick(msgIdx, localLine, col int) bool {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return false
	}
	msg := m.messages[msgIdx]
	if msg.Type != types.MessageTypeAssistant {
		return false
	}
	// Only clickable when hovered or selected
	if msgIdx != m.hoveredMessageIndex && (!m.focused || msgIdx != m.selectedMessageIndex) {
		return false
	}
	if msgIdx >= len(m.views) {
		return false
	}

	item := m.renderItem(msgIdx, m.views[msgIdx])
	if localLine < 0 || localLine >= len(item.lines) {
		return false
	}

	plainLine := ansi.Strip(item.lines[localLine])
	before, _, ok := strings.Cut(plainLine, types.AssistantMessageCopyLabel)
	if !ok {
		return false
	}

	labelStart := ansi.StringWidth(before)
	labelEnd := labelStart + ansi.StringWidth(types.AssistantMessageCopyLabel)
	return col >= labelStart && col < labelEnd
}

// copyMessageToClipboard copies the content of a specific message to clipboard.
func (m *model) copyMessageToClipboard(msgIdx int) tea.Cmd {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return nil
	}
	content := m.messages[msgIdx].Content
	if content == "" {
		return nil
	}
	return copyTextToClipboard(content)
}

func (m *model) mouseToLineCol(x, y int) (line, col int) {
	adjustedX := max(0, x-m.xPos)
	adjustedY := max(0, y-m.yPos)
	return m.scrollOffset + adjustedY, adjustedX
}

func (m *model) isMouseOnScrollbar(x, y int) bool {
	if m.totalHeight <= m.height {
		return false
	}
	return x == m.scrollview.ScrollbarX() && y >= m.yPos && y < m.yPos+m.height
}

func (m *model) IsScrollbarDragging() bool {
	return m.scrollview.IsDragging()
}

func (m *model) IsMouseOnScrollbar(x, y int) bool {
	return m.isMouseOnScrollbar(x, y)
}

func (m *model) handleScrollviewUpdate(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := m.scrollview.UpdateMouse(msg)
	m.scrollOffset = m.scrollview.ScrollOffset()
	if m.isAtBottom() {
		m.userHasScrolled = false
	} else {
		m.userHasScrolled = true
		m.bottomSlack = 0
	}
	return m, cmd
}

// hasAnimatedContent returns true if the message list contains content that
// requires tick-driven updates (spinners, fades, etc.). Used to decide whether
// to invalidate the render cache on animation ticks.
func (m *model) hasAnimatedContent() bool {
	for i, msg := range m.messages {
		switch msg.Type {
		case types.MessageTypeSpinner, types.MessageTypeLoading:
			// Spinner/loading messages always need ticks
			return true
		case types.MessageTypeToolCall:
			// Tool calls with pending/running status have spinners
			if msg.ToolStatus == types.ToolStatusPending ||
				msg.ToolStatus == types.ToolStatusRunning {
				return true
			}
		case types.MessageTypeAssistantReasoningBlock:
			// Check if reasoning block needs tick updates
			if i < len(m.views) {
				if block, ok := m.views[i].(*reasoningblock.Model); ok {
					if block.NeedsTick() {
						return true
					}
				}
			}
		}
	}
	return false
}

// StartInlineEdit begins inline editing for the specified message.
func (m *model) StartInlineEdit(msgIndex, sessionPosition int, content string) tea.Cmd {
	if msgIndex < 0 || msgIndex >= len(m.messages) {
		return nil
	}

	msg := m.messages[msgIndex]
	if msg.Type != types.MessageTypeUser {
		return nil
	}

	// Save the current selection state before entering inline edit
	// This allows restoring when the edit is cancelled
	m.inlineEditPrevSelection = m.selectedMessageIndex

	// Set focused state but clear any message selection to prevent highlight
	m.focused = true
	m.selectedMessageIndex = -1

	m.inlineEditMsgIndex = msgIndex
	m.inlineEditSessionPos = sessionPosition
	m.inlineEditOriginal = content

	// Create and configure the textarea
	ta := textarea.New()
	ta.SetValue(content)
	ta.Focus()

	// Configure appearance - use a style similar to user message
	innerWidth := m.contentWidth() - styles.UserMessageStyle.GetHorizontalFrameSize()
	if innerWidth > 0 {
		ta.SetWidth(innerWidth)
	}

	// Set a generous height so the textarea's internal viewport never scrolls.
	// This prevents cursor positioning bugs with multi-line content. The actual
	// rendered output is trimmed in renderInlineEditTextarea to remove padding.
	ta.SetHeight(max(1, m.height))

	// Remove the default prompt/placeholder styling for a cleaner look
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // No limit

	// Set custom styles with background color matching the user message style
	inlineEditStyle := textarea.Styles{
		Focused: textarea.StyleState{
			Base:        styles.BaseStyle.Background(styles.BackgroundAlt),
			Placeholder: styles.BaseStyle.Background(styles.BackgroundAlt).Foreground(styles.PlaceholderColor),
		},
		Blurred: textarea.StyleState{
			Base:        styles.BaseStyle.Background(styles.BackgroundAlt),
			Placeholder: styles.BaseStyle.Background(styles.BackgroundAlt).Foreground(styles.PlaceholderColor),
		},
		Cursor: textarea.CursorStyle{
			Color: styles.Accent,
		},
	}
	ta.SetStyles(inlineEditStyle)

	// Configure newline keybinding - use ctrl+j as the safe default
	// (shift+enter only works on terminals with keyboard enhancements)
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j")
	ta.KeyMap.InsertNewline.SetEnabled(true)

	m.inlineEditTextarea = ta
	m.invalidateItem(msgIndex)
	m.renderDirty = true

	// Invalidate statusbar cache since bindings have changed
	return tea.Batch(ta.Focus(), core.CmdHandler(messages.InvalidateStatusBarMsg{}))
}

// CancelInlineEdit cancels the current inline edit and restores the original content.
func (m *model) CancelInlineEdit() tea.Cmd {
	if m.inlineEditMsgIndex < 0 {
		return nil
	}

	msgIndex := m.inlineEditMsgIndex
	prevSelection := m.inlineEditPrevSelection

	m.inlineEditMsgIndex = -1
	m.inlineEditSessionPos = -1
	m.inlineEditOriginal = ""
	m.inlineEditTextarea = textarea.Model{}
	m.inlineEditPrevSelection = -1

	// Restore the previous selection state if we were in keyboard selection mode
	if prevSelection >= 0 {
		m.selectedMessageIndex = prevSelection
		m.focused = true
	} else {
		// We weren't in selection mode, blur the messages component
		m.focused = false
		m.selectedMessageIndex = -1
	}

	m.invalidateItem(msgIndex)
	m.invalidateAllItems() // Invalidate all to update selection highlight
	m.renderDirty = true

	// Invalidate statusbar cache since bindings have changed
	return tea.Batch(
		core.CmdHandler(InlineEditCancelledMsg{WasInSelectionMode: prevSelection >= 0}),
		core.CmdHandler(messages.InvalidateStatusBarMsg{}),
	)
}

// IsInlineEditing returns true if inline editing is currently active.
func (m *model) IsInlineEditing() bool {
	return m.inlineEditMsgIndex >= 0
}

// InlineEditCancelledMsg is sent when inline editing is cancelled.
type InlineEditCancelledMsg struct {
	WasInSelectionMode bool // True if we were in keyboard selection mode before editing
}

// commitInlineEdit commits the inline edit and sends the message.
func (m *model) commitInlineEdit() tea.Cmd {
	if m.inlineEditMsgIndex < 0 {
		return nil
	}

	content := strings.TrimSpace(m.inlineEditTextarea.Value())
	sessionPos := m.inlineEditSessionPos

	// Reset editing state
	m.inlineEditMsgIndex = -1
	m.inlineEditSessionPos = -1
	m.inlineEditOriginal = ""
	m.inlineEditTextarea = textarea.Model{}

	m.invalidateAllItems()

	// Invalidate statusbar cache since bindings have changed
	invalidateCmd := core.CmdHandler(messages.InvalidateStatusBarMsg{})

	if content == "" {
		// Empty content is treated as cancellation - notify the chat page
		return tea.Batch(
			core.CmdHandler(InlineEditCancelledMsg{}),
			invalidateCmd,
		)
	}

	// Emit InlineEditCommittedMsg with the edited content - the chat page handles branching
	return tea.Batch(
		core.CmdHandler(InlineEditCommittedMsg{
			SessionPosition: sessionPos,
			Content:         content,
		}),
		invalidateCmd,
	)
}

// InlineEditCommittedMsg is sent when inline editing is committed.
type InlineEditCommittedMsg struct {
	SessionPosition int
	Content         string
}
