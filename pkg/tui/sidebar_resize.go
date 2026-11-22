package tui

import (
	"image"

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

func foregroundBounds(layers []*lipgloss.Layer) []image.Rectangle {
	bounds := make([]image.Rectangle, 0, len(layers))
	for _, layer := range layers {
		x, y := layer.GetX(), layer.GetY()
		bounds = append(bounds, image.Rect(x, y, x+layer.Width(), y+layer.Height()))
	}
	return bounds
}

func (m *appModel) clearSidebarResizeGuide() {
	cache := &m.sidebarGuideCache
	if cache.guide.End > cache.guide.Start {
		cache.guide = chat.SidebarGuide{}
		cache.content = ""
		if m.imageWriter != nil {
			m.imageWriter.SetOcclusion(image.Rectangle{})
		}
	}
}

// withSidebarResizeGuide paints the guide over the chat rows and under the foreground.
func (m *appModel) withSidebarResizeGuide(view tea.View) tea.View {
	guide, ok := chat.SidebarResizeGuide(m.activeTab.chatPage)
	if !ok || guide.Start >= guide.End {
		m.clearSidebarResizeGuide()
		return view
	}
	cache := &m.sidebarGuideCache
	if cache.content == "" || cache.guide != guide {
		glyph := resizegrip.RenderGuide(guide.Opacity)
		if m.imageWriter != nil && (cache.guide.X != guide.X || cache.guide.Start != guide.Start || cache.guide.End != guide.End) {
			m.imageWriter.SetOcclusion(image.Rect(guide.X, guide.Start, guide.X+1, guide.End))
		}
		cache.guide = guide
		if len(m.foreground.layers) == 0 {
			cache.content = ansiline.PaintColumn(view.Content, glyph, guide.X, guide.Start, guide.End)
		} else {
			base := ansiline.PaintColumn(m.foreground.base, glyph, guide.X, guide.Start, guide.End)
			cache.content = m.foreground.compose(base)
		}
	}
	view.Content = cache.content
	return view
}
