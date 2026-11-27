package chat

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func (p *chatPage) bannerProgress() float64 {
	if p.bannerTransition.Running() {
		return p.bannerTransition.Progress()
	}
	return 1
}

func (p *chatPage) cancelBanner() {
	p.bannerTransition.Cancel()
	p.bannerMotion.Stop()
	p.bannerFlock = banner.Flock{}
}

// LeaveBanner releases hover when a dialog or another app region owns the pointer.
func LeaveBanner(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		return p.leaveBanner()
	}
	return nil
}

func (p *chatPage) leaveBanner() tea.Cmd {
	if p.bannerFlock.Leave() {
		return p.bannerMotion.Start()
	}
	return nil
}

// ResetBanner clears motion when the page loses pointer access.
func ResetBanner(page Page) {
	if p, ok := page.(*chatPage); ok {
		p.bannerMotion.Stop()
		p.bannerFlock = banner.Flock{}
	}
}

func (p *chatPage) pointBanner(x, y int, click bool) (bool, tea.Cmd) {
	sl := p.computeSidebarLayout()
	chatY := 0
	if sl.mode != sidebarVertical && !sl.bandAtBottom && !p.sidebarHidden() {
		chatY = sl.sidebarHeight
	}
	x -= styles.AppPadding + sl.chatStartX
	y -= chatY + (sl.chatHeight-banner.Height)/2
	left := (sl.chatWidth - banner.Width) / 2
	right := left + banner.Width
	if p.bannerTransition.Running() {
		left, right = 0, sl.chatWidth
	}
	if !p.bannerInteractive || !p.bannerVisible(sl) || x < left || x >= right || y < 0 || y >= banner.Height {
		return false, p.leaveBanner()
	}
	p.bannerFlock.Point(float64(x), float64(y), click, p.bannerProgress(), sl.chatWidth)
	return true, p.bannerMotion.Start()
}
