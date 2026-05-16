package tab

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func Render(title, content string, width int) string {
	styleTitle := styles.TabTitleStyle
	styleBody := styles.TabStyle

	return styles.NoStyle.PaddingBottom(1).Render(
		lipgloss.JoinVertical(lipgloss.Top,
			lipgloss.PlaceHorizontal(width, lipgloss.Left,
				styleTitle.PaddingRight(1).Render(title),
				lipgloss.WithWhitespaceChars("─"),
				lipgloss.WithWhitespaceStyle(styleTitle),
			),
			styles.RenderComposite(styleBody.Width(width), content),
		),
	)
}
