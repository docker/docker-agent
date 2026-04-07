package globfiles

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/tools/builtin"
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
	parsed, err := toolcommon.ParseArgs[builtin.GlobFilesArgs](args)
	if err != nil {
		return ""
	}

	pattern := parsed.Pattern
	if len(pattern) > 40 {
		pattern = pattern[:37] + "..."
	}

	if parsed.Path != "" && parsed.Path != "." {
		return fmt.Sprintf("%s in %s", pattern, toolcommon.ShortenPath(parsed.Path))
	}
	return pattern
}

func extractResult(msg *types.Message) string {
	if msg.ToolResult == nil || msg.ToolResult.Meta == nil {
		return "no matches"
	}
	meta, ok := msg.ToolResult.Meta.(builtin.GlobFilesMeta)
	if !ok {
		return "no matches"
	}

	if meta.FileCount == 0 {
		return "no matches"
	}

	fileWord := "file"
	if meta.FileCount != 1 {
		fileWord = "files"
	}

	result := fmt.Sprintf("%d %s", meta.FileCount, fileWord)
	if meta.Truncated {
		result += " (truncated)"
	}
	return result
}
