package editfile

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type ToggleDiffViewMsg struct{}

// New creates the edit_file tool UI model.
func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, render)
}

// render displays the edit_file tool output in the TUI.
func render(
	msg *types.Message,
	s spinner.Spinner,
	sessionState service.SessionStateReader,
	width,
	_ int,
) string {
	// Parse tool arguments to extract the file path for display.
	args, err := filesystem.ParseEditFileArgs([]byte(msg.ToolCall.Function.Arguments))
	if err != nil {
		return ""
	}

	if msg.ToolStatus == types.ToolStatusError {
		if msg.Content == "" {
			return ""
		}

		line := fmt.Sprintf(
			"%s%s %s",
			toolcommon.Icon(msg, s),
			styles.ToolNameError.Render(msg.ToolDefinition.DisplayName()),
			styles.ToolErrorMessageStyle.Render(msg.Content),
		)

		return styles.BaseStyle.
			MaxWidth(width).
			Render(line)
	}

	// Check for friendly description first
	var content string
	if header, ok := toolcommon.RenderFriendlyHeader(msg, s); ok {
		content = header
	} else {
		content = fmt.Sprintf(
			"%s%s %s",
			toolcommon.Icon(msg, s),
			styles.ToolName.Render(msg.ToolDefinition.DisplayName()),
			styles.ToolMessageStyle.Render(toolcommon.ShortenPath(args.Path)),
		)
	}

	if sessionState.HideToolResults() {
		return content
	}

	if msg.ToolCall.Function.Arguments != "" {
		contentWidth := width - styles.ToolCallResult.GetHorizontalFrameSize()

		content += "\n" + styles.ToolCallResult.Render(
			renderEditFile(
				msg.ToolCall,
				contentWidth,
				sessionState.SplitDiffView(),
				msg.ToolStatus,
			),
		)
	}

	return content
}
