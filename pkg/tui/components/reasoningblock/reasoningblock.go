package reasoningblock

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// toolEntry holds a tool call message and its view.
type toolEntry struct {
	msg  *types.Message
	view layout.Model
}

// contentItemKind identifies the type of content item.
type contentItemKind int

const (
	contentItemReasoning contentItemKind = iota
	contentItemTool
)

// contentItem represents either reasoning text or a tool call in sequence.
type contentItem struct {
	kind      contentItemKind
	reasoning string // Used when kind is contentItemReasoning
	toolIndex int    // Index into toolEntries when kind is contentItemTool
}

// Model represents a reasoning + tool calls block.
type Model struct {
	id                  string
	agentName           string
	contentItems        []contentItem // Ordered sequence of reasoning and tool calls
	toolEntries         []toolEntry   // All tool entries (referenced by contentItems)
	expanded            bool
	selected            bool
	width               int
	height              int
	sessionState        *service.SessionState
	animationRegistered bool // whether we're registered with animation coordinator
}

// New creates a new reasoning block.
func New(id, agentName string, sessionState *service.SessionState) *Model {
	return &Model{
		id:           id,
		agentName:    agentName,
		expanded:     true,
		width:        80,
		sessionState: sessionState,
	}
}

// ID returns the block's unique identifier.
func (m *Model) ID() string {
	return m.id
}

// AgentName returns the agent name for this block.
func (m *Model) AgentName() string {
	return m.agentName
}

// SetReasoning sets reasoning content (replaces all content items with a single reasoning item).
func (m *Model) SetReasoning(content string) {
	m.contentItems = []contentItem{{kind: contentItemReasoning, reasoning: content}}
}

// AppendReasoning appends to the reasoning content.
// Creates a new reasoning item if the last item was a tool, otherwise appends to the last reasoning item.
func (m *Model) AppendReasoning(content string) {
	if content == "" {
		return
	}

	// If no items yet or last item was a tool, create new reasoning item
	if len(m.contentItems) == 0 {
		m.contentItems = append(m.contentItems, contentItem{kind: contentItemReasoning, reasoning: content})
		return
	}

	lastIdx := len(m.contentItems) - 1
	if m.contentItems[lastIdx].kind == contentItemReasoning {
		// Append to existing reasoning
		m.contentItems[lastIdx].reasoning += content
	} else {
		// Last item was a tool, start new reasoning block
		m.contentItems = append(m.contentItems, contentItem{kind: contentItemReasoning, reasoning: content})
	}
}

// Reasoning returns the full reasoning content (concatenated from all reasoning items).
func (m *Model) Reasoning() string {
	var parts []string
	for _, item := range m.contentItems {
		if item.kind == contentItemReasoning && item.reasoning != "" {
			parts = append(parts, item.reasoning)
		}
	}
	return strings.Join(parts, "\n\n")
}

// AddToolCall adds a tool call to the block.
func (m *Model) AddToolCall(msg *types.Message) tea.Cmd {
	// Check if tool already exists (update case)
	for i, entry := range m.toolEntries {
		if entry.msg.ToolCall.ID == msg.ToolCall.ID {
			m.toolEntries[i].msg = msg
			m.toolEntries[i].view = tool.New(msg, m.sessionState)
			m.toolEntries[i].view.SetSize(m.contentWidth(), 0)
			return m.toolEntries[i].view.Init()
		}
	}

	// New tool call - add to entries and track position in content sequence
	view := tool.New(msg, m.sessionState)
	view.SetSize(m.contentWidth(), 0)
	toolIndex := len(m.toolEntries)
	m.toolEntries = append(m.toolEntries, toolEntry{msg: msg, view: view})
	m.contentItems = append(m.contentItems, contentItem{kind: contentItemTool, toolIndex: toolIndex})
	return view.Init()
}

// UpdateToolCall updates an existing tool call in the block.
func (m *Model) UpdateToolCall(toolCallID string, status types.ToolStatus, args string) {
	for i, entry := range m.toolEntries {
		if entry.msg.ToolCall.ID != toolCallID {
			continue
		}
		entry.msg.ToolStatus = status
		if status == types.ToolStatusRunning && entry.msg.StartedAt == nil {
			now := time.Now()
			entry.msg.StartedAt = &now
		}
		if args != "" {
			if status == types.ToolStatusPending {
				entry.msg.ToolCall.Function.Arguments += args
			} else {
				entry.msg.ToolCall.Function.Arguments = args
			}
		}
		m.toolEntries[i] = entry
		return
	}
}

// UpdateToolResult updates tool result for a tool call.
func (m *Model) UpdateToolResult(toolCallID, content string, status types.ToolStatus, result *tools.ToolCallResult) tea.Cmd {
	for i, entry := range m.toolEntries {
		if entry.msg.ToolCall.ID != toolCallID {
			continue
		}

		entry.msg.Content = strings.ReplaceAll(content, "\t", "    ")
		entry.msg.ToolStatus = status
		entry.msg.ToolResult = result

		view := tool.New(entry.msg, m.sessionState)
		view.SetSize(m.contentWidth(), 0)
		m.toolEntries[i] = entry
		m.toolEntries[i].view = view

		return view.Init()
	}
	return nil
}

// HasToolCall returns true if the block contains the given tool call ID.
func (m *Model) HasToolCall(toolCallID string) bool {
	for _, entry := range m.toolEntries {
		if entry.msg.ToolCall.ID == toolCallID {
			return true
		}
	}
	return false
}

// NeedsTick returns true if this reasoning block requires animation tick updates.
// This is true when any tool is pending/running and needs spinner animation.
func (m *Model) NeedsTick() bool {
	for _, entry := range m.toolEntries {
		if entry.msg.ToolStatus == types.ToolStatusPending ||
			entry.msg.ToolStatus == types.ToolStatusRunning {
			return true
		}
	}
	return false
}

// GetToolFadeProgress returns 0 because completed tools are kept visible instead of fading out.
func (m *Model) GetToolFadeProgress(string) float64 {
	return 0
}

// ToolCount returns the number of tool calls in this block.
func (m *Model) ToolCount() int {
	return len(m.toolEntries)
}

// IsExpanded returns the current expanded state.
func (m *Model) IsExpanded() bool {
	return m.expanded
}

// Toggle is retained for the message interface; reasoning blocks are always expanded.
func (m *Model) Toggle() {}

// SetExpanded is retained for tests and callers that used to control collapse state.
// Reasoning blocks stay expanded so thinking and tool calls remain visible.
func (m *Model) SetExpanded(bool) {}

// SetSelected sets the selected state for visual highlighting.
func (m *Model) SetSelected(selected bool) {
	m.selected = selected
}

// messageStyle returns the appropriate style based on selection state.
func (m *Model) messageStyle() lipgloss.Style {
	if m.selected {
		return styles.SelectedMessageStyle
	}
	return styles.AssistantMessageStyle
}

// Init initializes the component.
func (m *Model) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, entry := range m.toolEntries {
		if cmd := entry.view.Init(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Forward updates to all tool views (for spinners, etc.)
	var cmds []tea.Cmd
	for i, entry := range m.toolEntries {
		updated, cmd := entry.view.Update(msg)
		m.toolEntries[i].view = updated
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// View renders the block.
func (m *Model) View() string {
	return m.renderExpanded()
}

// SetSize sets the component dimensions.
func (m *Model) SetSize(width, height int) tea.Cmd {
	m.width = width
	m.height = height
	contentWidth := m.contentWidth()
	for _, entry := range m.toolEntries {
		entry.view.SetSize(contentWidth, 0)
	}
	return nil
}

// GetSize returns the current dimensions.
func (m *Model) GetSize() (int, int) {
	return m.width, m.height
}

// Height calculates the rendered height.
func (m *Model) Height() int {
	return lipgloss.Height(m.View())
}

// contentWidth returns width available for content.
func (m *Model) contentWidth() int {
	return m.width - styles.AssistantMessageStyle.GetHorizontalFrameSize()
}

// renderExpanded renders the full block with all content in order.
func (m *Model) renderExpanded() string {
	var parts []string

	// Header
	header := m.renderHeader(true)
	parts = append(parts, header)

	// Render content items in order (interleaved reasoning and tools)
	for i, item := range m.contentItems {
		switch item.kind {
		case contentItemReasoning:
			if item.reasoning != "" {
				if i == 0 {
					parts = append(parts, "") // blank line after header for first item
				}
				parts = append(parts, m.renderReasoningChunk(item.reasoning))
			}
		case contentItemTool:
			if item.toolIndex < len(m.toolEntries) {
				// Blank line before first tool in a consecutive group
				if i == 0 || (i > 0 && m.contentItems[i-1].kind == contentItemReasoning) {
					parts = append(parts, "")
				}
				parts = append(parts, m.toolEntries[item.toolIndex].view.View())
				// Blank line after last tool in a consecutive group (next is reasoning or end)
				isLastItem := i == len(m.contentItems)-1
				nextIsReasoning := !isLastItem && m.contentItems[i+1].kind == contentItemReasoning
				if isLastItem || nextIsReasoning {
					parts = append(parts, "")
				}
			}
		}
	}

	return strings.Join(parts, "\n")
}

// renderHeader renders the header line.
func (m *Model) renderHeader(bool) string {
	badge := styles.ThinkingBadgeStyle.Render("Thinking")
	return m.messageStyle().Render(badge)
}

// renderReasoningChunk renders a single reasoning chunk with styling.
func (m *Model) renderReasoningChunk(text string) string {
	contentWidth := m.contentWidth()
	rendered, err := markdown.NewRenderer(contentWidth).Render(text)
	if err != nil {
		rendered = text
	}

	// Strip ANSI and apply muted italic style
	clean := strings.TrimRight(ansi.Strip(rendered), "\n\r\t ")
	styled := styles.MutedStyle.Italic(true).Render(clean)

	return m.messageStyle().Render(styled)
}

// StopAnimation stops all animation subscriptions for this reasoning block.
// This must be called when the block is removed from the UI to avoid leaked animation subscriptions.
func (m *Model) StopAnimation() {
	// Stop the block's own fade animation registration
	if m.animationRegistered {
		m.animationRegistered = false
		animation.Unregister()
	}
	// Stop spinners in all tool entries
	for _, entry := range m.toolEntries {
		stopViewAnimation(entry.view)
	}
}

// stopViewAnimation stops animation subscriptions for a view being removed.
type animationStopper interface {
	StopAnimation()
}

func stopViewAnimation(view layout.Model) {
	if stopper, ok := view.(animationStopper); ok {
		stopper.StopAnimation()
	}
}

// IsHeaderLine returns true if the given line index is the header (line 0).
func (m *Model) IsHeaderLine(lineIdx int) bool {
	return lineIdx == 0
}

// IsToggleLine returns false because reasoning blocks stay expanded.
func (m *Model) IsToggleLine(int) bool {
	return false
}
