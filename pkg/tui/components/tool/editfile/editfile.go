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
	return toolcommon.NewBaseWithCollapsed(msg, sessionState, render, renderCollapsed)
}

// render displays the edit_file tool output in the TUI.
// It prioritizes the agent-provided friendly header when available,
// hides results when collapsed by the user, and renders tool errors
// in a single-line error style consistent with other tools.
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
		// If arguments cannot be parsed, fail silently to avoid breaking the TUI.
		return ""
	}

	// When the tool failed, render a single-line error header
	// consistent with other tool error renderings.
	if msg.ToolStatus == types.ToolStatusError {
		if msg.Content == "" {
			return ""
		}

		// Render everything on a single line:
		// - error icon
		// - tool name in error style
		// - rejection/error message
		line := fmt.Sprintf(
			"%s%s %s",
			toolcommon.Icon(msg, s),
			styles.ToolNameError.Render(msg.ToolDefinition.DisplayName()),
			styles.ToolErrorMessageStyle.Render(msg.Content),
		)

		// Truncate to terminal width to avoid wrapping
		return styles.BaseStyle.
			MaxWidth(width).
			Render(line)
	}

	// ---- Normal (non-error) rendering ----

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

	// Tool results are hidden when the user collapses them.
	if sessionState.HideToolResults() {
		return content
	}

	// Successful (or pending/confirmation) execution:
	// render the diff output inside the ToolCallResult container.
	if msg.ToolCall.Function.Arguments != "" {
		// Calculate available width for diff rendering, accounting for
		// ToolCallResult frame padding.
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

// renderCollapsed renders a simplified view for collapsed reasoning blocks.
// Shows only the file path and +N / -M line counts.
func renderCollapsed(
	msg *types.Message,
	s spinner.Spinner,
	_ service.SessionStateReader,
	width,
	_ int,
) string {
	args, err := filesystem.ParseEditFileArgs([]byte(msg.ToolCall.Function.Arguments))
	if err != nil {
		return ""
	}

	// Error state
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
		return styles.BaseStyle.MaxWidth(width).Render(line)
	}

	// Count added/removed lines
	added, removed := countDiffLines(msg.ToolCall, msg.ToolStatus)
	var diffSummary string
	if added > 0 || removed > 0 {
		addStr := styles.DiffAddStyle.Render(fmt.Sprintf("+%d", added))
		remStr := styles.DiffRemoveStyle.Render(fmt.Sprintf("-%d", removed))
		diffSummary = fmt.Sprintf(" %s / %s", addStr, remStr)
	}

	line := fmt.Sprintf(
		"%s%s %s%s",
		toolcommon.Icon(msg, s),
		styles.ToolName.Render(msg.ToolDefinition.DisplayName()),
		styles.ToolMessageStyle.Render(toolcommon.ShortenPath(args.Path)),
		diffSummary,
	)

	return styles.BaseStyle.MaxWidth(width).Render(line)
}
