package shell

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestShellSpinnerDotsRefreshOnThemeChange(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	darkTheme := styles.DefaultTheme()
	lightTheme, err := styles.LoadTheme(styles.DefaultLightThemeRef)
	require.NoError(t, err)

	styles.ApplyTheme(darkTheme)
	oldAccent := styles.Accent

	ar := animation.NewRuntime()
	msg := &types.Message{
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls -la"}`},
		},
		ToolStatus: types.ToolStatusRunning,
	}
	view := New(ar, msg, service.StaticSessionState{})
	view.SetSize(80, 1)

	frames := animation.Chat.Frames()
	require.True(t, containsDots(view.View(), oldAccent, frames))

	styles.ApplyTheme(lightTheme)
	newAccent := styles.Accent
	require.NotEqual(t, oldAccent, newAccent, "test themes must differ in accent color")

	_, _ = view.Update(messages.ThemeChangedMsg{})
	out := view.View()
	require.True(t, containsDots(out, newAccent, frames), "spinner dots must use the new theme's color, got %q", out)
	require.False(t, containsDots(out, oldAccent, frames), "spinner dots must not keep the previous theme's color, got %q", out)
}

// The tool box rewrites ANSI styling around the glyph.
func containsDots(out string, fg color.Color, frames []string) bool {
	prefix := colorPrefix(fg)
	for _, f := range frames {
		if strings.Contains(out, prefix+f) {
			return true
		}
	}
	return false
}

func colorPrefix(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r>>8, g>>8, b>>8)
}
