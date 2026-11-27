package editor

import (
	"image"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TextOccupancy appends the visible input glyph cells in screen coordinates,
// using origin as the editor view's top-left. Prompts, line numbers, padding,
// the attachment banner, placeholders, whitespace and ghosts are excluded.
type TextOccupancy interface {
	AppendOccupiedTextCells(dst []image.Rectangle, origin image.Point) []image.Rectangle
}

// textOccupancyKey lists every textarea input that changes which rendered
// glyph cells are visible. Caret and selection changes only restyle cells, so
// they are not part of the key except through the scroll offset they cause.
type textOccupancyKey struct {
	value                 string
	width, height, offset int
	row, column, gutter   int
	theme                 *styles.Theme
}

type textOccupancyCache struct {
	key   textOccupancyKey
	cells []image.Rectangle
	valid bool
}

// AppendOccupiedTextCells appends the occupied input cells translated by origin.
func (e *editor) AppendOccupiedTextCells(dst []image.Rectangle, origin image.Point) []image.Rectangle {
	var cells []image.Rectangle
	var rowOffset int
	if e.historySearch.active {
		e.occupancy = textOccupancyCache{}
		if !e.searchInput.Focused() || e.searchInput.Value() == "" {
			return dst
		}
		// Match the join in View: the search field comes after the rendered textarea.
		rowOffset = lipgloss.Height(e.textarea.View())
		view := e.searchInput.View()
		gutter := lipgloss.Width(e.searchInput.Prompt)
		cells = occupiedCellsFromView(view, gutter, lipgloss.Width(view)-gutter)
	} else {
		cells = e.occupiedTextCells()
	}
	if len(cells) == 0 {
		return dst
	}
	offset := origin.Add(e.textareaOrigin()).Add(image.Pt(0, rowOffset))
	dst = slices.Grow(dst, len(cells))
	for _, cell := range cells {
		dst = append(dst, cell.Add(offset))
	}
	return dst
}

// textareaOrigin is the textarea's top-left inside the editor View: the frame
// (margin, border and padding). The attachment banner is laid out separately.
func (e *editor) textareaOrigin() image.Point {
	frame := e.frame()
	return image.Pt(
		frame.GetMarginLeft()+frame.GetBorderLeftSize()+frame.GetPaddingLeft(),
		frame.GetMarginTop()+frame.GetBorderTopSize()+frame.GetPaddingTop(),
	)
}

// occupiedTextCells returns textarea-local cells, cached until a key input
// changes. The returned slice is owned by the cache and must not be retained.
func (e *editor) occupiedTextCells() []image.Rectangle {
	if !e.textarea.Focused() {
		e.occupancy = textOccupancyCache{}
		return nil
	}
	value := e.textarea.Value()
	if value == "" {
		e.occupancy = textOccupancyCache{}
		return nil
	}
	key := textOccupancyKey{
		value:  value,
		width:  e.textarea.Width(),
		height: e.textarea.Height(),
		offset: e.textarea.ScrollYOffset(),
		row:    e.textarea.Line(),
		column: e.textarea.Column(),
		gutter: e.textareaGutterWidth(),
		theme:  styles.CurrentTheme(),
	}
	if e.occupancy.valid && e.occupancy.key == key {
		return e.occupancy.cells
	}
	cells := occupiedCellsFromView(e.textarea.View(), key.gutter, key.width)
	e.occupancy = textOccupancyCache{key: key, cells: cells, valid: true}
	return cells
}

// textareaGutterWidth is the number of leading view columns that hold prompt
// and line-number decoration instead of input text.
func (e *editor) textareaGutterWidth() int {
	if e.textarea.Prompt == "" && !e.textarea.ShowLineNumbers {
		return 0
	}
	gutter := lipgloss.Width(stripANSI(e.textarea.Prompt))
	if e.textarea.ShowLineNumbers {
		// Mirrors textarea's line-number formatting: " %*v " with the digit
		// width of MaxHeight.
		gutter += len(strconv.Itoa(e.textarea.MaxHeight)) + 2
	}
	return gutter
}

// occupiedCellsFromView derives input cells from the textarea's own rendered
// rows, so soft wrapping, viewport scrolling and wide glyphs match what the
// user sees. Cells are in textarea-view coordinates; the first gutter columns
// hold prompt and line-number decoration and are never occupied. Each
// contiguous run of non-space graphemes on a row becomes one rectangle.
func occupiedCellsFromView(view string, gutter, width int) []image.Rectangle {
	var cells []image.Rectangle
	limit := gutter + width
	rows := strings.Split(stripANSI(view), "\n")
	for y, row := range rows {
		x := 0
		for rest, state := row, -1; rest != ""; {
			var cluster string
			var w int
			cluster, rest, w, state = uniseg.FirstGraphemeClusterInString(rest, state)
			start := x
			x += w
			if w == 0 || start < gutter || start >= limit || strings.TrimSpace(cluster) == "" {
				continue
			}
			end := min(x, limit)
			if last := len(cells) - 1; last >= 0 && cells[last].Min.Y == y && cells[last].Max.X == start {
				cells[last].Max.X = end
			} else {
				cells = append(cells, image.Rect(start, y, end, y+1))
			}
		}
	}
	return cells
}
