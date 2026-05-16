package searchfilescontent

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, toolcommon.SimpleRendererWithResult(
		extractArgs,
		extractResult,
	))
}

func extractArgs(args string) string {
	parsed, err := toolcommon.ParseArgs[filesystem.SearchFilesContentArgs](args)
	if err != nil {
		return ""
	}

	path := toolcommon.ShortenPath(parsed.Path)
	query := parsed.Query
	if len(query) > 30 {
		query = query[:27] + "..."
	}

	if parsed.IsRegex {
		return fmt.Sprintf("%s (regex: %s)", path, query)
	}
	return fmt.Sprintf("%s (%s)", path, query)
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil || msg.ToolResult.Meta == nil {
		return "no matches"
	}
	meta, ok := msg.ToolResult.Meta.(filesystem.SearchFilesContentMeta)
	if !ok {
		return "no matches"
	}

	if meta.MatchCount == 0 {
		return "no matches"
	}

	return fmt.Sprintf("%s in %s",
		toolcommon.Pluralize(meta.MatchCount, "match", "matches"),
		toolcommon.Pluralize(meta.FileCount, "file", "files"),
	)
}
