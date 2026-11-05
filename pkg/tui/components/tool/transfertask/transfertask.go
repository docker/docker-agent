package transfertask

import (
	"strings"

	"charm.land/lipgloss/v2"

	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(ar, msg, sessionState, render)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	params, err := toolcommon.ParseArgs[transfertask.Args](msg.ToolCall.Function.Arguments)
	if err != nil || (params.Agent == "" && params.Task == "") {
		hideResults := false
		if sessionState != nil {
			hideResults = sessionState.HideToolResults()
		}
		return toolcommon.RenderTool(msg, s, "", "", width, hideResults)
	}

	header := styles.AgentBadgeStyleFor(msg.Sender).MarginLeft(2).Render(msg.Sender) +
		" calls " +
		styles.AgentBadgeStyleFor(params.Agent).Render(params.Agent)

	// Status-aware icon: spinner while the delegation runs, ✓ on success, ✗ on error.
	// Single glyph (no elapsed suffix) keeps the wrap math below stable.
	icon := statusIcon(msg, s)
	iconWithSpace := icon + " "
	iconWidth := lipgloss.Width(iconWithSpace)

	// Calculate available width for task text (accounting for icon width)
	availableWidth := max(width-iconWidth, 10)

	// Wrap the task text to fit within the available width
	lines := toolcommon.WrapLines(params.Task, availableWidth)

	// Build the task content with proper indentation for wrapped lines
	var taskContent strings.Builder
	for i, line := range lines {
		if i == 0 {
			// First line: icon + text
			taskContent.WriteString(iconWithSpace)
			taskContent.WriteString(styles.ToolMessageStyle.Render(line))
		} else {
			// Subsequent lines: indent to align with first line's text
			taskContent.WriteString("\n")
			taskContent.WriteString(strings.Repeat(" ", iconWidth))
			taskContent.WriteString(styles.ToolMessageStyle.Render(line))
		}
	}

	return header + "\n\n" + taskContent.String()
}

// statusIcon picks the leading glyph for the delegation card from the tool
// status: an animated spinner while the sub-agent runs, ✓ on success, ✗ on
// error. The transfertask Base spinner is ModeSpinnerOnly, so s.View() is a
// single 1-cell glyph whose width matches ✓/✗, keeping the task-text wrap math
// stable.
func statusIcon(msg *types.Message, s spinner.Spinner) string {
	switch msg.ToolStatus {
	case types.ToolStatusRunning, types.ToolStatusPending, types.ToolStatusConfirmation:
		if msg.HideToolProgress {
			return styles.ToolMessageStyle.MarginLeft(2).Render("●")
		}
		return styles.NoStyle.MarginLeft(2).Render(s.View())
	case types.ToolStatusCompleted:
		return styles.ToolCompletedIcon.Render("✓")
	case types.ToolStatusError:
		return styles.ToolErrorIcon.Render("✗")
	default: // genuinely unknown/terminal states
		return styles.ToolCompletedIcon.Render("✓")
	}
}
