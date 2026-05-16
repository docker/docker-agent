package listdirectory

import (
	"strings"

	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, toolcommon.SimpleRendererWithResult(
		toolcommon.ExtractField(func(a filesystem.ListDirectoryArgs) string { return toolcommon.ShortenPath(a.Path) }),
		extractResult,
	))
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
