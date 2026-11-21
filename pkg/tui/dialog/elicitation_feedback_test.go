package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	messagecomponents "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/dialog/internal/testutil"
)

func TestElicitationSmallScreenGeometry(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object", "properties": map[string]any{"choices": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"a", "b"}}}}}
	for _, size := range []struct{ width, height int }{{40, 12}, {28, 25}, {60, 10}, {100, 24}} {
		d := NewElicitationDialog(strings.Repeat("words ", 100), schema, nil).(*ElicitationDialog)
		_, _ = d.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := d.View()
		assert.Equal(t, d.layout().dialogHeight(), lipgloss.Height(view), "%v", size)
		assert.LessOrEqual(t, lipgloss.Height(view), size.height, "%v", size)
		assert.LessOrEqual(t, lipgloss.Width(view), size.width, "%v", size)
		assert.GreaterOrEqual(t, d.scrollview.VisibleHeight(), 1)
	}
}

func TestElicitationMultilineTitleVisibleCopyClick(t *testing.T) {
	var copied string
	restore := messagecomponents.SetClipboardWriterForTest(func(s string) error { copied = s; return nil })
	defer restore()
	for _, title := range []string{"first line\nsecond line", strings.Repeat("long title ", 10)} {
		d := NewElicitationDialog("```\ncopy me\n```", nil, map[string]any{"cagent/title": title}).(*ElicitationDialog)
		_, _ = d.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
		view := strings.Split(ansi.Strip(d.View()), "\n")
		row, col := d.Position()
		found := false
		for y, line := range view {
			before, _, ok := strings.Cut(line, markdown.CodeBlockCopyIcon)
			if !ok {
				continue
			}
			found = true
			_, cmd := d.Update(tea.MouseClickMsg{X: col + ansi.StringWidth(before), Y: row + y, Button: tea.MouseLeft})
			require.NotNil(t, cmd)
			testutil.CollectMsgs(cmd)
			assert.Equal(t, "copy me", copied)
			copied = ""
		}
		require.True(t, found)
	}
}

func TestElicitationWholeMessageCopyRemovesHiddenControls(t *testing.T) {
	var copied string
	restore := messagecomponents.SetClipboardWriterForTest(func(s string) error { copied = s; return nil })
	defer restore()
	d := NewElicitationDialog("safe\x1b]0;\rid\r\x07", nil, nil).(*ElicitationDialog)
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	testutil.CollectMsgs(cmd)
	assert.Equal(t, "safe", copied)
}

func TestDialogContainsTopUsesOffsets(t *testing.T) {
	t.Parallel()
	mgr := New().(*manager)
	_, _ = mgr.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	d := NewElicitationDialog("text", nil, nil).(*ElicitationDialog)
	_, _ = mgr.Update(OpenDialogMsg{Model: d})
	row, col := d.Position()
	assert.True(t, mgr.ContainsTop(col, row))
	assert.False(t, mgr.ContainsTop(col-1, row))
	mgr.stack[0].offsetX = 5
	mgr.stack[0].offsetY = 3
	assert.False(t, mgr.ContainsTop(col, row))
	assert.True(t, mgr.ContainsTop(col+5, row+3))
}
