package ansiline

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestSpliceLine(t *testing.T) {
	t.Parallel()
	t.Run("middle of plain line", func(t *testing.T) {
		got := Splice("hello world", "XY", 6)
		require.Equal(t, "hello XYrld", got)
	})

	t.Run("beyond end pads", func(t *testing.T) {
		got := Splice("hi", "XY", 5)
		require.Equal(t, "hi   XY", got)
	})

	t.Run("empty line", func(t *testing.T) {
		got := Splice("", "XY", 3)
		require.Equal(t, "   XY", got)
	})

	t.Run("at column zero", func(t *testing.T) {
		got := Splice("hello", "XY", 0)
		require.Equal(t, "XYllo", got)
	})

	t.Run("styled base keeps left styling and width", func(t *testing.T) {
		styled := "\x1b[31mhello world\x1b[0m"
		got := Splice(styled, "XY", 6)
		require.Equal(t, "hello XYrld", ansi.Strip(got))
		require.Equal(t, 11, ansi.StringWidth(got))
		require.Contains(t, got, "\x1b[31m", "left side keeps its color")
	})

	t.Run("styled overlay stays intact", func(t *testing.T) {
		overlay := "\x1b[7mX\x1b[0m"
		got := Splice("hello world", overlay, 6)
		require.Equal(t, "hello Xorld", ansi.Strip(got))
		require.Equal(t, 11, ansi.StringWidth(got))
		require.Contains(t, got, overlay)
	})

	t.Run("wide runes on the right boundary", func(t *testing.T) {
		got := Splice("ab界cd", "X", 1) // replaces b; 界 stays at cols 2-3
		require.Equal(t, "aX界cd", ansi.Strip(got))
		require.Equal(t, 6, ansi.StringWidth(got))
	})

	t.Run("overlay cutting into a wide rune keeps cell widths", func(t *testing.T) {
		// X lands on the first cell of 界 (cols 2-3); the half-covered wide
		// rune cannot be drawn, so its remaining cell becomes padding — the
		// same thing a terminal compositor does.
		got := Splice("ab界cd", "X", 2)
		require.Equal(t, 6, ansi.StringWidth(got))
		require.Equal(t, "abX cd", ansi.Strip(got))
	})
}
