package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestToolRendererOptionReachesMessagesAndConfirmation(t *testing.T) {
	t.Parallel()

	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "weather", Arguments: `{}`}}
	def := tools.Tool{Name: "weather"}
	var pages []*chatPage
	for _, label := range []string{"first-renderer", "second-renderer"} {
		r := tool.NewRegistry()
		r.Register("weather", func(ar *animation.Runtime, msg *types.Message, state service.SessionStateReader) layout.Model {
			return toolcommon.NewBase(ar, msg, state, func(*types.Message, spinner.Spinner, service.SessionStateReader, int, int) string {
				return label
			})
		})
		sess := session.New()
		state := &service.SessionState{}
		state.SetExpandThinking(true)
		p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), state,
			WithToolRenderers(r), WithToolRenderers(nil)).(*chatPage)
		t.Cleanup(func() { Cleanup(p) })
		assert.Same(t, r, p.toolRenderers)
		p.messages.SetSize(100, 30)
		p.messages.AddOrUpdateToolCall("root", call, def, types.ToolStatusCompleted)
		p.messages.AppendReasoning("root", "Checking the forecast.")
		secondCall := call
		secondCall.ID = "call-2"
		p.messages.AddOrUpdateToolCall("root", secondCall, def, types.ToolStatusCompleted)
		assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeToolCall))
		assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
		pages = append(pages, p)
	}

	for i, label := range []string{"first-renderer", "second-renderer"} {
		out := ansi.Strip(pages[i].messages.View())
		assert.Equal(t, 2, strings.Count(out, label), "both the standalone tool and reasoning tool must use this page's registry")
		other := "first-renderer"
		if i == 0 {
			other = "second-renderer"
		}
		assert.NotContains(t, out, other)
		event := runtime.ToolCallConfirmation(call, def, "root", nil)
		cmd := pages[i].attentionDialogCmd(event)
		require.NotNil(t, cmd)
		open, ok := cmd().(dialog.OpenDialogMsg)
		require.True(t, ok)
		require.NotNil(t, open.Model)
		open.Model.SetSize(120, 40)
		assert.Contains(t, ansi.Strip(open.Model.View()), label)
		assert.NotContains(t, ansi.Strip(open.Model.View()), other)
	}
}
