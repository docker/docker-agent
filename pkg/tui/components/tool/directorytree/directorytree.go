package directorytree

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

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	extractPath := toolcommon.ExtractField(func(a filesystem.DirectoryTreeArgs) string { return pathx.ShortenHome(a.Path) })
	renderPath := toolcommon.SimpleRenderer(extractPath)
	return toolcommon.NewBaseWithCollapsed(
		msg,
		sessionState,
		render,
		toolcommon.CollapsedRenderer(renderPath),
	)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	path := toolcommon.ExtractField(func(a filesystem.DirectoryTreeArgs) string { return pathx.ShortenHome(a.Path) })(msg.ToolCall.Function.Arguments)
	header := toolcommon.RenderTool(msg, s, path, extractResult(msg), width, sessionState.HideToolResults())
	if sessionState.HideToolResults() || msg.Content == "" {
		return header
	}
	return header + "\n" + styles.ToolCallResult.Render(toolcommon.FormatToolResult(msg.Content, width))
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil || msg.ToolResult.Meta == nil {
		return ""
	}
	meta, ok := msg.ToolResult.Meta.(filesystem.DirectoryTreeMeta)
	if !ok {
		return ""
	}

	if meta.FileCount+meta.DirCount == 0 {
		return "empty"
	}

	var parts []string
	if meta.FileCount > 0 {
		parts = append(parts, toolcommon.Pluralize(meta.FileCount, "file", "files"))
	}
	if meta.DirCount > 0 {
		parts = append(parts, toolcommon.Pluralize(meta.DirCount, "dir", "dirs"))
	}

	result := strings.Join(parts, ", ")
	if meta.Truncated {
		result += " (truncated)"
	}
	return result
}
