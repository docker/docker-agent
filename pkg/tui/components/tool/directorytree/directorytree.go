package directorytree

import (
	"strings"

	pathx "github.com/docker/docker-agent/pkg/path"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, toolcommon.SimpleRendererWithResult(
		toolcommon.ExtractField(func(a filesystem.DirectoryTreeArgs) string { return pathx.ShortenHome(a.Path) }),
		extractResult,
	))
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil {
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
