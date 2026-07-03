package dialog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func newTestContextDialog(t *testing.T, files []ContextFile) *contextDialog {
	t.Helper()
	d, ok := NewContextDialog(files).(*contextDialog)
	require.True(t, ok)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return d
}

func TestBuildContextFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	attached := writeTempFile(t, dir, "notes.md", strings.Repeat("a", 400))
	prompt := writeTempFile(t, dir, "AGENTS.md", strings.Repeat("b", 40))
	missing := filepath.Join(dir, "deleted.go")

	files := BuildContextFiles([]string{attached, missing}, []string{prompt})

	require.Len(t, files, 3)
	assert.Equal(t, ContextFile{Path: attached, Prompt: false, Tokens: 100}, files[0])
	assert.Equal(t, ContextFile{Path: missing, Prompt: false, Tokens: -1}, files[1])
	assert.Equal(t, ContextFile{Path: prompt, Prompt: true, Tokens: 10}, files[2])
}

func TestContextDialog_EmptyState(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, nil)
	out := d.View()
	assert.Contains(t, out, "Context Files")
	assert.Contains(t, out, "No files in context")
}

func TestContextDialog_RendersSectionsAndTokens(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, []ContextFile{
		{Path: "/abs/src/main.go", Tokens: 1200},
		{Path: "/abs/missing.md", Tokens: -1},
		{Path: "/abs/AGENTS.md", Prompt: true, Tokens: 100},
	})

	out := d.View()
	assert.Contains(t, out, "2 attached files")
	assert.Contains(t, out, "1 prompt file")
	assert.Contains(t, out, "~1.3K tokens") // summary total: 1200 + 100
	assert.Contains(t, out, "Attached files")
	assert.Contains(t, out, "Prompt files (agent config)")
	assert.Contains(t, out, "main.go")
	assert.Contains(t, out, "~1.2K tokens")
	assert.Contains(t, out, "missing")
	assert.Contains(t, out, "AGENTS.md")
}

func TestContextDialog_Navigation(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, []ContextFile{
		{Path: "/abs/a.go"},
		{Path: "/abs/b.go"},
		{Path: "/abs/AGENTS.md", Prompt: true},
	})

	down := tea.KeyPressMsg{Code: tea.KeyDown}
	up := tea.KeyPressMsg{Code: tea.KeyUp}

	require.Equal(t, 0, d.selected)
	d.Update(down)
	require.Equal(t, 1, d.selected)
	d.Update(down)
	require.Equal(t, 2, d.selected)
	d.Update(down)
	require.Equal(t, 2, d.selected, "down must not move past the last file")
	d.Update(up)
	d.Update(up)
	d.Update(up)
	require.Equal(t, 0, d.selected, "up must not move before the first file")
}

func TestContextDialog_DropEmitsMsgAndRemovesItem(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, []ContextFile{
		{Path: "/abs/a.go"},
		{Path: "/abs/b.go"},
	})

	_, cmd := d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.NotNil(t, cmd)
	msg := cmd()
	require.Equal(t, messages.DropAttachedFileMsg{FilePath: "/abs/a.go"}, msg)

	require.Len(t, d.files, 1)
	assert.Equal(t, "/abs/b.go", d.files[0].Path)
	assert.Equal(t, 0, d.selected)
}

func TestContextDialog_DropLastItemClampsSelection(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, []ContextFile{
		{Path: "/abs/a.go"},
		{Path: "/abs/b.go"},
	})

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 1, d.selected)

	_, cmd := d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.NotNil(t, cmd)
	require.Equal(t, messages.DropAttachedFileMsg{FilePath: "/abs/b.go"}, cmd())
	assert.Equal(t, 0, d.selected)

	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.NotNil(t, cmd)
	require.Equal(t, messages.DropAttachedFileMsg{FilePath: "/abs/a.go"}, cmd())
	assert.Empty(t, d.files)

	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	assert.Nil(t, cmd, "drop on an empty list must be a no-op")
}

func TestContextDialog_PromptFilesCannotBeDropped(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, []ContextFile{
		{Path: "/abs/a.go"},
		{Path: "/abs/AGENTS.md", Prompt: true},
	})

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.NotNil(t, cmd, "dropping a prompt file should surface a notification")
	_, isDrop := cmd().(messages.DropAttachedFileMsg)
	assert.False(t, isDrop, "prompt files must not emit a drop message")
	assert.Len(t, d.files, 2, "prompt files must stay listed")
}

func TestContextDialog_EscCloses(t *testing.T) {
	t.Parallel()
	d := newTestContextDialog(t, []ContextFile{{Path: "/abs/a.go"}})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	require.NotNil(t, cmd)
	assert.Equal(t, CloseDialogMsg{}, cmd())
}
