package sidebar

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// CollapsedViewModel holds the computed layout decisions for collapsed mode.
// This is a pure data structure - rendering is handled by separate view functions.
// Computing this once avoids duplicating the layout logic between CollapsedHeight and collapsedView.
type CollapsedViewModel struct {
	TitleWithStar    string
	WorkingIndicator string
	WorkingDir       string
	UsageSummary     string
	// InfoLine is the compact agents/tools/todos summary shown when the
	// sidebar renders as a horizontal band.
	InfoLine     string
	PlansSummary string

	// Layout decisions computed from the data
	TitleAndIndicatorOnOneLine bool
	WdAndUsageOnOneLine        bool
	ContentWidth               int
}

// LineCount returns the number of lines needed to render this layout.
func (vm CollapsedViewModel) LineCount() int {
	lines := 1 // divider
	lines += vm.titleSectionLines()

	// Path + usage metadata row. The two share one line only when both are
	// present and fit; a missing part (e.g. hidden session path or hidden
	// usage) is skipped so the row never renders blank.
	switch {
	case vm.WorkingDir != "" && vm.UsageSummary != "" && vm.WdAndUsageOnOneLine:
		lines++
	default:
		if vm.WorkingDir != "" {
			lines += linesNeeded(lipgloss.Width(vm.WorkingDir), vm.ContentWidth)
		}
		if vm.UsageSummary != "" {
			lines += linesNeeded(lipgloss.Width(vm.UsageSummary), vm.ContentWidth)
		}
	}

	if vm.InfoLine != "" {
		lines += linesNeeded(lipgloss.Width(vm.InfoLine), vm.ContentWidth)
	}
	if vm.PlansSummary != "" {
		lines += linesNeeded(lipgloss.Width(vm.PlansSummary), vm.ContentWidth)
	}

	return lines
}

// titleSectionLines returns the number of rendered lines consumed by the
// title (and optional working indicator) section.
func (vm CollapsedViewModel) titleSectionLines() int {
	switch {
	case vm.TitleAndIndicatorOnOneLine:
		return 1
	case vm.WorkingIndicator == "":
		return linesNeeded(lipgloss.Width(vm.TitleWithStar), vm.ContentWidth)
	default:
		return linesNeeded(lipgloss.Width(vm.TitleWithStar), vm.ContentWidth) +
			linesNeeded(lipgloss.Width(vm.WorkingIndicator), vm.ContentWidth)
	}
}

// RenderCollapsedView renders the collapsed sidebar from a CollapsedViewModel.
// This is a pure function that takes data and returns a string.
func RenderCollapsedView(vm CollapsedViewModel) string {
	var lines []string

	// Title line(s)
	switch {
	case vm.TitleAndIndicatorOnOneLine:
		if vm.WorkingIndicator == "" {
			lines = append(lines, vm.TitleWithStar)
		} else {
			gap := vm.ContentWidth - lipgloss.Width(vm.TitleWithStar) - lipgloss.Width(vm.WorkingIndicator)
			lines = append(lines, fmt.Sprintf("%s%*s%s", vm.TitleWithStar, gap, "", vm.WorkingIndicator))
		}
	case vm.WorkingIndicator == "":
		// No working indicator but title wraps - just output title (lipgloss will wrap)
		lines = append(lines, vm.TitleWithStar)
	default:
		// Title and working indicator on separate lines
		lines = append(lines, vm.TitleWithStar, vm.WorkingIndicator)
	}

	// Working directory + usage line(s). WorkingDir arrives pre-styled
	// (accent block + primary text) to match the vertical Session tab.
	// The two share one line only when both are present and fit; a missing
	// part (e.g. hidden session path or hidden usage) is skipped so the row
	// never renders blank. Mirrors LineCount.
	switch {
	case vm.WorkingDir != "" && vm.UsageSummary != "" && vm.WdAndUsageOnOneLine:
		gap := vm.ContentWidth - lipgloss.Width(vm.WorkingDir) - lipgloss.Width(vm.UsageSummary)
		lines = append(lines, fmt.Sprintf("%s%*s%s", vm.WorkingDir, gap, "", vm.UsageSummary))
	default:
		if vm.WorkingDir != "" {
			lines = append(lines, vm.WorkingDir)
		}
		if vm.UsageSummary != "" {
			lines = append(lines, vm.UsageSummary)
		}
	}

	if vm.InfoLine != "" {
		lines = append(lines, vm.InfoLine)
	}
	if vm.PlansSummary != "" {
		lines = append(lines, vm.PlansSummary)
	}

	return strings.Join(lines, "\n")
}

// linesNeeded calculates how many lines are needed to display text of given width
// within a container of contentWidth. Returns at least 1 line.
func linesNeeded(textWidth, contentWidth int) int {
	if contentWidth <= 0 || textWidth <= 0 {
		return 1
	}
	return max(1, (textWidth+contentWidth-1)/contentWidth)
}
