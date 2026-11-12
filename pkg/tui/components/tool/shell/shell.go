package shell

import (
	"strings"

	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const maxVisibleShellOutputLines = 20

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	extractCmd := toolcommon.ExtractField(func(a builtinshell.RunShellArgs) string { return a.Cmd })
	return toolcommon.NewBase(ar, msg, sessionState, func(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, height int) string {
		return renderShell(msg, s, sessionState, width, height, extractCmd)
	})
}

func renderShell(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int, extractCmd func(string) string) string {
	arg := ""
	if msg.ToolCall.Function.Arguments != "" {
		arg = extractCmd(msg.ToolCall.Function.Arguments)
	}

	result := ""
	if msg.Content != "" {
		result = formatShellOutput(msg.Content, width)
	}

	if preview, ok := commandPreview(arg, width); ok {
		header := toolcommon.RenderTool(msg, s, "", "", width, sessionState.HideToolResults())
		if sessionState.HideToolResults() {
			return header
		}
		content := header + "\n" + preview
		if result != "" {
			resultStyle := styles.ToolMessageStyle
			if msg.ToolStatus == types.ToolStatusError {
				resultStyle = styles.ToolErrorMessageStyle
			}
			content += "\n" + resultStyle.MarginLeft(styles.ToolCompletedIcon.GetMarginLeft()).Render(result)
		}
		return content
	}

	return toolcommon.RenderTool(msg, s, arg, result, width, sessionState.HideToolResults())
}

func formatShellOutput(output string, width int) string {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.ReplaceAll(output, "\r", "\n")
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return ""
	}

	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 10)
	lines := toolcommon.WrapLines(output, availableWidth)
	if len(lines) > maxVisibleShellOutputLines {
		lines = append([]string{"…"}, lines[len(lines)-maxVisibleShellOutputLines:]...)
	}
	return strings.Join(lines, "\n")
}
