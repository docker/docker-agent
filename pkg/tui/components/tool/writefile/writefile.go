package writefile

import (
	"strings"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const maxPreviewLines = 10

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	renderPath := toolcommon.SimpleRenderer(
		toolcommon.ExtractField(func(a filesystem.WriteFileArgs) string { return pathx.ShortenHome(a.Path) }),
	)
	return toolcommon.NewBaseWithCollapsed(msg, sessionState, render, toolcommon.CollapsedRenderer(renderPath))
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	args, err := toolcommon.ParseArgs[filesystem.WriteFileArgs](msg.ToolCall.Function.Arguments)
	if err != nil {
		return toolcommon.RenderTool(msg, s, "", "", width, sessionState.HideToolResults())
	}

	result := ""
	if msg.ToolStatus == types.ToolStatusCompleted || msg.ToolStatus == types.ToolStatusError {
		result = msg.Content
	}
	header := toolcommon.RenderTool(msg, s, pathx.ShortenHome(args.Path), result, width, sessionState.HideToolResults())
	if sessionState.HideToolResults() || msg.ToolStatus == types.ToolStatusError {
		return header
	}

	preview := formatLastLines(args.Content, width)
	if preview == "" {
		return header
	}
	return header + "\n" + styles.ToolCallResult.Render(preview)
}

func formatLastLines(content string, width int) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	truncated := len(lines) > maxPreviewLines
	if truncated {
		lines = lines[len(lines)-maxPreviewLines:]
	}

	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 10)
	wrapped := make([]string, 0, len(lines)+1)
	if truncated {
		wrapped = append(wrapped, "…")
	}
	for _, line := range lines {
		wrapped = append(wrapped, toolcommon.WrapLines(line, availableWidth)...)
	}
	return strings.Join(wrapped, "\n")
}
