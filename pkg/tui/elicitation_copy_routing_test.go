package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	messagecomponents "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func runCopyCommand(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	value := reflect.ValueOf(cmd())
	if value.IsValid() && value.Kind() == reflect.Slice {
		for i := range value.Len() {
			if child, ok := reflect.TypeAssert[tea.Cmd](value.Index(i)); ok {
				runCopyCommand(child)
			}
		}
	}
}

func TestBackgroundElicitationCopyOverTabBar(t *testing.T) {
	m, _, _ := wallClockRoot(t, 80, 30)
	m.tabBar.SetTabs([]messages.TabInfo{{SessionID: "one", Title: "One"}, {SessionID: "two", Title: "Two"}}, 0)
	_ = m.tabBar.View()
	var copied string
	restore := messagecomponents.SetClipboardWriterForTest(func(s string) error { copied = s; return nil })
	defer restore()
	d := dialog.NewElicitationDialog("```\ncopy this\n```", nil, nil)
	_, _ = m.dialogMgr.Update(dialog.OpenDialogMsg{Model: d, OriginatingEvent: &runtime.ElicitationRequestEvent{}})
	row, col := d.Position()
	lines := strings.Split(ansi.Strip(d.View()), "\n")
	var x, y int
	for i, line := range lines {
		before, _, ok := strings.Cut(line, markdown.CodeBlockCopyIcon)
		if ok {
			x, y = col+ansi.StringWidth(before), row+i
			break
		}
	}
	require.NotZero(t, y)
	m.contentHeight = y - 1
	require.Equal(t, regionTabBar, m.hitTestRegion(y))
	_, cmd := m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	runCopyCommand(cmd)
	assert.Equal(t, "copy this", copied)

	// Move the dialog so its label remains over the tab bar at a new position.
	_, _ = m.dialogMgr.Update(tea.MouseClickMsg{X: col + 4, Y: row + 1, Button: tea.MouseLeft})
	_, _ = m.dialogMgr.Update(tea.MouseMotionMsg{X: col + 9, Y: row + 3, Button: tea.MouseLeft})
	_, _ = m.dialogMgr.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	x += 5
	y += 2
	m.contentHeight = y - 1
	copied = ""
	_, cmd = m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	runCopyCommand(cmd)
	assert.Equal(t, "copy this", copied)

	// A tab outside the dialog remains clickable while the dialog is open.
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: styles.AppPadding + 2, Y: y, Button: tea.MouseLeft})
	assert.True(t, m.tabBar.IsDragging())
}
