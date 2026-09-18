package ui

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// The style helpers are evaluated lazily (on each call) so they always reflect
// the theme that styles.ApplyTheme installed before the TUI started.

func StAccent() lipgloss.Style      { return lipgloss.NewStyle().Foreground(styles.Accent) }
func StMuted() lipgloss.Style       { return lipgloss.NewStyle().Foreground(styles.TextMutedGray) }
func StSecondary() lipgloss.Style   { return lipgloss.NewStyle().Foreground(styles.TextSecondary) }
func StPrimary() lipgloss.Style     { return lipgloss.NewStyle().Foreground(styles.TextPrimary) }
func StBold() lipgloss.Style        { return lipgloss.NewStyle().Foreground(styles.TextPrimary).Bold(true) }
func StError() lipgloss.Style       { return lipgloss.NewStyle().Foreground(styles.Error) }
func StWarning() lipgloss.Style     { return lipgloss.NewStyle().Foreground(styles.Warning) }
func StSuccess() lipgloss.Style     { return lipgloss.NewStyle().Foreground(styles.Success) }
func StPlaceholder() lipgloss.Style { return lipgloss.NewStyle().Foreground(styles.TextMuted) }
func StReasoning() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(styles.TextMutedGray).Italic(true)
}

func StUserBox(width int) lipgloss.Style {
	if width < 1 {
		width = 1
	}
	horizontalPadding := 1
	if width < 3 {
		horizontalPadding = 0
	}
	return lipgloss.NewStyle().
		Foreground(styles.AgentBadgeFg).
		Background(styles.MobyBlue).
		Padding(1, horizontalPadding).
		Width(width)
}

func StToolBox(width int) lipgloss.Style {
	if width < 1 {
		width = 1
	}
	horizontalPadding := 1
	if width < 4 {
		horizontalPadding = 0
	}
	return lipgloss.NewStyle().
		Foreground(styles.TextMutedGray).
		Background(styles.BackgroundAlt).
		Padding(1, horizontalPadding).
		Width(width)
}

const (
	PromptText   = "❯ "
	PromptWidth  = 2
	Continuation = "  "
)
