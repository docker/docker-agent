package dialog

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// permissionsDialog displays the configured tool permissions (allow/deny patterns).
type permissionsDialog struct {
	readOnlyScrollDialog

	permissions *runtime.PermissionsInfo
	yoloEnabled bool
}

// NewPermissionsDialog creates a new dialog showing tool permission rules.
func NewPermissionsDialog(perms *runtime.PermissionsInfo, yoloEnabled bool) Dialog {
	d := &permissionsDialog{
		permissions: perms,
		yoloEnabled: yoloEnabled,
	}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(
		readOnlyScrollDialogSize{widthPercent: 60, minWidth: 40, maxWidth: 70, heightPercent: 70, heightMax: 30},
		d.renderLines,
	)
	return d
}

func (d *permissionsDialog) renderLines(contentWidth, _ int) []string {
	lines := []string{
		RenderTitle("Tool Permissions", contentWidth, styles.DialogTitleStyle),
		RenderSeparator(contentWidth),
		"",
	}

	// Show yolo mode status
	lines = append(lines, d.renderYoloStatus(), "")

	if d.permissions == nil {
		lines = append(lines, styles.MutedStyle.Render("No permission patterns configured."), "")
	} else {
		if len(d.permissions.Deny) > 0 {
			lines = append(lines, d.renderSectionHeader("Deny", "Always blocked, even with yolo mode"), "")
			for _, pattern := range d.permissions.Deny {
				lines = append(lines, d.renderPattern(pattern, true))
			}
			lines = append(lines, "")
		}

		if len(d.permissions.Allow) > 0 {
			lines = append(lines, d.renderSectionHeader("Allow", "Auto-approved without confirmation"), "")
			for _, pattern := range d.permissions.Allow {
				lines = append(lines, d.renderPattern(pattern, false))
			}
			lines = append(lines, "")
		}

		if len(d.permissions.Ask) > 0 {
			lines = append(lines, d.renderSectionHeader("Ask", "Always requires confirmation, even for read-only tools"), "")
			for _, pattern := range d.permissions.Ask {
				lines = append(lines, d.renderAskPattern(pattern))
			}
			lines = append(lines, "")
		}

		if len(d.permissions.Allow) == 0 && len(d.permissions.Ask) == 0 && len(d.permissions.Deny) == 0 {
			lines = append(lines, styles.MutedStyle.Render("No permission patterns configured."), "")
		}
	}

	return lines
}

func (d *permissionsDialog) renderYoloStatus() string {
	label := lipgloss.NewStyle().Bold(true).Render("Yolo Mode: ")
	var status string
	if d.yoloEnabled {
		status = lipgloss.NewStyle().Foreground(styles.Success).Render("ON")
		status += styles.MutedStyle.Render(" (auto-approve unmatched tools)")
	} else {
		status = lipgloss.NewStyle().Foreground(styles.TextSecondary).Render("OFF")
		status += styles.MutedStyle.Render(" (ask for unmatched tools)")
	}
	return label + status
}

func (d *permissionsDialog) renderSectionHeader(title, description string) string {
	header := lipgloss.NewStyle().Bold(true).Foreground(styles.TextSecondary).Render(title)
	desc := styles.MutedStyle.Render(" - " + description)
	return header + desc
}

func (d *permissionsDialog) renderPattern(pattern string, isDeny bool) string {
	var icon string
	var style lipgloss.Style
	if isDeny {
		icon = "✗"
		style = lipgloss.NewStyle().Foreground(styles.Error)
	} else {
		icon = "✓"
		style = lipgloss.NewStyle().Foreground(styles.Success)
	}

	return style.Render(icon) + "  " + lipgloss.NewStyle().Foreground(styles.Highlight).Render(pattern)
}

func (d *permissionsDialog) renderAskPattern(pattern string) string {
	icon := "?"
	style := lipgloss.NewStyle().Foreground(styles.TextSecondary)
	return style.Render(icon) + "  " + lipgloss.NewStyle().Foreground(styles.Highlight).Render(pattern)
}
