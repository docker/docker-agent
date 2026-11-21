package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/internal/ansiline"
	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// viewForeground retains the last base frame and its foreground layers when
// there are overlays. Guide frames can then reuse the exact same stacking
// without asking any component to render again.
type viewForeground struct {
	base   string
	layers []*lipgloss.Layer
}

func (f viewForeground) compose(base string) string {
	layers := make([]*lipgloss.Layer, 1, 1+len(f.layers))
	layers[0] = lipgloss.NewLayer(base)
	return lipgloss.NewCompositor(append(layers, f.layers...)...).Render()
}

// withSidebarResizeGuide paints the guide over the chat rows and under the foreground.
func (m *appModel) withSidebarResizeGuide(view tea.View) tea.View {
	guide, ok := chat.SidebarResizeGuide(m.activeTab.chatPage)
	if !ok || guide.Start >= guide.End {
		m.sidebarGuideCache.content = ""
		return view
	}
	cache := &m.sidebarGuideCache
	if cache.content == "" || cache.guide != guide {
		glyph := resizegrip.RenderGuide(guide.Opacity)
		cache.guide = guide
		if len(m.foreground.layers) == 0 {
			cache.content = ansiline.PaintColumn(view.Content, glyph, guide.X, guide.Start, guide.End)
		} else {
			base := ansiline.PaintColumn(m.foreground.base, glyph, guide.X, guide.Start, guide.End)
			cache.content = m.prepareFrame(m.foreground.compose(base))
		}
	}
	view.Content = cache.content
	return view
}
