package searchfilescontent

import (
	"fmt"

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
	renderArgs := toolcommon.SimpleRenderer(extractArgs)
	return toolcommon.NewBaseWithCollapsed(
		msg,
		sessionState,
		render,
		toolcommon.CollapsedRenderer(renderArgs),
	)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	header := toolcommon.RenderTool(msg, s, extractArgs(msg.ToolCall.Function.Arguments), extractResult(msg), width, sessionState.HideToolResults())
	if sessionState.HideToolResults() || msg.Content == "" {
		return header
	}
	return header + "\n" + styles.ToolCallResult.Render(toolcommon.FormatToolResult(msg.Content, width))
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
