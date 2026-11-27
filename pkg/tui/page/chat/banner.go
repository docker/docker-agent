package chat

import (
	"image"

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

func (p *chatPage) bannerOrigin(sl sidebarLayout) image.Point {
	chatY := 0
	if sl.mode != sidebarVertical && !sl.bandAtBottom && !p.sidebarHidden() {
		chatY = sl.sidebarHeight
	}
	return image.Pt(styles.AppPadding+sl.chatStartX, chatY+(sl.chatHeight-banner.Height)/2)
}

// PointBanner observes viewport pointer input without capturing it.
func PointBanner(page Page, x, y int, click bool) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		return p.pointBanner(x, y, click)
	}
	return nil
}

// CompositeBanner paints particles in terminal coordinates above the base UI.
func CompositeBanner(page Page, base string, width, height, top int, masks []image.Rectangle) string {
	p, ok := page.(*chatPage)
	if !ok || !p.bannerFlock.Displaced() {
		return base
	}
	sl := p.computeSidebarLayout()
	origin := p.bannerOrigin(sl).Add(image.Pt(0, top))
	return p.bannerFlock.Composite(base, p.bannerProgress(), sl.chatWidth, origin, image.Rect(0, 0, width, height), masks)
}

func (p *chatPage) pointBanner(x, y int, click bool) tea.Cmd {
	sl := p.computeSidebarLayout()
	origin := p.bannerOrigin(sl)
	x -= origin.X
	y -= origin.Y
	left := (sl.chatWidth - banner.Width) / 2
	right := left + banner.Width
	if p.bannerTransition.Running() {
		left, right = 0, sl.chatWidth
	}
	inside := x >= left && x < right && y >= 0 && y < banner.Height
	if !p.bannerInteractive || !p.bannerVisible(sl) || (!inside && !p.bannerFlock.Near(float64(x), float64(y), p.bannerProgress(), sl.chatWidth)) {
		return p.leaveBanner()
	}
	p.bannerFlock.Point(float64(x), float64(y), click, p.bannerProgress(), sl.chatWidth)
	return p.bannerMotion.Start()
}
