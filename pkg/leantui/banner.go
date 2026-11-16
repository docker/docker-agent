package leantui

import (
	"github.com/docker/docker-agent/pkg/leantui/ui"
	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
)

const (
	bannerTopPadding  = 1
	bannerLeftPadding = 2
)

var bannerLines = tuibanner.Lines

func renderBanner(custom []string) []string {
	if len(custom) == 0 {
		return tuibanner.Render()
	}
	lines := make([]string, len(custom))
	for i, line := range custom {
		lines[i] = ui.StAccent().Render(line)
	}
	return lines
}
