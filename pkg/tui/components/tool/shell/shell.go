package shell

import (
	"strings"

	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const maxVisibleShellOutputLines = 20

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBaseWithCollapsed(msg, sessionState, renderShell, renderShellCollapsed)
}

func renderShell(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	result := ""
	if msg.Content != "" {
		result = formatShellOutput(msg.Content, width)
	}

	return toolcommon.RenderTool(msg, s, extractCommand(msg.ToolCall.Function.Arguments), result, width, sessionState.HideToolResults())
}

func renderShellCollapsed(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	return toolcommon.RenderTool(msg, s, extractCommand(msg.ToolCall.Function.Arguments), "", width, sessionState.HideToolResults())
}

func extractCommand(arguments string) string {
	if arguments == "" {
		return ""
	}
	return toolcommon.ExtractField(func(a builtinshell.RunShellArgs) string { return a.Cmd })(arguments)
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
