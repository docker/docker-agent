package dialog

import (
	"cmp"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// closeDialogCmd is a shorthand used by every picker for sending CloseDialogMsg.
func closeDialogCmd() tea.Cmd { return core.CmdHandler(CloseDialogMsg{}) }

// -----------------------------------------------------------------------------
// Key map
// -----------------------------------------------------------------------------

// pickerKeyMap defines the standard navigation key bindings used by every
// list-with-filter picker dialog (command palette, theme picker, model
// picker, file picker, working-directory picker, …).
type pickerKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	Escape key.Binding
}

// defaultPickerKeyMap returns the standard picker key bindings.
func defaultPickerKeyMap() pickerKeyMap {
	return pickerKeyMap{
		Up:     key.NewBinding(key.WithKeys("up", "ctrl+k"), key.WithHelp("↑/ctrl+k", "up")),
		Down:   key.NewBinding(key.WithKeys("down", "ctrl+j"), key.WithHelp("↓/ctrl+j", "down")),
		Enter:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "execute")),
		Escape: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
	}
}

// -----------------------------------------------------------------------------
// Layout
// -----------------------------------------------------------------------------

// Default sizing shared by the model picker and theme picker.
const (
	pickerWidthPercent  = 80
	pickerMinWidth      = 50
	pickerMaxWidth      = 100
	pickerHeightPercent = 70
	pickerMaxHeight     = 150

	// pickerHorizontalChrome is the horizontal chrome of styles.DialogStyle
	// (border 1 + padding 2 on each side = 6 cells).
	pickerHorizontalChrome = 6
	// pickerContentStartX is the X offset from the dialog's left edge to the
	// first column of content (border + horizontal padding).
	pickerContentStartX = 3
)

// pickerLayout describes the dimensions and chrome offsets of a picker
// dialog. Concrete dialogs vary in how much chrome they have above/below
// the scrollable list; pickerLayout captures everything needed for sizing,
// mouse hit-testing, and scrollview placement.
type pickerLayout struct {
	WidthPercent  int // percentage of screen width to fill
	MinWidth      int // minimum dialog width in cells
	MaxWidth      int // maximum dialog width in cells
	HeightPercent int // percentage of screen height to fill
	MaxHeight     int // maximum dialog height in cells

	// ListOverhead is the number of rows of chrome (header + footer) outside
	// the scrollable list area, including dialog borders/padding.
	ListOverhead int

	// ListStartOffset is the Y offset from the top of the dialog to the
	// first row of the scrollable list area. Used for mouse hit-testing.
	ListStartOffset int
}

// -----------------------------------------------------------------------------
// pickerCore
// -----------------------------------------------------------------------------

// pickerCore bundles the state and behaviour shared by every list-with-filter
// dialog. Concrete dialogs embed it and add their own item type, filtering,
// and rendering.
type pickerCore struct {
	BaseDialog

	textInput  textinput.Model
	scrollview *scrollview.Model
	keyMap     pickerKeyMap
	layout     pickerLayout

	selected int

	// Double-click detection
	lastClickTime  time.Time
	lastClickIndex int
}

// newPickerCore returns a pickerCore initialised with a focused, blank text
// input, a scroll-bar-reserving scrollview, and the default key map.
func newPickerCore(layout pickerLayout, placeholder string) pickerCore {
	ti := textinput.New()
	ti.SetStyles(styles.DialogInputStyle)
	ti.Placeholder = placeholder
	ti.Focus()
	ti.CharLimit = 256
	ti.SetWidth(50)

	return pickerCore{
		textInput:      ti,
		scrollview:     scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
		keyMap:         defaultPickerKeyMap(),
		layout:         layout,
		lastClickIndex: -1,
	}
}

// -----------------------------------------------------------------------------
// Sizing & positioning
// -----------------------------------------------------------------------------

// dialogSize returns the dialog dimensions and the inner content width.
// Content width subtracts horizontal chrome and reserved scrollbar columns.
func (p *pickerCore) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	l := p.layout
	dialogWidth = max(min(p.Width()*l.WidthPercent/100, l.MaxWidth), l.MinWidth)
	maxHeight = min(p.Height()*l.HeightPercent/100, l.MaxHeight)
	contentWidth = dialogWidth - pickerHorizontalChrome - p.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

// regionWidth returns the scrollview region width (content + reserved cols).
func (p *pickerCore) regionWidth(contentWidth int) int {
	return contentWidth + p.scrollview.ReservedCols()
}

// Position returns the centred (row, col) of the dialog on screen.
func (p *pickerCore) Position() (row, col int) {
	dialogWidth, maxHeight, _ := p.dialogSize()
	return CenterPosition(p.Width(), p.Height(), dialogWidth, maxHeight)
}

// SetSize updates dialog dimensions and reconfigures the scrollview region.
func (p *pickerCore) SetSize(width, height int) tea.Cmd {
	cmd := p.BaseDialog.SetSize(width, height)
	_, maxHeight, contentWidth := p.dialogSize()
	visLines := max(1, maxHeight-p.layout.ListOverhead)
	p.scrollview.SetSize(p.regionWidth(contentWidth), visLines)
	return cmd
}

// updateScrollviewPosition repositions the scrollview for accurate mouse
// hit-testing. Concrete dialogs call this from their View() method.
func (p *pickerCore) updateScrollviewPosition() {
	dialogRow, dialogCol := p.Position()
	p.scrollview.SetPosition(dialogCol+pickerContentStartX, dialogRow+p.layout.ListStartOffset)
}

// -----------------------------------------------------------------------------
// Update helpers
// -----------------------------------------------------------------------------

// updateInput feeds msg into the embedded text input and runs the optional
// filter callback. It returns the textinput command (typically Blink) so the
// caller can pass it back from Update.
func (p *pickerCore) updateInput(msg tea.Msg, filter func()) tea.Cmd {
	var cmd tea.Cmd
	p.textInput, cmd = p.textInput.Update(msg)
	if filter != nil {
		filter()
	}
	return cmd
}

// handleListClick processes a mouse click on the list area. lineToItem maps a
// rendered line index (which may include separators) to an item index, or
// returns -1 for non-item lines; pass nil when the rendered list maps 1:1
// to items.
//
// It updates the selection internally and reports:
//   - doubleClicked: the same item was clicked twice within the threshold
//     (selection is also updated to the clicked item)
//   - changed: a single click moved the selection to a different item
//
// Both flags are false for non-list, non-left, or out-of-range clicks.
func (p *pickerCore) handleListClick(msg tea.MouseClickMsg, lineToItem func(int) int) (doubleClicked, changed bool) {
	if msg.Button != tea.MouseLeft {
		return false, false
	}
	idx := p.mouseListIndex(msg.Y, lineToItem)
	if idx < 0 {
		return false, false
	}
	if p.recordClick(idx) {
		p.selected = idx
		return true, true
	}
	changed = idx != p.selected
	p.selected = idx
	return false, changed
}

// navigate moves the selection by delta within [0, num-1] and ensures the
// new selection stays visible. lineForSelected returns the rendered line
// index of the selection (used by EnsureLineVisible); pass nil when the
// list maps 1:1 to items. Returns true when the selection actually moved.
func (p *pickerCore) navigate(delta, num int, lineForSelected func() int) bool {
	next := p.selected + delta
	if next < 0 || next >= num {
		return false
	}
	p.selected = next
	line := next
	if lineForSelected != nil {
		line = lineForSelected()
	}
	p.scrollview.EnsureLineVisible(line)
	return true
}

// mouseListIndex maps a mouse Y coordinate to an item index, or -1 when the
// click is outside the list or on a non-item line. lineToItem may be nil
// when the rendered list maps 1:1 to items.
func (p *pickerCore) mouseListIndex(y int, lineToItem func(line int) int) int {
	dialogRow, _ := p.Position()
	listStartY := dialogRow + p.layout.ListStartOffset
	visLines := p.scrollview.VisibleHeight()
	if y < listStartY || y >= listStartY+visLines {
		return -1
	}
	actualLine := p.scrollview.ScrollOffset() + (y - listStartY)
	if lineToItem == nil {
		return actualLine
	}
	return lineToItem(actualLine)
}

// recordClick stores the current click and reports whether it forms a
// double-click on the same item. idx must be a valid item index (>= 0).
func (p *pickerCore) recordClick(idx int) bool {
	now := time.Now()
	if idx == p.lastClickIndex && now.Sub(p.lastClickTime) < styles.DoubleClickThreshold {
		p.lastClickTime = time.Time{}
		p.lastClickIndex = -1
		return true
	}
	p.lastClickTime = now
	p.lastClickIndex = idx
	return false
}

// -----------------------------------------------------------------------------
// Empty / error placeholder rendering
// -----------------------------------------------------------------------------

// renderEmptyState fills the scrollview with a centred italic placeholder.
func (p *pickerCore) renderEmptyState(message string, contentWidth int) string {
	style := styles.DialogContentStyle.Italic(true).Align(lipgloss.Center).Width(contentWidth)
	return p.renderPlaceholder(style.Render(message))
}

// renderErrorState fills the scrollview with a centred error message.
func (p *pickerCore) renderErrorState(message string, contentWidth int) string {
	style := styles.ErrorStyle.Align(lipgloss.Center).Width(contentWidth)
	return p.renderPlaceholder(style.Render(message))
}

// renderPlaceholder fills the visible scrollview area with a single rendered
// line plus blank padding so the dialog keeps a stable height.
func (p *pickerCore) renderPlaceholder(rendered string) string {
	visLines := p.scrollview.VisibleHeight()
	lines := []string{"", rendered}
	for len(lines) < visLines {
		lines = append(lines, "")
	}
	return p.scrollview.ViewWithLines(lines)
}

// -----------------------------------------------------------------------------
// Sort comparator shared by sectioned pickers
// -----------------------------------------------------------------------------

// pickerSortKeys captures the comparison keys for ordering a picker item.
// Items with smaller Section appear first; within each section, items with
// IsCurrent=true appear first, then IsDefault=true, then alphabetically by
// Name (case-insensitive), then by Tiebreak.
type pickerSortKeys struct {
	Section   int
	IsCurrent bool
	IsDefault bool
	Name      string
	Tiebreak  string
}

// comparePickerSortKeys compares two pickerSortKeys; suitable for slices.SortFunc.
func comparePickerSortKeys(a, b pickerSortKeys) int {
	if a.Section != b.Section {
		return cmp.Compare(a.Section, b.Section)
	}
	if a.IsCurrent != b.IsCurrent {
		if a.IsCurrent {
			return -1
		}
		return 1
	}
	if a.IsDefault != b.IsDefault {
		if a.IsDefault {
			return -1
		}
		return 1
	}
	if al, bl := strings.ToLower(a.Name), strings.ToLower(b.Name); al != bl {
		return cmp.Compare(al, bl)
	}
	return cmp.Compare(a.Tiebreak, b.Tiebreak)
}

// -----------------------------------------------------------------------------
// Grouped list builder (lists with separators / headers)
// -----------------------------------------------------------------------------

// groupedList builds a list of rendered lines mixed with non-item lines
// (separators, headers) and tracks the mapping between line indices and
// item indices, so callers can do mouse hit-testing and selection scrolling
// without re-deriving the layout.
//
// Usage:
//
//	gl := newGroupedList()
//	for i, item := range filtered {
//		if needsSeparatorBefore(item) {
//			gl.AddNonItem(renderSeparator(item))
//		}
//		gl.AddItem(renderItem(item, i == selected))
//	}
//	lines := gl.Lines()
//	idx   := gl.ItemForLine(actualLine) // for mouse hit-testing
//	line  := gl.LineForItem(selected)   // for EnsureLineVisible
type groupedList struct {
	lines      []string
	lineToItem []int // -1 for non-item lines, item index otherwise
	itemToLine []int // line index for each item, in order of insertion
}

// newGroupedList returns an empty grouped list.
func newGroupedList() *groupedList { return &groupedList{} }

// AddNonItem appends a non-selectable line (header, separator, …).
func (g *groupedList) AddNonItem(line string) {
	g.lines = append(g.lines, line)
	g.lineToItem = append(g.lineToItem, -1)
}

// AddItem appends a selectable item line.
func (g *groupedList) AddItem(line string) {
	g.itemToLine = append(g.itemToLine, len(g.lines))
	g.lines = append(g.lines, line)
	g.lineToItem = append(g.lineToItem, len(g.itemToLine)-1)
}

// Lines returns the full ordered list of rendered lines.
func (g *groupedList) Lines() []string { return g.lines }

// LineToItem returns the line→item slice (-1 for non-item lines).
func (g *groupedList) LineToItem() []int { return g.lineToItem }

// ItemForLine returns the item index at the given rendered line, or -1
// when the line is a separator/header or out of range.
func (g *groupedList) ItemForLine(line int) int {
	if line < 0 || line >= len(g.lineToItem) {
		return -1
	}
	return g.lineToItem[line]
}

// LineForItem returns the rendered line index for the given item index, or 0
// when the item is out of range.
func (g *groupedList) LineForItem(item int) int {
	if item < 0 || item >= len(g.itemToLine) {
		return 0
	}
	return g.itemToLine[item]
}
