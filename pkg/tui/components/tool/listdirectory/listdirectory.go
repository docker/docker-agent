package listdirectory

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

const maxVisibleEntries = 10

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	extractPath := toolcommon.ExtractField(func(a filesystem.ListDirectoryArgs) string { return pathx.ShortenHome(a.Path) })
	renderPath := toolcommon.SimpleRenderer(extractPath)
	return toolcommon.NewBaseWithCollapsed(
		msg,
		sessionState,
		render,
		toolcommon.CollapsedRenderer(renderPath),
	)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	path := toolcommon.ExtractField(func(a filesystem.ListDirectoryArgs) string { return pathx.ShortenHome(a.Path) })(msg.ToolCall.Function.Arguments)
	header := toolcommon.RenderTool(msg, s, path, extractResult(msg), width, sessionState.HideToolResults())
	if sessionState.HideToolResults() {
		return header
	}

	details := renderEntries(msg)
	if details == "" {
		return header
	}
	return header + "\n" + styles.ToolCallResult.Render(details)
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil || msg.ToolResult.Meta == nil {
		return "empty directory"
	}
	meta, ok := msg.ToolResult.Meta.(filesystem.ListDirectoryMeta)
	if !ok {
		return "empty directory"
	}

	fileCount := len(meta.Files)
	dirCount := len(meta.Dirs)
	if fileCount+dirCount == 0 {
		return "empty directory"
	}

	var parts []string
	if fileCount > 0 {
		parts = append(parts, toolcommon.Pluralize(fileCount, "file", "files"))
	}
	if dirCount > 0 {
		parts = append(parts, toolcommon.Pluralize(dirCount, "directory", "directories"))
	}

	result := strings.Join(parts, " and ")
	if meta.Truncated {
		result += " (truncated)"
	}
	return result
}

func renderEntries(msg *types.Message) string {
	if msg.ToolResult == nil || msg.ToolResult.Meta == nil {
		return ""
	}
	meta, ok := msg.ToolResult.Meta.(filesystem.ListDirectoryMeta)
	if !ok {
		return ""
	}

	entries := make([]string, 0, len(meta.Dirs)+len(meta.Files)+1)
	for _, dir := range meta.Dirs {
		entries = append(entries, "DIR  "+dir)
	}
	for _, file := range meta.Files {
		entries = append(entries, "FILE "+file)
	}
	if len(entries) == 0 {
		return ""
	}
	if len(entries) > maxVisibleEntries {
		entries = append(entries[:maxVisibleEntries], "…")
	}
	return strings.Join(entries, "\n")
}
