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

func codeCopyClick(t *testing.T, d *ElicitationDialog, index int) tea.MouseClickMsg {
	t.Helper()
	require.Less(t, index, len(d.codeBlocks))
	block := d.codeBlocks[index]
	relY := block.Line - d.scrollview.ScrollOffset()
	require.GreaterOrEqual(t, relY, 0)
	require.Less(t, relY, d.scrollview.VisibleHeight())
	before, _, ok := strings.Cut(ansi.Strip(d.bodyLines[block.Line]), markdown.CodeBlockCopyIcon)
	require.True(t, ok)
	return tea.MouseClickMsg{X: d.scrollableCol + ansi.StringWidth(before), Y: d.scrollableRow + relY, Button: tea.MouseLeft}
}

func TestElicitationCopyCodeBlocks(t *testing.T) {
	var copied []string
	restore := messagecomponents.SetClipboardWriterForTest(func(s string) error {
		copied = append(copied, s)
		return nil
	})
	defer restore()

	longCode := "  echo '" + strings.Repeat("é", 90) + "'\n    printf '第二行'"
	message := "Run this:\n\n```sh\n" + longCode + "\n```\n\nNext:\n\n```\nother\n```"
	d := NewElicitationDialog(message, map[string]any{
		"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
	}, nil).(*ElicitationDialog)
	_, _ = d.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	view := ansi.Strip(d.View())
	require.Len(t, d.codeBlocks, 2)
	assert.Equal(t, 2, strings.Count(view, markdown.CodeBlockCopyIcon))

	click := codeCopyClick(t, d, 0)
	_, cmd := d.Update(click)
	require.NotNil(t, cmd)
	msgs := testutil.CollectMsgs(cmd)
	require.Equal(t, []string{longCode}, copied, "copy preserves source indentation and does not insert display wraps")
	assert.Contains(t, msgs, tea.SetClipboard(longCode)(), "OSC 52 copy accompanies the native clipboard")
	assert.False(t, d.boolValues[0])

	click = codeCopyClick(t, d, 1)
	_, cmd = d.Update(click)
	testutil.CollectMsgs(cmd)
	require.Equal(t, []string{longCode, "other"}, copied)

	click.X--
	_, cmd = d.Update(click)
	assert.Nil(t, cmd, "click beside the label must not copy")
	click.X++
	click.Button = tea.MouseRight
	_, cmd = d.Update(click)
	assert.Nil(t, cmd, "only left clicks copy")
	assert.Len(t, copied, 2)

	// The regular form remains clickable and responds to keyboard input.
	clickBodyLine(t, d, d.fieldStarts[0]+d.fieldGeoms[0].optionStarts[0])
	assert.True(t, d.boolValues[0])
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.False(t, d.boolValues[0])
}

func TestElicitationCopyScrolledResizedAndMoved(t *testing.T) {
	var copied string
	restore := messagecomponents.SetClipboardWriterForTest(func(s string) error { copied = s; return nil })
	defer restore()

	message := strings.Repeat("A paragraph.\n\n", 30) + "```\nexact command\n```"
	mgr := New().(*manager)
	_, _ = mgr.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	d := NewElicitationDialog(message, nil, nil).(*ElicitationDialog)
	_, _ = mgr.Update(OpenDialogMsg{Model: d})
	_ = d.View()
	require.Len(t, d.codeBlocks, 1)
	assert.Greater(t, d.codeBlocks[0].Line, d.scrollview.VisibleHeight())
	// An off-screen block cannot be hit through the title or footer.
	_, ok := d.codeBlockAt(d.scrollableCol, d.scrollableRow+d.codeBlocks[0].Line)
	assert.False(t, ok)

	d.scrollview.SetScrollOffset(d.codeBlocks[0].Line)
	_ = d.View()
	click := codeCopyClick(t, d, 0)
	_, cmd := mgr.Update(click)
	testutil.CollectMsgs(cmd)
	assert.Equal(t, "exact command", copied)

	_, _ = mgr.Update(tea.WindowSizeMsg{Width: 95, Height: 28})
	_ = d.View()
	d.scrollview.SetScrollOffset(d.codeBlocks[0].Line)
	_ = d.View()
	row, col := d.Position()
	_, _ = mgr.Update(tea.MouseClickMsg{X: col + 4, Y: row + 1, Button: tea.MouseLeft})
	_, _ = mgr.Update(tea.MouseMotionMsg{X: col + 9, Y: row + 3, Button: tea.MouseLeft})
	_, _ = mgr.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	require.Equal(t, 5, mgr.stack[0].offsetX)
	require.Equal(t, 2, mgr.stack[0].offsetY)
	click = codeCopyClick(t, d, 0)
	click.X += 5
	click.Y += 2
	copied = ""
	_, cmd = mgr.Update(click)
	testutil.CollectMsgs(cmd)
	assert.Equal(t, "exact command", copied)
}

func TestElicitationCopyWholeMessageAndKeepAnswer(t *testing.T) {
	var copied string
	restore := messagecomponents.SetClipboardWriterForTest(func(s string) error { copied = s; return nil })
	defer restore()

	message := "Text and `inline code`\n\n```\nlong command\n```"
	d := NewElicitationDialog(message, nil, nil).(*ElicitationDialog)
	_, _ = d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	_ = d.View()
	_, _ = d.Update(tea.PasteMsg{Content: "private answer"})
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	msgs := testutil.CollectMsgs(cmd)
	assert.Equal(t, message, copied)
	assert.NotContains(t, copied, "private answer")
	assert.Contains(t, msgs, tea.SetClipboard(message)())
	assert.Equal(t, "private answer", d.responseInput.Value())
	assert.Contains(t, d.helpPairs(false), "ctrl+o")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.NotNil(t, cmd, "submission remains available")

	empty := NewElicitationDialog("", nil, nil).(*ElicitationDialog)
	assert.NotContains(t, empty.helpPairs(false), "ctrl+o")
	_, cmd = empty.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	assert.Nil(t, cmd)
	assert.Equal(t, message, copied)
}

func TestElicitationCopyHelpFitsDialog(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type": "object", "properties": map[string]any{
			"choices": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"a", "b"}}},
		},
	}
	for _, size := range []struct{ width, height int }{{60, 25}, {90, 40}, {160, 60}} {
		d := NewElicitationDialog(strings.Repeat("Long text.\n\n", 30)+"```\ncopy me\n```", schema, nil).(*ElicitationDialog)
		_, _ = d.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := d.View()
		l := d.layout()
		assert.Equal(t, l.dialogHeight(), lipgloss.Height(view), "dialog height must include wrapped help at width %d", size.width)
		assert.LessOrEqual(t, lipgloss.Width(view), size.width)
		assert.Contains(t, ansi.Strip(view), "ctrl+o")
		assert.Contains(t, ansi.Strip(view), "esc")
		assert.Contains(t, ansi.Strip(view), "pgup/pgdn")
	}
}
