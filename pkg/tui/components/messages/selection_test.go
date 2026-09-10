package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestDoubleClickDragExtendsSelectionAndCopiesRange(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{
		"hello world foo",
		"second line here",
		"third line ends",
	})

	// Click + release at the same spot: plain click, selection cleared but
	// multi-click tracking must survive.
	m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	m.handleMouseRelease(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	require.False(t, m.selection.active)

	// Second press at the same spot: double-click selects the word and keeps
	// tracking the button for drag extension.
	m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.selection.active, "double-click should select the word under the cursor")
	require.True(t, m.selection.mouseButtonDown, "double-click must keep tracking the button for drags")
	require.True(t, m.selection.anchored)
	pendingID := m.selection.pendingCopyID

	// Drag down to the third line: the selection must extend from the
	// anchored word instead of being ignored.
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 2})
	startLine, startCol, endLine, endCol := m.selection.normalized()
	assert.Equal(t, 0, startLine)
	assert.Equal(t, 0, startCol, "selection should start at the anchored word start")
	assert.Equal(t, 2, endLine)
	assert.Equal(t, 5, endCol)

	// Release copies the extended range.
	_, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: 5, Y: 2, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "drag release should trigger a copy")
	assert.Equal(t, "hello world foo\nsecond line here\nthird", m.extractSelectedText())

	// The debounced word copy scheduled by the double-click must be dead.
	assert.Nil(t, m.handleDebouncedCopy(DebouncedCopyMsg{ClickID: pendingID}),
		"drag must cancel the pending double-click word copy")
}

func TestDoubleClickWithoutDragStillCopiesWord(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{"hello world foo"})

	m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	m.handleMouseRelease(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	m.handleMouseRelease(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseLeft})

	require.True(t, m.selection.active, "word selection must survive the release")
	assert.Equal(t, "hello", m.extractSelectedText())

	// The debounced copy is still pending and must fire.
	assert.NotNil(t, m.handleDebouncedCopy(DebouncedCopyMsg{ClickID: m.selection.pendingCopyID}))
}

func TestDoubleClickDragUpwardsExtendsBackwards(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{
		"first line here",
		"hello world foo",
	})

	m.handleMouseClick(tea.MouseClickMsg{X: 7, Y: 1, Button: tea.MouseLeft})
	m.handleMouseRelease(tea.MouseReleaseMsg{X: 7, Y: 1, Button: tea.MouseLeft})
	m.handleMouseClick(tea.MouseClickMsg{X: 7, Y: 1, Button: tea.MouseLeft})
	require.True(t, m.selection.anchored)

	m.handleMouseMotion(tea.MouseMotionMsg{X: 6, Y: 0})
	startLine, startCol, endLine, endCol := m.selection.normalized()
	assert.Equal(t, 0, startLine)
	assert.Equal(t, 6, startCol)
	assert.Equal(t, 1, endLine)
	assert.Equal(t, 11, endCol, "anchored word end must stay selected when dragging up")

	_, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: 6, Y: 0, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	assert.Equal(t, "line here\nhello world", m.extractSelectedText())
}

func TestSelectionClearKeepsMultiClickTracking(t *testing.T) {
	t.Parallel()

	s := &selectionState{}
	s.detectClickType(3, 7)
	s.start(3, 7)
	s.clear()

	assert.False(t, s.active)
	assert.Equal(t, 1, s.clickCount, "clear must not erase click tracking")
	assert.False(t, s.lastClickTime.IsZero(), "clear must not erase the last click time")
	assert.Equal(t, 2, s.detectClickType(3, 7), "the next nearby click must register as a double-click")
}

func TestTripleClickSelectsLineAndCopiesViaDebounce(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{"  hello world foo   ", "second line"})

	for range 2 {
		m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: 0, Button: tea.MouseLeft})
		m.handleMouseRelease(tea.MouseReleaseMsg{X: 4, Y: 0, Button: tea.MouseLeft})
	}
	m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.selection.active)
	require.True(t, m.selection.anchored, "triple-click must anchor the line for drag extension")
	m.handleMouseRelease(tea.MouseReleaseMsg{X: 4, Y: 0, Button: tea.MouseLeft})

	assert.Equal(t, "hello world foo", m.extractSelectedText())
	assert.NotNil(t, m.handleDebouncedCopy(DebouncedCopyMsg{ClickID: m.selection.pendingCopyID}),
		"stationary triple-click must copy the line via the debounce")
}

func TestTripleClickDragExtendsSelection(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{"first line", "second line", "third line"})

	for range 2 {
		m.handleMouseClick(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
		m.handleMouseRelease(tea.MouseReleaseMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	}
	m.handleMouseClick(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.selection.mouseButtonDown, "triple-click must keep tracking the button")

	m.handleMouseMotion(tea.MouseMotionMsg{X: 6, Y: 2})
	_, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: 6, Y: 2, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "triple-click drag release should copy the extended range")
	assert.Equal(t, "first line\nsecond line\nthird", m.extractSelectedText())
}

func TestDoubleClickOnBlankLineFallsBackToDragSelection(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{"hello world", "", "second line"})

	// Click-release then press again on the blank line: the word selection
	// cannot anchor, so the press must start a live drag instead of dying.
	m.handleMouseClick(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	m.handleMouseRelease(tea.MouseReleaseMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	m.handleMouseClick(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	require.True(t, m.selection.mouseButtonDown, "press on blank line must start a live drag")
	require.True(t, m.IsSelecting())

	m.handleMouseMotion(tea.MouseMotionMsg{X: 8, Y: 2})
	_, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: 8, Y: 2, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "drag from a blank line must still copy on release")
	assert.Equal(t, "second l", m.extractSelectedText())
	assert.False(t, m.IsSelecting())
}

func TestDragReleaseResetsClickTracking(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{"first line here", "second line here"})

	// Full drag selection.
	m.handleMouseClick(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	m.handleMouseMotion(tea.MouseMotionMsg{X: 10, Y: 1})
	_, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: 10, Y: 1, Button: tea.MouseLeft})
	require.NotNil(t, cmd)

	// An immediate retry at the same spot must be a fresh single-click drag,
	// not a double-click word selection.
	m.handleMouseClick(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	assert.Equal(t, 1, m.selection.clickCount, "a completed drag must reset multi-click tracking")
	require.True(t, m.selection.mouseButtonDown)
	require.False(t, m.selection.anchored)

	m.handleMouseMotion(tea.MouseMotionMsg{X: 11, Y: 1})
	_, cmd = m.handleMouseRelease(tea.MouseReleaseMsg{X: 11, Y: 1, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	assert.Equal(t, "first line here\nsecond line", m.extractSelectedText())
}

func TestFourthQuickClickCyclesBackToDragSelection(t *testing.T) {
	t.Parallel()

	s := &selectionState{}
	assert.Equal(t, 1, s.detectClickType(0, 5))
	assert.Equal(t, 2, s.detectClickType(0, 5))
	assert.Equal(t, 3, s.detectClickType(0, 5))
	assert.Equal(t, 1, s.detectClickType(0, 5), "click count must cycle after a triple-click")
}

func TestCopiedFeedbackLabelWidthMatchesCopyLabels(t *testing.T) {
	t.Parallel()

	want := ansi.StringWidth(types.CopiedFeedbackLabel)
	assert.Equal(t, want, ansi.StringWidth(types.MessageCopyLabel),
		"copied feedback must keep the message copy label width")
	assert.Equal(t, want, ansi.StringWidth(markdown.CodeBlockCopyIcon),
		"copied feedback must keep the code block copy label width")
}

func TestApplyCopiedFlashSwapsLabel(t *testing.T) {
	t.Parallel()

	line := "        " + types.MessageCopyLabel
	m := newSelectionModel([]string{line})
	m.lineOffsets = []int{0}

	m.copiedFlash = &copiedFlash{msgIdx: 0, localLine: 0, seq: 1}
	out := m.applyCopiedFlash([]string{line}, 0)

	plain := ansi.Strip(out[0])
	assert.Contains(t, plain, types.CopiedFeedbackLabel)
	assert.NotContains(t, plain, types.MessageCopyLabel)
	assert.Equal(t, ansi.StringWidth(line), ansi.StringWidth(out[0]), "swap must preserve line width")

	// Expiry with a stale sequence keeps the flash; the right one clears it.
	m.handleCopiedFlashExpired(copiedFlashExpiredMsg{Seq: 0})
	require.NotNil(t, m.copiedFlash)
	m.handleCopiedFlashExpired(copiedFlashExpiredMsg{Seq: 1})
	assert.Nil(t, m.copiedFlash)

	// Without flash state the lines pass through untouched.
	out = m.applyCopiedFlash([]string{line}, 0)
	assert.Equal(t, line, out[0])
}

func TestApplyCopiedFlashOffscreenIsNoop(t *testing.T) {
	t.Parallel()

	line := "        " + types.MessageCopyLabel
	m := newSelectionModel([]string{line})
	m.lineOffsets = []int{0}
	m.copiedFlash = &copiedFlash{msgIdx: 0, localLine: 0, seq: 1}

	// Viewport starts below the flashed line.
	out := m.applyCopiedFlash([]string{"other"}, 5)
	assert.Equal(t, "other", out[0])
}

func TestClickOnCopyLabelFlashesCopied(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)
	msg := types.Agent(types.MessageTypeAssistant, "", "hello response")
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
	m.renderDirty = true
	m.View()

	// Hover the message so the copy label renders, then re-render.
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	m.View()

	var line, col int
	found := false
	for i := range m.totalHeight {
		rendered := m.renderedLine(i)
		plain := ansi.Strip(rendered)
		if before, _, ok := strings.Cut(plain, types.MessageCopyLabel); ok {
			line = i
			col = ansi.StringWidth(before)
			found = true
			break
		}
	}
	require.True(t, found, "hovered assistant message should render the copy label")

	_, cmd := m.handleMouseClick(tea.MouseClickMsg{X: col, Y: line, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "copy label click should produce a command")
	require.NotNil(t, m.copiedFlash, "copy label click should start the copied flash")

	out := ansi.Strip(m.View())
	assert.Contains(t, out, types.CopiedFeedbackLabel)
	assert.NotContains(t, out, types.MessageCopyLabel)

	// Once expired, the label comes back.
	m.handleCopiedFlashExpired(copiedFlashExpiredMsg{Seq: m.copiedFlash.seq})
	out = ansi.Strip(m.View())
	assert.Contains(t, out, types.MessageCopyLabel)
}
