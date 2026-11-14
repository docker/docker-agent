package ui

import (
	"bufio"
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestParseMouseClicks(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Key{Typ: KeyMouseClick, X: 9, Y: 3}, singleKey(t, "\x1b[<0;10;4M"))
	assert.Equal(t, Key{Typ: KeyMouseWheelUp, X: 9, Y: 3}, singleKey(t, "\x1b[<64;10;4M"))
	assert.Equal(t, Key{Typ: KeyMouseWheelDown, X: 9, Y: 3}, singleKey(t, "\x1b[<65;10;4M"))
	for _, sequence := range []string{
		"\x1b[<0;10;4m", "\x1b[<2;10;4M", "\x1b[<32;10;4M",
		"\x1b[<0;0;4M", "\x1b[<0;10;0M", "\x1b[<0;10M", "\x1b[<0;999999999999999999999999;4M",
	} {
		var parser InputParser
		assert.Empty(t, parser.Feed([]byte(sequence)), sequence)
		assert.Equal(t, []Key{{Typ: KeyRune, Runes: []rune{'x'}}}, parser.Feed([]byte("x")))
	}
	for split := 1; split < len("\x1b[<0;10;4M"); split++ {
		var parser InputParser
		sequence := []byte("\x1b[<0;10;4M")
		assert.Empty(t, parser.Feed(sequence[:split]))
		assert.Equal(t, []Key{{Typ: KeyMouseClick, X: 9, Y: 3}}, parser.Feed(sequence[split:]))
	}
}

func TestTerminalMouseMode(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	terminal := &Terminal{writer: bufio.NewWriter(&output)}
	terminal.SetMouseEnabled(false)
	assert.Empty(t, output.String())
	terminal.SetMouseEnabled(true)
	assert.Equal(t, ansi.SetModeMouseExtSgr+ansi.SetModeMouseNormal, output.String())
	output.Reset()
	terminal.SetMouseEnabled(true)
	assert.Empty(t, output.String())
	terminal.SetMouseEnabled(false)
	assert.Equal(t, ansi.ResetModeMouseNormal+ansi.ResetModeMouseExtSgr, output.String())
}

func TestRendererInteractionPreservesScrollback(t *testing.T) {
	t.Parallel()
	renderer, output := newTestRenderer(6)
	renderer.SetInteractive(true)
	renderer.Frame([]string{"summary", "input", "footer"}, 1, 0)
	assert.Contains(t, output.String(), "\x1b[2J\x1b[H")
	output.Reset()
	expanded := []string{"summary [-]", "detail", "detail", "detail", "detail", "detail", "summary [-]", "input", "footer"}
	renderer.RepaintInteraction()
	renderer.Frame(expanded, 7, 0)
	assert.Equal(t, 3, renderer.ViewportTop())
	assert.NotContains(t, output.String(), "\x1b[3J")
	output.Reset()
	renderer.RepaintInteraction()
	renderer.Frame([]string{"summary [+]", "input", "footer"}, 1, 0)
	assert.Zero(t, renderer.ViewportTop())
	assert.Contains(t, output.String(), "summary [+]")
	assert.NotContains(t, output.String(), "detail")
	assert.NotContains(t, output.String(), "\x1b[3J")
}

func TestRendererScrollExpandedActivity(t *testing.T) {
	t.Parallel()
	renderer, output := newTestRenderer(4)
	renderer.SetInteractive(true)
	lines := []string{"summary", "detail 1", "detail 2", "detail 3", "detail 4", "detail 5", "input", "footer"}
	renderer.Frame(lines, 6, 0)
	assert.Equal(t, 4, renderer.ViewportTop())
	output.Reset()
	renderer.Scroll(-3)
	renderer.Frame(lines, 6, 0)
	assert.Equal(t, 1, renderer.ViewportTop())
	assert.Contains(t, output.String(), "detail 1")
	assert.NotContains(t, output.String(), seqShowCursor)
	assert.NotContains(t, output.String(), "\x1b[3J")
	grown := append(slices.Clone(lines), "new activity")
	renderer.Frame(grown, 6, 0)
	assert.Equal(t, 1, renderer.ViewportTop(), "updates must not snap back to the bottom")
	renderer.SetSize(80, 5)
	renderer.Frame(grown, 6, 0)
	assert.Equal(t, 1, renderer.ViewportTop(), "resize preserves the reading position")
	renderer.Scroll(-100)
	renderer.Frame(grown, 6, 0)
	assert.Zero(t, renderer.ViewportTop())
	renderer.Scroll(100)
	renderer.Frame(grown, 6, 0)
	assert.Equal(t, 4, renderer.ViewportTop())
	assert.False(t, renderer.scrolled)
	renderer.Scroll(-3)
	renderer.Frame(grown, 6, 0)
	renderer.Follow()
	renderer.Frame(grown, 6, 0)
	assert.Equal(t, 4, renderer.ViewportTop())
	renderer.Scroll(-3)
	renderer.Frame(grown, 6, 0)
	renderer.RepaintInteraction()
	renderer.Frame([]string{"summary", "input", "footer"}, 1, 0)
	assert.Zero(t, renderer.ViewportTop(), "collapse clamps the viewport")
	assert.False(t, renderer.scrolled)
}

func compactTestTranscript(t *testing.T) (*Transcript, *service.SessionState) {
	t.Helper()
	transcript := NewTranscript()
	state := &service.SessionState{}
	state.SetCompactActivity(true)
	transcript.UpsertTool("root", tools.ToolCall{ID: "read", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"secret.txt"}`}}, tools.Tool{Name: "read_file"}, tuitypes.ToolStatusRunning)
	transcript.FinishTool("read", ToolResult{Result: tools.ResultSuccess("done")}, state)
	return transcript, state
}

func TestCompactSummaryToggle(t *testing.T) {
	t.Parallel()
	transcript, state := compactTestTranscript(t)
	render := func() string { return ansi.Strip(strings.Join(transcript.Lines(80, 0, false, state, nil), "\n")) }
	assert.NotContains(t, render(), "secret.txt")
	require.True(t, transcript.ToggleActivityAt(0))
	assert.Contains(t, render(), "secret.txt")
	assert.Contains(t, render(), "[-]")
	assert.Equal(t, 1, strings.Count(render(), "[-]"))
	lines := transcript.Lines(80, 0, false, state, nil)
	assert.Empty(t, lines[len(lines)-1])
	assert.NotEmpty(t, lines[len(lines)-2], "expanded groups have only one trailing separator")
	require.True(t, transcript.ToggleActivityAt(0))
	assert.NotContains(t, render(), "secret.txt")
	assert.False(t, transcript.ToggleActivityAt(-1))
	assert.False(t, transcript.ToggleActivityAt(999))
}
