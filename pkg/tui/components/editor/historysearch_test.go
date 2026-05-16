package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
)

func enterSearch(t *testing.T, e *editor) *editor {
	t.Helper()
	m, _ := e.EnterHistorySearch()
	return m.(*editor)
}

func TestHistorySearch(t *testing.T) {
	t.Parallel()

	setupEditor := func(t *testing.T, messages []string) *editor {
		t.Helper()
		tmpDir := t.TempDir()
		h, err := history.New(tmpDir)
		require.NoError(t, err)

		for _, msg := range messages {
			require.NoError(t, h.Add(msg))
		}

		e := New(h).(*editor)
		e.textarea.SetWidth(80)
		return e
	}

	press := func(t *testing.T, e *editor, msg tea.Msg) *editor {
		t.Helper()
		m, _ := e.Update(msg)
		return m.(*editor)
	}

	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	up := tea.KeyPressMsg{Code: tea.KeyUp}
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	backspace := tea.KeyPressMsg{Code: tea.KeyBackspace}

	typeStr := func(t *testing.T, e *editor, s string) *editor {
		t.Helper()
		for _, r := range s {
			e = press(t, e, tea.KeyPressMsg{Text: string(r)})
		}
		return e
	}

	t.Run("enter and exit search mode", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"cmd1", "cmd2"})

		assert.False(t, e.historySearch.active)

		e = enterSearch(t, e)
		assert.True(t, e.historySearch.active)
		assert.Empty(t, e.historySearch.query)
		assert.Empty(t, e.historySearch.match)
		assert.Empty(t, e.Value())

		e = press(t, e, esc)
		assert.False(t, e.historySearch.active)
		assert.Empty(t, e.Value())
	})

	t.Run("search query matching", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"deploy staging", "run tests", "deploy production"})

		e = enterSearch(t, e)

		e = typeStr(t, e, "te")
		assert.Equal(t, "te", e.historySearch.query)
		assert.Equal(t, "run tests", e.historySearch.match)
		assert.False(t, e.historySearch.failing)

		e = press(t, e, backspace)
		assert.Equal(t, "t", e.historySearch.query)
		assert.Equal(t, "deploy production", e.historySearch.match)
	})

	t.Run("cycling older matches with up arrow", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"echo 1", "echo 2", "echo 3"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "echo")
		assert.Equal(t, "echo 3", e.historySearch.match)

		e = press(t, e, up)
		assert.Equal(t, "echo 2", e.historySearch.match)

		e = press(t, e, up)
		assert.Equal(t, "echo 1", e.historySearch.match)
	})

	t.Run("cycling newer matches with down arrow", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"echo 1", "echo 2", "echo 3"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "echo")
		assert.Equal(t, "echo 3", e.historySearch.match)

		e = press(t, e, up)
		e = press(t, e, up)
		assert.Equal(t, "echo 1", e.historySearch.match)

		e = press(t, e, down)
		assert.Equal(t, "echo 2", e.historySearch.match)

		e = press(t, e, down)
		assert.Equal(t, "echo 3", e.historySearch.match)
	})

	t.Run("wrap around when cycling past oldest match", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"echo 1", "echo 2", "echo 3"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "echo")
		assert.Equal(t, "echo 3", e.historySearch.match)

		e = press(t, e, up)
		e = press(t, e, up)
		assert.Equal(t, "echo 1", e.historySearch.match)

		e = press(t, e, up)
		assert.Equal(t, "echo 3", e.historySearch.match)
	})

	t.Run("wrap around when cycling past newest match", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"echo 1", "echo 2", "echo 3"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "echo")
		assert.Equal(t, "echo 3", e.historySearch.match)

		e = press(t, e, down)
		assert.Equal(t, "echo 1", e.historySearch.match)
	})

	t.Run("accept match", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"cmd1", "cmd2", "cmd3"})
		e.SetValue("partial input")

		e = enterSearch(t, e)
		e = typeStr(t, e, "cmd2")
		assert.Equal(t, "cmd2", e.historySearch.match)

		e = press(t, e, enter)
		assert.False(t, e.historySearch.active)
		assert.Equal(t, "cmd2", e.Value())

		e = press(t, e, up)
		assert.Equal(t, "cmd1", e.Value())

		e = press(t, e, down)
		assert.Equal(t, "cmd2", e.Value())
		e = press(t, e, down)
		assert.Equal(t, "cmd3", e.Value())
	})

	t.Run("cancel restores original input", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"history"})
		e.SetValue("original input")

		e = enterSearch(t, e)
		assert.Empty(t, e.textarea.Value())

		e = press(t, e, esc)
		assert.False(t, e.historySearch.active)
		assert.Equal(t, "original input", e.textarea.Value())
	})

	t.Run("ctrl+g cancels search", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"history"})
		e.SetValue("original input")

		e = enterSearch(t, e)
		e = typeStr(t, e, "hist")
		assert.Equal(t, "history", e.historySearch.match)

		e = press(t, e, ctrlG)
		assert.False(t, e.historySearch.active)
		assert.Equal(t, "original input", e.textarea.Value())
	})

	t.Run("failing search status", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"foo"})

		e = enterSearch(t, e)
		assert.False(t, e.historySearch.failing)

		e = typeStr(t, e, "z")
		assert.True(t, e.historySearch.failing)
		assert.Empty(t, e.textarea.Value())
	})

	t.Run("empty history", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{})

		e = enterSearch(t, e)
		assert.True(t, e.historySearch.active)
		assert.False(t, e.historySearch.failing)
		assert.Empty(t, e.historySearch.match)
	})

	t.Run("case insensitive matching", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"Deploy Staging", "run tests"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "deploy")
		assert.Equal(t, "Deploy Staging", e.historySearch.match)
		assert.False(t, e.historySearch.failing)
	})

	t.Run("backspace when query is empty", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"foo"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "f")
		assert.Equal(t, "foo", e.historySearch.match)

		e = press(t, e, backspace)
		assert.Empty(t, e.historySearch.query)
		assert.Empty(t, e.historySearch.match)
		assert.False(t, e.historySearch.failing)
	})

	t.Run("enter while failing restores original", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"foo", "bar"})
		e.SetValue("original")

		e = enterSearch(t, e)
		e = typeStr(t, e, "zzz")
		assert.True(t, e.historySearch.failing)

		e = press(t, e, enter)
		assert.False(t, e.historySearch.active)
		assert.Equal(t, "original", e.Value())
	})

	t.Run("cancel does not change history pointer", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"first", "second", "third"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "first")
		assert.Equal(t, "first", e.historySearch.match)

		e = press(t, e, esc)
		assert.False(t, e.historySearch.active)

		e = press(t, e, up)
		assert.Equal(t, "third", e.Value())
	})

	t.Run("state fully reset on exit", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"hello"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "hel")

		e = press(t, e, enter)
		assert.False(t, e.historySearch.active)
		assert.Empty(t, e.historySearch.query)
		assert.Empty(t, e.historySearch.match)
		assert.Equal(t, -1, e.historySearch.matchIndex)
		assert.False(t, e.historySearch.failing)
		assert.Empty(t, e.historySearch.origTextValue)
	})

	t.Run("re-enter search after exiting", func(t *testing.T) {
		t.Parallel()
		e := setupEditor(t, []string{"aaa", "bbb"})

		e = enterSearch(t, e)
		e = typeStr(t, e, "aaa")
		e = press(t, e, enter)
		assert.Equal(t, "aaa", e.Value())

		e = enterSearch(t, e)
		assert.True(t, e.historySearch.active)
		assert.Empty(t, e.historySearch.query)
		assert.Equal(t, "aaa", e.historySearch.origTextValue)
		assert.Empty(t, e.historySearch.match)
	})
}
