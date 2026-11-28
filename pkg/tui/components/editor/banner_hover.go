package editor

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func (b *attachmentBanner) renderPill(item bannerItem) string {
	return renderHoveredAttachmentPill(item, b.hoverFade.Fraction(item.placeholder))
}

func (b *attachmentBanner) Hover(x, y int) tea.Cmd {
	var targets []string
	if b.ToggleAt(x, y) {
		targets = append(targets, "count")
	}
	if item, hasItem := b.HitTestPosition(x, y); hasItem {
		targets = append(targets, item.placeholder)
	}
	return b.hoverFade.Set(targets...)
}

func (b *attachmentBanner) Tick(tick animation.TickMsg) {
	if b.hoverFade.Tick(tick) {
		b.reflow()
	}
}

func (b *attachmentBanner) ClearHover() {
	if b.hoverFade.Clear() {
		b.reflow()
	}
}
