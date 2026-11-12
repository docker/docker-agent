package codemode

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestRenderJavascript(t *testing.T) {
	t.Parallel()
	for _, status := range []types.ToolStatus{types.ToolStatusPending, types.ToolStatusRunning, types.ToolStatusCompleted, types.ToolStatusError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			msg := types.ToolCallMessage("root", tools.ToolCall{
				ID: "js", Function: tools.FunctionCall{Name: "run_tools_with_javascript", Arguments: `{"script":"const result = await Echo({});\nreturn result;"}`},
			}, tools.Tool{Name: "run_tools_with_javascript", Annotations: tools.ToolAnnotations{Title: "Run tools with Javascript"}}, status)
			msg.Content = `{"value":"script result","stdout":"debug output\n","stderr":"warning\n"}`
			v := New(animation.NewRuntime(), msg, service.StaticSessionState{})
			v.SetSize(120, 0)
			text := ansi.Strip(v.View())
			assert.Contains(t, text, "Run tools with Javascript")
			assert.Contains(t, text, "const result = await Echo({});")
			assert.Contains(t, text, "return result;")
			assert.NotContains(t, text, "script result")
			assert.NotContains(t, text, "debug output")
			assert.NotContains(t, text, "warning")
			assert.NotContains(t, text, "stdout")
			assert.NotContains(t, text, "stderr")
			assert.NotContains(t, text, "copy")
			assert.NotContains(t, v.View(), "const result", "syntax styling should separate the keyword from the variable")
			assert.Equal(t, v.View(), v.(interface{ ExpandedView() string }).ExpandedView())
		})
	}
}

func TestRenderJavascriptMalformedArguments(t *testing.T) {
	t.Parallel()
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"script":`}}, tools.Tool{Name: "run_tools_with_javascript"}, types.ToolStatusError)
	msg.Content = "execution failed"
	v := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	assert.Contains(t, ansi.Strip(v.View()), "run_tools_with_javascript")
	assert.NotContains(t, v.View(), "execution failed")
}

func TestRenderJavascriptWrapsScript(t *testing.T) {
	t.Parallel()
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"script":"const result = await Echo({value: 'a very long argument that needs wrapping'});"}`}}, tools.Tool{Name: "javascript"}, types.ToolStatusRunning)
	v := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	for _, width := range []int{10, 40, 80} {
		v.SetSize(width, 0)
		for line := range strings.SplitSeq(v.View(), "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), width)
		}
	}
}
