package readmultiplefiles

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(msg, sessionState, render)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	// Parse arguments
	var args filesystem.ReadMultipleFilesArgs
	if err := json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args); err != nil {
		return toolcommon.RenderTool(msg, s, "", "", width, sessionState.HideToolResults())
	}

	// For pending/running state, show files being read
	if msg.ToolStatus == types.ToolStatusPending || msg.ToolStatus == types.ToolStatusRunning {
		return toolcommon.RenderTool(msg, s, formatFilesList(args.Paths), "", width, sessionState.HideToolResults())
	}

	// For completed/error state, render each file line
	var meta *filesystem.ReadMultipleFilesMeta
	if msg.ToolResult != nil {
		if m, ok := msg.ToolResult.Meta.(filesystem.ReadMultipleFilesMeta); ok {
			meta = &m
		}
	}

	// Each file on its own line with checkmark
	var content strings.Builder
	for _, summary := range formatSummaryLines(meta) {
		if content.Len() > 0 {
			content.WriteString("\n")
		}

		// Icon / Tool name / File path
		nameStyle := styles.ToolName
		icon := toolcommon.Icon(msg, s)
		if summary.isError {
			nameStyle = styles.ToolNameError
			icon = toolcommon.Icon(&types.Message{ToolStatus: types.ToolStatusError}, s)
		}
		readCall := icon + nameStyle.Render("Read")
		if summary.path != "" {
			readCall += " " + summary.path
		}

		// Output aligned to the right using lipgloss
		outputStyle := styles.ToolMessageStyle
		if summary.isError {
			outputStyle = styles.ToolErrorMessageStyle
		}
		remainingWidth := max(width-lipgloss.Width(readCall)-1, 1)
		renderedOutput := outputStyle.Render(summary.output)
		if lipgloss.Width(renderedOutput) > remainingWidth {
			// Truncate output to fit
			renderedOutput = outputStyle.Render(toolcommon.TruncateText(summary.output, remainingWidth))
		}
		output := renderedOutput

		content.WriteString(readCall)
		content.WriteString(" ")
		content.WriteString(output)
	}

	return styles.RenderComposite(styles.ToolMessageStyle.Width(width), content.String())
}

type fileSummary struct {
	path    string
	output  string
	isError bool
}

// formatSummaryLines creates a summary for each file from metadata.
func formatSummaryLines(meta *filesystem.ReadMultipleFilesMeta) []fileSummary {
	if meta == nil || len(meta.Files) == 0 {
		return nil
	}

	var summaries []fileSummary
	for _, file := range meta.Files {
		path := toolcommon.ShortenPath(file.Path)
		var output string
		if file.Error != "" {
			output = " " + file.Error
		} else {
			output = fmt.Sprintf(" %d lines", file.LineCount)
		}

		summaries = append(summaries, fileSummary{
			path:    path,
			output:  output,
			isError: file.Error != "",
		})
	}

	return summaries
}

// formatFilesList formats a list of file paths for display.
func formatFilesList(filePaths []string) string {
	if len(filePaths) == 0 {
		return ""
	}

	shortened := make([]string, len(filePaths))
	for i, p := range filePaths {
		shortened[i] = toolcommon.ShortenPath(p)
	}

	if len(shortened) == 1 {
		return shortened[0]
	}

	return strings.Join(shortened, ", ")
}
