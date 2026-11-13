package searchfilescontent

import (
	"fmt"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, toolcommon.SimpleRendererWithResult(
		toolcommon.StableExtractor(extractArgs),
		extractResult,
	))
}

func extractArgs(args string) string {
	parsed, err := toolcommon.ParseArgs[filesystem.SearchFilesContentArgs](args)
	if err != nil {
		return ""
	}

	path := pathx.ShortenHome(parsed.Path)
	query := parsed.Query
	if r := []rune(query); len(r) > 30 {
		query = string(r[:27]) + "..."
	}

	if parsed.IsRegex {
		return fmt.Sprintf("%s (regex: %s)", path, query)
	}
	return fmt.Sprintf("%s (%s)", path, query)
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil {
		return "no matches"
	}
	meta, ok := tools.DecodeResultMeta[filesystem.SearchFilesContentMeta](msg.ToolResult.Meta)
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
