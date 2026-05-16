package todotool

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func renderTodoIcon(status string) (string, lipgloss.Style) {
	switch status {
	case "pending":
		return "◯", styles.ToBeDoneStyle
	case "in-progress":
		return "◔", styles.InProgressStyle
	case "completed":
		return "✓", styles.CompletedStyle.Strikethrough(true)
	default:
		return "?", styles.ToBeDoneStyle
	}
}
