package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
)

func TestEditorInsertAndText(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.Insert([]rune("hello"))
	assert.Equal(t, "hello", e.Text())
	assert.Equal(t, 5, e.cursor)

	e.MoveLeft()
	e.Insert([]rune("X"))
	assert.Equal(t, "hellXo", e.Text())
}

func TestEditorInsertStripsCarriageReturns(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.Insert([]rune("a\r\nb"))
	assert.Equal(t, "a\nb", e.Text())
}

func TestEditorBackspaceAndDelete(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.SetText("abc")
	e.Backspace()
	assert.Equal(t, "ab", e.Text())

	e.MoveLineStart()
	e.DeleteForward()
	assert.Equal(t, "b", e.Text())
}

func TestEditorWordOps(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.SetText("foo bar baz")
	e.MoveWordLeft()
	assert.Equal(t, 8, e.cursor)
	e.MoveWordLeft()
	assert.Equal(t, 4, e.cursor)

	e.MoveLineStart()
	e.MoveWordRight()
	assert.Equal(t, 3, e.cursor)

	e.MoveLineEnd()
	e.DeleteWordBack()
	assert.Equal(t, "foo bar ", e.Text())
}

func TestEditorLayoutSingleLine(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.SetText("hello")
	lines, row, col := e.Layout(20)
	require.Len(t, lines, 1)
	assert.Equal(t, 0, row)
	assert.Equal(t, PromptWidth+5, col)
	assert.LessOrEqual(t, DisplayWidth(lines[0]), 20)
}

func TestEditorLayoutWrapping(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.SetText(strings.Repeat("a", 25))
	lines, row, col := e.Layout(12) // content width 10
	require.Len(t, lines, 3)
	assert.Equal(t, 2, row)
	assert.Equal(t, PromptWidth+5, col)
	for _, l := range lines {
		assert.LessOrEqual(t, DisplayWidth(l), 12)
	}
}

func TestEditorLayoutPlaceholder(t *testing.T) {
	t.Parallel()
	e := NewEditor("type here")
	lines, row, col := e.Layout(40)
	require.Len(t, lines, 1)
	assert.Equal(t, 0, row)
	assert.Equal(t, PromptWidth, col)
	assert.Contains(t, lines[0], "type here")
}

func TestEditorVerticalMovement(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.SetText("line1\nline2\nline3")
	// cursor at end (line3)
	require.True(t, e.Up(40))
	_, _, col := e.Layout(40)
	assert.Equal(t, PromptWidth+5, col) // preserved column on "line2"

	require.True(t, e.Up(40))
	// now on the first row; up should fail and let history take over
	assert.False(t, e.Up(40))
}

func TestEditorHistory(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	require.NoError(t, e.RememberHistory("first"))
	require.NoError(t, e.RememberHistory("second"))

	e.SetText("draft")
	e.HistoryPrev()
	assert.Equal(t, "second", e.Text())
	e.HistoryPrev()
	assert.Equal(t, "first", e.Text())
	e.HistoryPrev()
	assert.Equal(t, "first", e.Text()) // clamped

	e.HistoryNext()
	assert.Equal(t, "second", e.Text())
	e.HistoryNext()
	assert.Equal(t, "draft", e.Text()) // restored draft
}

func TestEditorPersistentHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := history.New(dir)
	require.NoError(t, err)
	require.NoError(t, store.Add("from full tui"))

	e := NewEditor("", store)
	e.HistoryPrev()
	assert.Equal(t, "from full tui", e.Text())

	require.NoError(t, e.RememberHistory("from lean tui"))
	reloaded, err := history.New(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"from full tui", "from lean tui"}, reloaded.Messages)
}

func TestEditorHistoryDeduplicates(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	require.NoError(t, e.RememberHistory("same"))
	require.NoError(t, e.RememberHistory("same"))
	assert.Len(t, e.history, 1)
}
