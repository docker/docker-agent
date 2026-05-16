package reasoningblock

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	// previewLines is the number of reasoning lines to show when collapsed.
	previewLines = 3
	// completedToolVisibleDuration is how long a completed tool remains fully visible before fading.
	completedToolVisibleDuration = 1500 * time.Millisecond
	// completedToolFadeDuration is how long the fade-out effect lasts before hiding.
	completedToolFadeDuration = 1000 * time.Millisecond
	// fadeSteps is the number of discrete quantized fade levels.
	fadeSteps = 20
)

// fadeStyles is a pre-computed table of lipgloss styles for discrete fade levels.
// Index 0 = no fade (normal), index fadeSteps = fully faded.
var (
	fadeStyles     [fadeSteps + 1]lipgloss.Style
	fadeStylesOnce sync.Once
)

func initFadeStyles() {
	for i := range fadeSteps + 1 {
		progress := float64(i) / float64(fadeSteps)
		startR, startG, startB := 128, 128, 128
		endR, endG, endB := 48, 48, 56
		r := int(float64(startR) + progress*float64(endR-startR))
		g := int(float64(startG) + progress*float64(endG-startG))
		b := int(float64(startB) + progress*float64(endB-startB))
		c := lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", r, g, b))
		fadeStyles[i] = lipgloss.NewStyle().Foreground(c)
	}
}

// fadeStyleForProgress returns the pre-computed style for the given fade progress.
func fadeStyleForProgress(progress float64) lipgloss.Style {
	fadeStylesOnce.Do(initFadeStyles)
	idx := min(max(int(progress*fadeSteps), 0), fadeSteps)
	return fadeStyles[idx]
}

// nowFunc is the time function used to get the current time.
// Tests can override this for deterministic behavior.
var nowFunc = time.Now

// toolEntry holds a tool call message and its view.
type toolEntry struct {
	msg                   *types.Message
	view                  layout.Model
	collapsedVisibleUntil time.Time // Zero means no grace period (hide immediately when completed)
	fadeProgress          float64   // 0.0 = not fading, 0.0-1.0 = fading (higher = more faded)
	strippedCollapsed     string    // Cached ANSI-stripped collapsed view (set once on completion)
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

// renderCache holds cached markdown rendering results to avoid re-rendering on every View() call.
// Invalidated when reasoning content or width changes.
type renderCache struct {
	width            int      // width used for rendering
	reasoningVersion int      // version of reasoning content when cached
	lines            []string // all rendered lines (ANSI stripped)
	hasExtra         bool     // whether there's extra content beyond preview
}

type expandedToolView interface {
	ExpandedView() string
}

// Model represents a collapsible reasoning + tool calls block.
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
	reasoningVersion    int          // increments when reasoning content changes
	cache               *renderCache // cached rendering results
	animationRegistered bool         // whether we're registered with animation coordinator
}

// New creates a new reasoning block.
func New(id, agentName string, sessionState *service.SessionState) *Model {
	return &Model{
		id:           id,
		agentName:    agentName,
		expanded:     sessionState == nil || sessionState.ExpandThinking(),
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
	m.reasoningVersion++
	m.cache = nil // invalidate cache
}

// AppendReasoning appends to the reasoning content.
// Creates a new reasoning item if the last item was a tool, otherwise appends to the last reasoning item.
func (m *Model) AppendReasoning(content string) {
	if content == "" {
		return
	}

	m.reasoningVersion++
	m.cache = nil // invalidate cache

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
		// Check if this is a transition from in-progress to completed/error
		wasInProgress := entry.msg.ToolStatus == types.ToolStatusPending ||
			entry.msg.ToolStatus == types.ToolStatusRunning
		isCompleted := status == types.ToolStatusCompleted || status == types.ToolStatusError

		entry.msg.Content = strings.ReplaceAll(content, "\t", "    ")
		entry.msg.ToolStatus = status
		entry.msg.ToolResult = result

		// Set grace period if transitioning from in-progress to completed
		// Total visible time = completedToolVisibleDuration + completedToolFadeDuration
		// Fade animation is driven by global animation tick
		var animCmd tea.Cmd
		if wasInProgress && isCompleted {
			totalDuration := completedToolVisibleDuration + completedToolFadeDuration
			entry.collapsedVisibleUntil = nowFunc().Add(totalDuration)
			entry.fadeProgress = 0
			// Register with animation coordinator if not already
			if !m.animationRegistered {
				animCmd = animation.StartTickIfFirst()
				m.animationRegistered = true
			}
		}

		// Recreate view to pick up new state
		view := tool.New(entry.msg, m.sessionState)
		view.SetSize(m.contentWidth(), 0)
		m.toolEntries[i] = entry
		m.toolEntries[i].view = view

		// Pre-cache the ANSI-stripped collapsed view for fade rendering
		if wasInProgress && isCompleted {
			if cv, ok := view.(layout.CollapsedViewer); ok {
				m.toolEntries[i].strippedCollapsed = ansi.Strip(cv.CollapsedView())
			} else {
				m.toolEntries[i].strippedCollapsed = ansi.Strip(view.View())
			}
		}

		initCmd := view.Init()
		if animCmd != nil {
			return tea.Batch(initCmd, animCmd)
		}
		return initCmd
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

// computeFadeProgressAt computes the fade progress for all tools based on elapsed time.
// This makes fade progress time-based (tick-rate independent) - the tick only affects smoothness.
// A tool should fade if it's past its fade start time (collapsedVisibleUntil - completedToolFadeDuration).
func (m *Model) computeFadeProgressAt(now time.Time) {
	for i, entry := range m.toolEntries {
		if entry.collapsedVisibleUntil.IsZero() {
			continue // No grace period set
		}
		// Compute when fade should start
		fadeStartTime := entry.collapsedVisibleUntil.Add(-completedToolFadeDuration)
		if now.Before(fadeStartTime) {
			m.toolEntries[i].fadeProgress = 0 // Not time to fade yet
			continue
		}
		// Compute fade progress as a fraction of the fade duration (0.0 to 1.0)
		elapsed := now.Sub(fadeStartTime)
		progress := float64(elapsed) / float64(completedToolFadeDuration)
		m.toolEntries[i].fadeProgress = math.Min(progress, 1.0)
	}
}

// hasFadingTools returns true if any tools are still within their visibility/fade window.
// This must match the condition in getVisibleToolsCollapsed to avoid unregistering
// the animation while tools are still visible.
// Uses fadeProgress (computed just before this is called) for consistency.
func (m *Model) hasFadingTools() bool {
	for _, entry := range m.toolEntries {
		if entry.collapsedVisibleUntil.IsZero() {
			continue
		}
		// Tool needs ticks while fade hasn't completed
		if entry.fadeProgress < 1.0 {
			return true
		}
	}
	return false
}

// NeedsTick returns true if this reasoning block requires animation tick updates.
// This is true when:
//   - Any tool is pending/running (needs spinner animation)
//   - Any tool is still fading (fadeProgress < 1.0)
//
// The messages list uses this to decide whether to invalidate its render cache on ticks.
// Use fadeProgress (updated on ticks) to stay consistent with renderCollapsed/hasFadingTools.
func (m *Model) NeedsTick() bool {
	for _, entry := range m.toolEntries {
		// Check for in-progress tools (need spinner)
		if entry.msg.ToolStatus == types.ToolStatusPending ||
			entry.msg.ToolStatus == types.ToolStatusRunning {
			return true
		}
		// Check for tools within visibility/fade window
		if !entry.collapsedVisibleUntil.IsZero() && entry.fadeProgress < 1.0 {
			return true
		}
	}
	return false
}

// GetToolFadeProgress returns the fade progress for a tool (0.0 = not fading, 0.0-1.0 = fading).
func (m *Model) GetToolFadeProgress(toolCallID string) float64 {
	for _, entry := range m.toolEntries {
		if entry.msg.ToolCall.ID == toolCallID {
			return entry.fadeProgress
		}
	}
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

// Toggle switches between expanded and collapsed state.
func (m *Model) Toggle() {
	m.expanded = !m.expanded
}

// SetExpanded sets the expanded state directly.
func (m *Model) SetExpanded(expanded bool) {
	m.expanded = expanded
}

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
	switch msg.(type) {
	case messages.ThemeChangedMsg:
		// Theme changed - invalidate cached rendering
		m.cache = nil
	case animation.TickMsg:
		// Compute fade levels based on elapsed time (tick-rate independent)
		m.computeFadeProgressAt(nowFunc())
		// Unregister if no more fading tools (uses fadeProgress computed above)
		if m.animationRegistered && !m.hasFadingTools() {
			m.animationRegistered = false
			animation.Unregister()
		}
		// Continue to forward tick to tool views for their spinners
	}

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
	if m.expanded {
		return m.renderExpanded()
	}
	return m.renderCollapsed()
}

// SetSize sets the component dimensions.
func (m *Model) SetSize(width, height int) tea.Cmd {
	if m.width != width {
		m.cache = nil // invalidate cache on width change
	}
	m.width = width
	m.height = height
	contentWidth := m.contentWidth()
	for _, entry := range m.toolEntries {
		entry.view.SetSize(contentWidth, 0)
	}
	return nil
}

// ensureCache computes and caches the rendered reasoning lines if needed.
// Returns the cached result. Safe to call repeatedly; only re-renders when content or width changes.
func (m *Model) ensureCache() *renderCache {
	contentWidth := m.contentWidth()

	// Return existing cache if still valid
	if m.cache != nil && m.cache.width == contentWidth && m.cache.reasoningVersion == m.reasoningVersion {
		return m.cache
	}

	// Compute fresh cache
	reasoning := m.Reasoning()
	var lines []string
	if reasoning != "" {
		rendered, err := markdown.NewRenderer(contentWidth).Render(reasoning)
		if err != nil {
			rendered = reasoning
		}
		clean := strings.TrimRight(ansi.Strip(rendered), "\n\r\t ")
		lines = strings.Split(clean, "\n")
	}

	m.cache = &renderCache{
		width:            contentWidth,
		reasoningVersion: m.reasoningVersion,
		lines:            lines,
		hasExtra:         len(m.toolEntries) > 0 || len(lines) > previewLines,
	}
	return m.cache
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

	// Header with collapse affordance
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
				parts = append(parts, m.renderToolExpanded(m.toolEntries[item.toolIndex]))
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

func (m *Model) renderToolExpanded(entry toolEntry) string {
	if view, ok := entry.view.(expandedToolView); ok {
		return view.ExpandedView()
	}
	return entry.view.View()
}

// renderCollapsed renders the compact preview.
func (m *Model) renderCollapsed() string {
	var parts []string

	// Header with expand affordance
	header := m.renderHeader(false)
	parts = append(parts, header)

	// Last N lines of reasoning
	if m.Reasoning() != "" {
		preview, _ := m.renderReasoningPreviewWithTruncationInfo()
		if preview != "" {
			parts = append(parts, preview)
		}
	}

	// Show in-progress tools and recently completed tools (within grace period)
	visibleTools := m.getVisibleToolsCollapsed()
	if len(visibleTools) > 0 {
		parts = append(parts, "") // blank line before tools
		for i, entry := range visibleTools {
			// Prefer CollapsedView() for simplified rendering in collapsed state
			var toolView string
			if cv, ok := entry.view.(layout.CollapsedViewer); ok {
				toolView = cv.CollapsedView()
			} else {
				toolView = entry.view.View()
			}
			if entry.fadeProgress > 0 {
				// Use cached stripped text (populated once on tool completion)
				stripped := visibleTools[i].strippedCollapsed
				if stripped == "" {
					stripped = ansi.Strip(toolView)
				}
				toolView = fadeStyleForProgress(entry.fadeProgress).Render(stripped)
			}
			parts = append(parts, toolView)
		}
	}

	return strings.Join(parts, "\n")
}

// getVisibleToolsCollapsed returns tool entries that should be visible in collapsed view.
// This includes in-progress tools (pending/running) and recently completed tools that haven't fully faded.
// Must use the same logic as hasFadingTools to avoid unregistering animation while tools are still visible.
func (m *Model) getVisibleToolsCollapsed() []toolEntry {
	var visible []toolEntry
	for _, entry := range m.toolEntries {
		// Show in-progress tools
		if entry.msg.ToolStatus == types.ToolStatusPending ||
			entry.msg.ToolStatus == types.ToolStatusRunning {
			visible = append(visible, entry)
			continue
		}
		// For completed tools: visible if fade hasn't completed
		// This matches hasFadingTools() to ensure consistency
		if !entry.collapsedVisibleUntil.IsZero() && entry.fadeProgress < 1.0 {
			visible = append(visible, entry)
		}
	}
	return visible
}

// hasExtraContent returns true if there's content that would be shown when expanded
// but is hidden when collapsed (truncated reasoning or completed tool calls).
func (m *Model) hasExtraContent() bool {
	return m.ensureCache().hasExtra
}

// renderHeader renders the header line with toggle affordance.
func (m *Model) renderHeader(expanded bool) string {
	badge := styles.ThinkingBadgeStyle.Render("Thinking")

	// Use [+] to expand and [-] to collapse
	var indicator string
	switch {
	case expanded:
		indicator = styles.MutedStyle.Bold(true).Render(" [-]")
	case m.hasExtraContent():
		indicator = styles.MutedStyle.Bold(true).Render(" [+]")
	}

	// Add tool count indicator when collapsed
	var toolInfo string
	if !expanded && len(m.toolEntries) > 0 {
		if len(m.toolEntries) == 1 {
			toolInfo = styles.MutedStyle.Render(" (1 tool)")
		} else {
			toolInfo = styles.MutedStyle.Render(" (" + strconv.Itoa(len(m.toolEntries)) + " tools)")
		}
	}

	return m.messageStyle().Render(badge + indicator + toolInfo)
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

// renderReasoningPreviewWithTruncationInfo renders the last N lines of reasoning
// and returns whether the content was truncated.
func (m *Model) renderReasoningPreviewWithTruncationInfo() (string, bool) {
	cache := m.ensureCache()
	if len(cache.lines) == 0 {
		return "", false
	}

	// Filter empty lines for preview
	var lines []string
	for _, line := range cache.lines {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}

	// Take last N lines
	start := 0
	reasoningTruncated := false
	if len(lines) > previewLines {
		start = len(lines) - previewLines
		reasoningTruncated = true
	}
	previewLinesContent := lines[start:]

	// Style each line - dim the first line more if there's content above (truncated)
	var styledLines []string
	for i, line := range previewLinesContent {
		if i == 0 && reasoningTruncated {
			// Extra dim first line to indicate more content above
			styledLines = append(styledLines, styles.MutedStyle.Italic(true).Faint(true).Render(line))
		} else {
			styledLines = append(styledLines, styles.MutedStyle.Italic(true).Render(line))
		}
	}

	preview := strings.Join(styledLines, "\n")
	return m.messageStyle().Render(preview), reasoningTruncated
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

// IsToggleLine returns true if clicking this line should toggle the block.
// Only the header is toggleable.
func (m *Model) IsToggleLine(lineIdx int) bool {
	return m.IsHeaderLine(lineIdx) && (m.expanded || m.hasExtraContent())
}
