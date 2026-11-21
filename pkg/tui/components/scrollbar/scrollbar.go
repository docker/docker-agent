package scrollbar

import (
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Width is the intrinsic width of the scrollbar component in terminal columns.
const Width = 1

type Model struct {
	totalHeight  int
	viewHeight   int
	scrollOffset int

	width  int
	height int

	xPos int
	yPos int

	dragging        bool
	dragStartY      int
	dragStartOffset int

	dim float64

	trackChar string
	thumbChar string

	cache viewCache
}

// viewCache memoizes the rendered scrollbar. View() is called on every frame
// while scrolling, but its output only changes when the thumb geometry moves
// or the styled cells change (drag state, theme switch). The styled cells are
// part of the key, so a theme change naturally invalidates the cache.
type viewCache struct {
	height      int
	thumbTop    int
	thumbHeight int
	trackCell   string
	thumbCell   string
	lines       []string
	result      string
}

func New() *Model {
	return &Model{
		width:     Width,
		trackChar: "│",
		thumbChar: "│",
	}
}

func (m *Model) SetDimensions(viewHeight, totalHeight int) {
	m.viewHeight = viewHeight
	m.height = viewHeight
	m.totalHeight = totalHeight
	// Clamp scroll offset to valid range after dimension change
	m.scrollOffset = max(0, min(m.scrollOffset, m.maxScrollOffset()))
}

func (m *Model) SetScrollOffset(offset int) {
	m.scrollOffset = max(0, min(offset, m.maxScrollOffset()))
}

func (m *Model) SetPosition(x, y int) {
	m.xPos = x
	m.yPos = y
}

func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && m.isMouseOnScrollbar(msg.X, msg.Y) {
			return m.handleClick(msg.Y)
		}

	case tea.MouseMotionMsg:
		if m.dragging {
			m.updateScrollFromDrag(msg.Y - m.dragStartY)
		}

	case tea.MouseReleaseMsg:
		if msg.Button == tea.MouseLeft {
			m.dragging = false
		}
	}

	return m, nil
}

func (m *Model) handleClick(y int) (*Model, tea.Cmd) {
	thumbTop, thumbHeight := m.calculateThumbPosition()
	relativeY := y - m.yPos

	switch {
	case relativeY >= thumbTop && relativeY < thumbTop+thumbHeight:
		m.dragging = true
		m.dragStartY = y
		m.dragStartOffset = m.scrollOffset
		return m, nil
	case relativeY < thumbTop:
		cmd := m.PageUp()
		return m, cmd
	default:
		cmd := m.PageDown()
		return m, cmd
	}
}

// SetDim blends the scrollbar's colors toward the terminal background.
func (m *Model) SetDim(dim float64) { m.dim = dim }

func (m *Model) dimStyle(style lipgloss.Style) lipgloss.Style {
	if m.dim == 0 {
		return style
	}
	fade := func(c color.Color) color.Color {
		r, g, b := styles.ColorToRGB(c)
		br, bg, bb := styles.ColorToRGB(styles.Background)
		return styles.RGBToColor(r+(br-r)*m.dim, g+(bg-g)*m.dim, b+(bb-b)*m.dim)
	}
	style = style.Foreground(fade(style.GetForeground()))
	if c := style.GetBackground(); c != (lipgloss.NoColor{}) {
		style = style.Background(fade(c))
	}
	return style
}

func (m *Model) View() string {
	if m.ViewLines() == nil {
		return ""
	}
	return m.cache.result
}

// ViewLines returns the rendered scrollbar as one string per track line.
// The returned slice is shared with the internal cache and must not be
// mutated. Returns nil when no scrollbar is needed.
func (m *Model) ViewLines() []string {
	if m.height <= 0 || m.totalHeight <= m.viewHeight {
		return nil
	}

	thumbTop, thumbHeight := m.calculateThumbPosition()

	thumbStyle := styles.ThumbStyle
	if m.dragging {
		thumbStyle = styles.ThumbActiveStyle
	}
	// Render each styled cell once instead of once per line.
	trackCell := m.dimStyle(styles.TrackStyle).Render(strings.Repeat(m.trackChar, m.width))
	thumbCell := m.dimStyle(thumbStyle).Render(strings.Repeat(m.thumbChar, m.width))

	c := &m.cache
	if c.lines != nil && c.height == m.height && c.thumbTop == thumbTop && c.thumbHeight == thumbHeight &&
		c.trackCell == trackCell && c.thumbCell == thumbCell {
		return c.lines
	}

	lines := make([]string, m.height)
	for i := range lines {
		if i >= thumbTop && i < thumbTop+thumbHeight {
			lines[i] = thumbCell
		} else {
			lines[i] = trackCell
		}
	}

	m.cache = viewCache{
		height:      m.height,
		thumbTop:    thumbTop,
		thumbHeight: thumbHeight,
		trackCell:   trackCell,
		thumbCell:   thumbCell,
		lines:       lines,
		result:      strings.Join(lines, "\n"),
	}
	return m.cache.lines
}

func (m *Model) calculateThumbPosition() (top, height int) {
	if m.totalHeight <= m.viewHeight || m.height <= 0 {
		return 0, 0
	}

	thumbHeight := max(1, (m.viewHeight*m.height)/m.totalHeight)

	maxScroll := m.maxScrollOffset()
	if maxScroll == 0 {
		return 0, thumbHeight
	}

	scrollableTrackHeight := m.height - thumbHeight
	thumbTop := (m.scrollOffset * scrollableTrackHeight) / maxScroll

	return thumbTop, thumbHeight
}

func (m *Model) isMouseOnScrollbar(x, y int) bool {
	return x >= m.xPos &&
		x < m.xPos+m.width &&
		y >= m.yPos &&
		y < m.yPos+m.height
}

func (m *Model) updateScrollFromDrag(deltaY int) {
	if m.height <= 0 {
		return
	}

	_, thumbHeight := m.calculateThumbPosition()
	scrollableTrackHeight := m.height - thumbHeight

	if scrollableTrackHeight <= 0 {
		return
	}

	maxScroll := m.maxScrollOffset()
	deltaScroll := (deltaY * maxScroll) / scrollableTrackHeight

	newOffset := m.dragStartOffset + deltaScroll
	m.scrollOffset = max(0, min(newOffset, maxScroll))
}

func (m *Model) maxScrollOffset() int {
	return max(0, m.totalHeight-m.viewHeight)
}

func (m *Model) ScrollUp() tea.Cmd {
	m.scrollOffset = max(0, m.scrollOffset-1)
	return nil
}

func (m *Model) ScrollDown() tea.Cmd {
	m.scrollOffset = min(m.scrollOffset+1, m.maxScrollOffset())
	return nil
}

func (m *Model) PageUp() tea.Cmd {
	m.scrollOffset = max(0, m.scrollOffset-m.viewHeight)
	return nil
}

func (m *Model) PageDown() tea.Cmd {
	m.scrollOffset = min(m.scrollOffset+m.viewHeight, m.maxScrollOffset())
	return nil
}

func (m *Model) GetScrollOffset() int {
	return m.scrollOffset
}

func (m *Model) IsDragging() bool {
	return m.dragging
}
