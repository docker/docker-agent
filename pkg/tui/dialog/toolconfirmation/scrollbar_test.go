package toolconfirmation

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type scrollbarClock struct{ now time.Time }

func (c *scrollbarClock) Now() time.Time { return c.now }
func (c *scrollbarClock) Tick(delay time.Duration, deliver func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(delay)
		return deliver(c.now)
	}
}

func overflowingConfirmation(ar *animation.Runtime, width, height int) *toolConfirmationDialog {
	args := make([]string, 40)
	for i := range args {
		args[i] = fmt.Sprintf("%q: %d", fmt.Sprintf("key_%02d", i), i)
	}
	event := &runtime.ToolCallConfirmationEvent{
		Type:           "tool_call_confirmation",
		ToolCall:       tools.ToolCall{ID: "x", Function: tools.FunctionCall{Name: "write", Arguments: "{" + strings.Join(args, ",") + "}"}},
		ToolDefinition: tools.Tool{Name: "write"},
	}
	d := NewToolConfirmationDialog(ar, event, &service.EmbeddedSessionState{}).(*toolConfirmationDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return d
}

func TestConfirmationScrollbarHitTestFollowsWrappedTitle(t *testing.T) {
	wrapped := 0
	for width := 4; width <= 40; width++ {
		d := overflowingConfirmation(animation.NewRuntime(), width, 60)
		_, contentWidth := d.dialogDimensions()
		if lipgloss.Height(styles.DialogTitleStyle.Width(contentWidth).Render(toolconfirm.Title)) > 1 {
			wrapped++
		}
		sv := d.scrollbar()
		require.NotNil(t, sv)
		if !sv.NeedsScrollbar() {
			continue
		}
		row, _ := d.Position()
		lines := strings.Split(ansi.Strip(d.View()), "\n")
		separator := -1
		for i, line := range lines[1:] { // skip the top border
			i++
			if strings.Contains(line, strings.Repeat("─", contentWidth)) {
				separator = i
				break
			}
		}
		require.Positivef(t, separator, "width %d: separator must render", width)
		first := row + separator + 2 // View() inserts one blank row before the arguments
		last := first + sv.VisibleHeight() - 1
		x := sv.ScrollbarX()
		require.Falsef(t, d.scrollView.IsMouseOnScrollbar(x, first-1), "width %d: blank row above the arguments", width)
		require.Truef(t, d.scrollView.IsMouseOnScrollbar(x, first), "width %d: first argument row", width)
		require.Truef(t, d.scrollView.IsMouseOnScrollbar(x, last), "width %d: last argument row", width)
		require.Falsef(t, d.scrollView.IsMouseOnScrollbar(x, last+1), "width %d: row below the arguments", width)
	}
	require.Positive(t, wrapped, "the sweep must cover a wrapped title")
}

// The reusable dialog drives its own scrollbar without the app dialog manager.
func TestConfirmationOwnsScrollbarTicksHoverAndTeardown(t *testing.T) {
	clock := animation.NewRuntimeWithScheduler(&scrollbarClock{now: time.Unix(1, 0)})
	d := overflowingConfirmation(clock, 80, 40)
	sv := d.scrollbar()
	require.True(t, sv.NeedsScrollbar(), "the fixture must overflow")
	drain := func() {
		t.Helper()
		cmd := clock.EnsureRunning()
		for range 200 {
			if cmd == nil {
				break
			}
			tick, ok := clock.Accept(cmd().(animation.TickMsg))
			require.True(t, ok)
			_, _ = d.Update(tick)
			cmd = clock.Continue()
		}
		require.False(t, clock.HasActive(), "the dialog's ticks must expire")
	}

	d.View()
	before := sv.ScrollOffset()
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Less(t, sv.ScrollOffset(), before, "the arguments open scrolled to their end")
	require.True(t, clock.HasActive(), "keyboard scrolling reveals the scrollbar")
	drain()

	x, y := sv.ScrollbarX(), -1
	for row := range 40 {
		if d.scrollView.IsMouseOnScrollbar(x, row) {
			y = row
			break
		}
	}
	require.NotEqual(t, -1, y)
	_, cmd := d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.NotNil(t, cmd, "hover starts the fade-in")
	d.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	drain()

	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, clock.HasActive())
	d.StopAnimation()
	require.False(t, clock.HasActive(), "teardown releases the scrollbar subscription")
}
