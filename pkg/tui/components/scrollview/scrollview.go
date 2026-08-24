// Package scrollview provides a composable scrollable view that pairs content
// with a fixed-position scrollbar.
//
// Simple path: call [Model.Update] + [Model.View].
// Advanced path (custom scroll management): use [Model.UpdateMouse],
// [Model.SetScrollOffset], and [Model.ViewWithLines].
package scrollview

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// ScrollKeyMap defines which keys trigger scroll actions.
type ScrollKeyMap struct {
	Up       key.Binding // optional — leave unset for list dialogs that use up/down for selection
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding // home
	Bottom   key.Binding // end
}

// DefaultScrollKeyMap returns a key map with page-up/down and home/end.
// Up/Down are intentionally unbound so list dialogs can use them for selection.
func DefaultScrollKeyMap() *ScrollKeyMap {
	return &ScrollKeyMap{
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		Top:      key.NewBinding(key.WithKeys("home")),
		Bottom:   key.NewBinding(key.WithKeys("end")),
	}
}

// ReadOnlyScrollKeyMap returns a key map where up/down/j/k also scroll.
func ReadOnlyScrollKeyMap() *ScrollKeyMap {
	return &ScrollKeyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k")),
		Down:     key.NewBinding(key.WithKeys("down", "j")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		Top:      key.NewBinding(key.WithKeys("home")),
		Bottom:   key.NewBinding(key.WithKeys("end")),
	}
}

type Option func(*Model)

// WithReserveScrollbarSpace always reserves gap+scrollbar columns, preventing layout shifts.
func WithReserveScrollbarSpace(v bool) Option {
	return func(m *Model) { m.reserveScrollbarSpace = v }
}

// WithWheelStep sets lines scrolled per wheel tick (default 2).
func WithWheelStep(n int) Option { return func(m *Model) { m.wheelStep = n } }

// WithKeyMap sets keyboard bindings for scroll actions. Pass nil to disable.
func WithKeyMap(km *ScrollKeyMap) Option { return func(m *Model) { m.keyMap = km } }

// Model is a composable scrollable view that owns a scrollbar and ensures
// fixed-width rendering.
type Model struct {
	sb *scrollbar.Model

	xPos, yPos    int
	width, height int

	gapWidth              int
	reserveScrollbarSpace bool
	wheelStep             int
	keyMap                *ScrollKeyMap

	lines       []string
	totalHeight int

	// lineWidths lazily caches the display width of each content line. It is
	// invalidated when SetContent receives a different slice. Width
	// measurement (ansi.StringWidth) dominates per-frame compose cost for
	// large viewports, and content lines are immutable between rebuilds.
	lineWidths []int

	// scrollOffset tracks the desired scroll position independently of the
	// scrollbar, so EnsureLineVisible works before SetContent is called.
	scrollOffset int
}

// New creates a new scrollview with the given options.
func New(opts ...Option) *Model {
	m := &Model{
		sb:        scrollbar.New(),
		gapWidth:  1,
		wheelStep: 2,
		keyMap:    DefaultScrollKeyMap(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetSize sets the total width and height of the scrollable region.
func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.updateScrollbarPosition()
}

// SetPosition sets the absolute screen position (for mouse hit-testing).
func (m *Model) SetPosition(x, y int) {
	m.xPos = x
	m.yPos = y
	m.updateScrollbarPosition()
}

// SetContent provides the full content buffer and total height.
// totalHeight may be >= len(lines) for virtual blank lines (e.g. bottomSlack).
// The lines slice must not be mutated in place after being passed here;
// callers rebuild a fresh slice when content changes.
func (m *Model) SetContent(lines []string, totalHeight int) {
	if len(lines) != len(m.lines) || (len(lines) > 0 && &lines[0] != &m.lines[0]) {
		m.lineWidths = nil
	}
	m.lines = lines
	m.totalHeight = max(totalHeight, len(lines))
	m.sb.SetDimensions(m.height, m.totalHeight)
}

// lineWidth returns the display width of content line i, memoized across frames.
func (m *Model) lineWidth(i int) int {
	if m.lineWidths == nil {
		m.lineWidths = make([]int, len(m.lines))
		for j := range m.lineWidths {
			m.lineWidths[j] = -1
		}
	}
	if w := m.lineWidths[i]; w >= 0 {
		return w
	}
	w := ansi.StringWidth(m.lines[i])
	m.lineWidths[i] = w
	return w
}

// NeedsScrollbar returns true if content is taller than the viewport.
func (m *Model) NeedsScrollbar() bool { return m.totalHeight > m.height }

// ContentWidth returns the width available for content text.
func (m *Model) ContentWidth() int {
	if m.reserveScrollbarSpace || m.NeedsScrollbar() {
		return max(1, m.width-m.gapWidth-scrollbar.Width)
	}
	return max(1, m.width)
}

// ReservedCols returns columns reserved for gap + scrollbar.
func (m *Model) ReservedCols() int { return m.gapWidth + scrollbar.Width }

// VisibleHeight returns the viewport height in lines.
func (m *Model) VisibleHeight() int { return m.height }

// ScrollbarX returns the absolute screen X of the scrollbar column.
func (m *Model) ScrollbarX() int { return m.xPos + m.width - scrollbar.Width }

// ScrollOffset returns the current scroll offset.
func (m *Model) ScrollOffset() int { return m.scrollOffset }

// SetScrollOffset sets the scroll offset, clamped when content dimensions are known.
func (m *Model) SetScrollOffset(offset int) {
	m.scrollOffset = max(0, offset)
	if m.totalHeight > 0 && m.height > 0 {
		m.scrollOffset = min(m.scrollOffset, max(0, m.totalHeight-m.height))
	}
	m.sb.SetScrollOffset(m.scrollOffset)
}

// ScrollBy adjusts the scroll offset by delta lines.
func (m *Model) ScrollBy(delta int) { m.SetScrollOffset(m.scrollOffset + delta) }
func (m *Model) LineUp()            { m.ScrollBy(-1) }
func (m *Model) LineDown()          { m.ScrollBy(1) }
func (m *Model) PageUp()            { m.ScrollBy(-m.height) }
func (m *Model) PageDown()          { m.ScrollBy(m.height) }
func (m *Model) ScrollToTop()       { m.SetScrollOffset(0) }
func (m *Model) ScrollToBottom()    { m.SetScrollOffset(m.totalHeight) }

// EnsureLineVisible scrolls minimally to bring a line into the viewport.
// Works before [SetContent] — only needs [SetSize].
func (m *Model) EnsureLineVisible(line int) {
	m.EnsureRangeVisible(line, line)
}

// EnsureRangeVisible scrolls minimally to bring lines startLine..endLine into
// the view. If the range is taller than the view, the start is prioritized.
func (m *Model) EnsureRangeVisible(startLine, endLine int) {
	startLine = max(0, startLine)
	endLine = max(startLine, endLine)
	if endLine >= m.scrollOffset+m.height {
		m.SetScrollOffset(endLine - m.height + 1)
	}
	if startLine < m.scrollOffset {
		m.SetScrollOffset(startLine)
	}
}

// Update handles mouse (scrollbar click/drag/wheel) and keyboard scroll events.
// Returns handled=true when the event was consumed.
func (m *Model) Update(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	m.updateScrollbarPosition() // Ensure scrollbar position is fresh for hit-testing
	switch msg := msg.(type) {
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return m.UpdateMouse(msg)

	case messages.WheelCoalescedMsg:
		if msg.Delta != 0 {
			m.ScrollBy(msg.Delta * m.wheelStep)
			return true, nil
		}

	case tea.MouseWheelMsg:
		switch msg.Button.String() {
		case "wheelup":
			m.ScrollBy(-m.wheelStep)
			return true, nil
		case "wheeldown":
			m.ScrollBy(m.wheelStep)
			return true, nil
		}

	case tea.KeyPressMsg:
		if m.keyMap == nil {
			return false, nil
		}
		switch {
		case m.keyMap.Up.Enabled() && key.Matches(msg, m.keyMap.Up):
			m.LineUp()
			return true, nil
		case m.keyMap.Down.Enabled() && key.Matches(msg, m.keyMap.Down):
			m.LineDown()
			return true, nil
		case key.Matches(msg, m.keyMap.PageUp):
			m.PageUp()
			return true, nil
		case key.Matches(msg, m.keyMap.PageDown):
			m.PageDown()
			return true, nil
		case key.Matches(msg, m.keyMap.Top):
			m.ScrollToTop()
			return true, nil
		case key.Matches(msg, m.keyMap.Bottom):
			m.ScrollToBottom()
			return true, nil
		}
	}
	return false, nil
}

// UpdateMouse delegates mouse events to the scrollbar. Low-level alternative to [Update].
func (m *Model) UpdateMouse(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	prev := m.scrollOffset
	sb, c := m.sb.Update(msg)
	m.sb = sb
	m.scrollOffset = m.sb.GetScrollOffset()
	return m.scrollOffset != prev || m.sb.IsDragging(), c
}

// IsDragging returns whether the scrollbar thumb is being dragged.
func (m *Model) IsDragging() bool { return m.sb.IsDragging() }

// View renders the scrollable region with automatic content slicing.
func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()

	nLines := m.height
	if !m.NeedsScrollbar() {
		nLines = min(m.height, max(0, len(m.lines)-m.scrollOffset))
	}
	visible := make([]string, nLines)
	for i := range nLines {
		if idx := m.scrollOffset + i; idx < len(m.lines) {
			visible[i] = m.lines[idx]
		}
	}
	return m.compose(visible, m.scrollOffset)
}

// ViewWithLines renders pre-sliced visible lines with the scrollbar.
func (m *Model) ViewWithLines(visibleLines []string) string {
	return m.viewWithLines(visibleLines, -1)
}

// ViewWithPaddedLines renders pre-sliced lines that are already exactly
// ContentWidth columns wide. It skips ANSI/grapheme measurement and wrapping;
// callers must uphold the width contract.
func (m *Model) ViewWithPaddedLines(visibleLines []string) string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()
	if m.NeedsScrollbar() && len(visibleLines) < m.height {
		result := make([]string, m.height)
		copy(result, visibleLines)
		visibleLines = result
	}
	return m.composePadded(visibleLines)
}

func (m *Model) composePadded(lines []string) string {
	switch {
	case m.NeedsScrollbar():
		sbLines := m.sb.ViewLines()
		gap := strings.Repeat(" ", m.gapWidth)
		var b strings.Builder
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			b.WriteString(gap)
			if i < len(sbLines) {
				b.WriteString(sbLines[i])
			} else {
				b.WriteString(strings.Repeat(" ", scrollbar.Width))
			}
		}
		return b.String()
	case m.reserveScrollbarSpace:
		blank := strings.Repeat(" ", m.gapWidth+scrollbar.Width)
		var b strings.Builder
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			b.WriteString(blank)
		}
		return b.String()
	default:
		return strings.Join(lines, "\n")
	}
}

// ViewWithRestyledLines is like [Model.ViewWithLines] for callers whose
// visibleLines are sliced from the content set via [Model.SetContent] at the
// current scroll offset (possibly restyled, e.g. selection or hover
// highlights). Unchanged lines reuse memoized width lookups in compose;
// restyled lines are re-measured.
func (m *Model) ViewWithRestyledLines(visibleLines []string) string {
	return m.viewWithLines(visibleLines, m.scrollOffset)
}

func (m *Model) viewWithLines(visibleLines []string, baseLine int) string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()

	if m.NeedsScrollbar() && len(visibleLines) < m.height {
		result := make([]string, m.height)
		copy(result, visibleLines)
		return m.compose(result, baseLine)
	}
	return m.compose(visibleLines, baseLine)
}

// syncScrollbar syncs the local scroll offset to the scrollbar and reads back the clamped value.
func (m *Model) syncScrollbar() {
	m.sb.SetDimensions(m.height, m.totalHeight)
	m.sb.SetScrollOffset(m.scrollOffset)
	m.scrollOffset = m.sb.GetScrollOffset()
}

// compose pads/truncates lines to contentWidth and joins with the scrollbar
// column. When baseLine >= 0, lines[i] that are unchanged from content line
// baseLine+i take their display width from the memoized cache; restyled lines
// are re-measured since restyling may not be exactly width-preserving for
// complex grapheme clusters (ZWJ emoji, flags).
func (m *Model) compose(lines []string, baseLine int) string {
	contentWidth := m.ContentWidth()

	// Pad or truncate each line to exact content width
	for i, line := range lines {
		var w int
		// The equality check is O(1) for unchanged lines: they share the same
		// string backing, so it short-circuits on pointer identity.
		if gi := baseLine + i; baseLine >= 0 && gi < len(m.lines) && line == m.lines[gi] {
			w = m.lineWidth(gi)
		} else {
			w = ansi.StringWidth(line)
		}
		switch {
		case w > contentWidth:
			lines[i] = ansi.Truncate(line, contentWidth, "")
		case w < contentWidth:
			lines[i] = line + strings.Repeat(" ", contentWidth-w)
		}
	}

	contentView := strings.Join(lines, "\n")

	// Zip the right-side column (scrollbar or placeholder) directly: every
	// line is exactly contentWidth wide at this point, so JoinHorizontal's
	// per-line re-measuring would be pure overhead.
	switch {
	case m.NeedsScrollbar():
		sbLines := m.sb.ViewLines()
		gap := strings.Repeat(" ", m.gapWidth)
		var b strings.Builder
		b.Grow(len(contentView) + len(lines)*(m.gapWidth+scrollbar.Width*4))
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			b.WriteString(gap)
			if i < len(sbLines) {
				b.WriteString(sbLines[i])
			} else {
				b.WriteString(strings.Repeat(" ", scrollbar.Width))
			}
		}
		return b.String()
	case m.reserveScrollbarSpace:
		blank := strings.Repeat(" ", m.gapWidth+scrollbar.Width)
		var b strings.Builder
		b.Grow(len(contentView) + len(lines)*len(blank))
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			b.WriteString(blank)
		}
		return b.String()
	default:
		return contentView
	}
}

func (m *Model) updateScrollbarPosition() {
	m.sb.SetPosition(m.ScrollbarX(), m.yPos)
}
