package editor

import (
	"charm.land/bubbles/v2/textarea"
	"github.com/rivo/uniseg"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ContentLineCounter measures draft rows at the requested input width, without
// counting any placeholder, suggestion, or editor chrome.
type ContentLineCounter interface {
	ContentLineCount(width int) int
}

type contentHeight struct {
	measure textarea.Model
	value   string
	width   int
}

func (e *editor) ContentLineCount(width int) int {
	value := e.textarea.Value()
	if value == "" {
		return 1
	}
	if compactInput(width) {
		width += styles.EditorStyle.GetHorizontalFrameSize()
	}
	if e.textarea.MaxWidth > 0 {
		width = min(width, e.textarea.MaxWidth)
	}
	style := e.textarea.Styles().Blurred.Base
	if e.textarea.Focused() {
		style = e.textarea.Styles().Focused.Base
	}
	width = max(1, width-style.GetHorizontalFrameSize()-uniseg.StringWidth(e.textarea.Prompt))
	if e.contentHeight == nil {
		measure := textarea.New()
		measure.DynamicHeight = true
		measure.Prompt, measure.ShowLineNumbers = "", false
		measure.CharLimit, measure.MaxHeight = 0, 0
		measure.MaxWidth = 0
		e.contentHeight = &contentHeight{measure: measure}
	}
	c := e.contentHeight
	// DynamicHeight uses Bubbles' own visual rows (including the insertion
	// cell at an exact wrap boundary). An independent model keeps measurement
	// from moving the real viewport, cursor or selection.
	if c.width != width {
		c.measure.SetWidth(width)
		c.width = width
	}
	if c.value != value {
		c.measure.SetValue(value)
		c.value = value
	}
	return c.measure.Height()
}
