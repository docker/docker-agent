package input

import (
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestRapidEditsAndFocusChanges(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first, second := NewText(), NewText()
		first.Focus()
		pending := Expose(nil, &first)
		for _, ch := range "abc" {
			<-time.NewTimer(100 * time.Millisecond).C
			var cmd tea.Cmd
			first, cmd = first.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
			require.Nil(t, cmd, "typing should not emit a blink command")
		}
		typed := time.Now()
		pending = applyTick(t, pending, &first)
		require.Equal(t, 200*time.Millisecond, time.Since(typed))
		require.True(t, first.VirtualCursor(), "rapid edits extend the same cycle")

		first.Blur()
		second.Focus()
		other := Expose(&first, &second)
		require.False(t, first.VirtualCursor())
		require.True(t, second.VirtualCursor())

		<-time.NewTimer(100 * time.Millisecond).C
		second.Blur()
		first.Focus()
		resumed := Expose(&second, &first)
		require.Equal(t, "abc", first.Value())
		require.Nil(t, applyTick(t, pending, &first), "old ticks from the same input stay retired")
		require.Nil(t, applyTick(t, other, &first), "old ticks from another input stay retired")
		require.True(t, first.VirtualCursor())
		require.False(t, second.VirtualCursor())

		resumed = applyTick(t, resumed, &first)
		require.False(t, first.VirtualCursor())
		first, _ = first.Update(tea.PasteMsg{})
		require.Equal(t, "abc", first.Value())
		require.True(t, first.VirtualCursor(), "even an empty paste wakes the caret")
		<-time.NewTimer(200 * time.Millisecond).C
		first, _ = first.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		applyTick(t, resumed, &first)
		require.False(t, first.VirtualCursor(), "non-input messages do not extend the deadline")
	})
}

func TestDirectAreaEditsWakeCursor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		area := NewArea()
		area.Focus()
		pending := applyTick(t, Expose(nil, &area), &area)
		require.False(t, area.VirtualCursor())
		area.Reset()
		area.InsertString("")
		area.SetValue("")
		area.MoveToEnd()
		require.False(t, area.VirtualCursor(), "no-op mutations do not wake the caret")

		area.InsertString("ab\ncd")
		require.True(t, area.VirtualCursor())
		pending = applyTick(t, pending, &area)
		require.False(t, area.VirtualCursor())

		area.CursorUp()
		require.True(t, area.VirtualCursor())
		pending = applyTick(t, pending, &area)
		require.False(t, area.VirtualCursor())

		area.SetValue("ab\nef")
		require.True(t, area.VirtualCursor())
		pending = applyTick(t, pending, &area)
		require.False(t, area.VirtualCursor())

		area.Reset()
		require.True(t, area.VirtualCursor())
		Expose(&area, nil)
		area.InsertString("inactive")
		require.False(t, area.VirtualCursor(), "mutations of a covered input must not show its caret")
		require.Nil(t, applyTick(t, pending, &area))
	})
}

func applyTick(t *testing.T, cmd tea.Cmd, active Cursor) tea.Cmd {
	t.Helper()
	require.NotNil(t, cmd)
	tick, ok := cmd().(*BlinkMsg)
	require.True(t, ok)
	return tick.Apply(active)
}
