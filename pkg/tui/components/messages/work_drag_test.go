package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workDragView is a scrollable child probe; upstream shell cards have no body drag.
type workDragView struct {
	width, offset int
	dragging      bool
}

func (*workDragView) Init() tea.Cmd                            { return nil }
func (v *workDragView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (*workDragView) View() string                             { return strings.Repeat("output row\n", 40) }
func (v *workDragView) SetSize(width, _ int) tea.Cmd           { v.width = width; return nil }
func (v *workDragView) WheelAt(_, _, delta int) bool {
	v.offset = min(20, max(0, v.offset+delta))
	return true
}
func (v *workDragView) BodyScrollState() (int, bool, bool) { return v.offset, v.offset == 20, true }
func (v *workDragView) IsBodyDragging() bool               { return v.dragging }
func (v *workDragView) MouseAt(line, col int, msg tea.MouseMsg) (bool, tea.Cmd) {
	switch msg.(type) {
	case tea.MouseClickMsg:
		v.dragging = col == v.width-1
	case tea.MouseMotionMsg:
		if v.dragging {
			v.offset = line
		}
	case tea.MouseReleaseMsg:
		if v.dragging {
			v.dragging = false
			return true, nil
		}
	}
	return v.dragging, nil
}

func newDragWorkFixture(t *testing.T) (*model, *workDragView) {
	t.Helper()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddUserMessage("run it")
	view := &workDragView{}
	m.messages = append(m.messages, &types.Message{Type: types.MessageTypeToolCall})
	m.views = append(m.views, view)
	m.addMessage(&types.Message{Type: types.MessageTypeAssistant, Sender: "root", Content: "done"})
	m.SetCompactWorkLog(true)
	expandWork(t, m)
	m.scrollOffset = 0
	return m, view
}

// workScrollbarProbe finds a block-local press on the child body's scrollbar. It
// uses a separate fixture, since a successful press starts a drag.
func workScrollbarProbe(t *testing.T) (line, col int) {
	t.Helper()
	m, _ := newDragWorkFixture(t)
	block := m.work.ranges[0].block
	height := m.work.ranges[0].height
	for line := range height {
		for col := range m.contentWidth() {
			if handled, _ := block.MouseAt(line, col, tea.MouseClickMsg{X: col, Y: line, Button: tea.MouseLeft}); handled {
				return line, col
			}
		}
	}
	t.Fatal("the expanded child body exposes a scrollbar inside the Work block")
	return 0, 0
}

// workScreen maps a Work block-local line and column to a screen position.
func workScreen(m *model, line, col int) (x, y int) {
	m.updateScrollState()
	global := m.work.ranges[0].start + line
	return m.xPos + col, m.yPos + global - m.scrollOffset
}

func TestCompactWorkScrollbarPressCapturesDragAndForwardsMotionAndRelease(t *testing.T) {
	line, col := workScrollbarProbe(t)
	m, child := newDragWorkFixture(t)
	block := m.work.ranges[0].block

	x, y := workScreen(m, line, col)
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, block, m.work.drag, "the press on the grouped child's scrollbar captures the drag in the Work block")
	require.True(t, child.IsBodyDragging(), "the child holds its body drag")
	require.False(t, m.selection.mouseButtonDown, "a scrollbar press is not a text selection")

	before := child.offset
	m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y + 4})
	after := child.offset
	require.NotEqual(t, before, after, "motion moves the grouped child's body")
	require.Same(t, block, m.work.drag, "motion keeps the capture")

	m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: y + 4, Button: tea.MouseLeft})
	require.Nil(t, m.work.drag, "the release ends the capture")
	require.False(t, child.IsBodyDragging())
	require.False(t, m.selection.active, "the release is consumed and starts no copy or selection")
}

func TestCompactWorkDragKeepsCaptureOutsideWorkViewportAndReleaseEndsIt(t *testing.T) {
	line, col := workScrollbarProbe(t)
	m, child := newDragWorkFixture(t)
	x, y := workScreen(m, line, col)
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, m.work.drag)

	outside := m.yPos + m.height + 5
	m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: outside})
	require.NotNil(t, m.work.drag, "a drag past the transcript viewport stays captured")
	require.True(t, child.IsBodyDragging())

	m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: outside, Button: tea.MouseLeft})
	require.Nil(t, m.work.drag, "a release outside the viewport still ends the capture")
	require.False(t, child.IsBodyDragging())
}

func TestCompactWorkBodyPressOutsideScrollbarIsNotCaptured(t *testing.T) {
	m, child := newDragWorkFixture(t)
	block := m.work.ranges[0].block
	x, y := workScreen(m, 2, 6)
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Nil(t, m.work.drag)
	require.False(t, block.IsBodyDragging())
	require.False(t, child.IsBodyDragging())
}
