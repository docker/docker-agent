package readfile

import (
	"fmt"
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
	extractPath := toolcommon.ExtractField(func(a filesystem.ReadFileArgs) string { return pathx.ShortenHome(a.Path) })
	renderPath := toolcommon.SimpleRenderer(extractPath)
	return toolcommon.NewBaseWithCollapsed(
		msg,
		sessionState,
		render,
		toolcommon.CollapsedRenderer(renderPath),
	)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	path := toolcommon.ExtractField(func(a filesystem.ReadFileArgs) string { return pathx.ShortenHome(a.Path) })(msg.ToolCall.Function.Arguments)
	header := toolcommon.RenderTool(msg, s, path, extractResult(msg), width, sessionState.HideToolResults())
	if sessionState.HideToolResults() || msg.ToolStatus == types.ToolStatusError {
		return header
	}

	preview := formatLastLines(msg.Content, width)
	if preview == "" {
		return header
	}
	return header + "\n" + styles.ToolCallResult.Render(preview)
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil || msg.ToolResult.Meta == nil {
		return ""
	}
	meta, ok := msg.ToolResult.Meta.(filesystem.ReadFileMeta)
	if !ok {
		return ""
	}
	if meta.Error != "" {
		return meta.Error
	}
	return fmt.Sprintf("%d lines", meta.LineCount)
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
