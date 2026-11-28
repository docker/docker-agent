package tui

import (
	"image"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
)

type viewForeground struct {
	layers     []*lipgloss.Layer
	helpBounds image.Rectangle
}

func (f viewForeground) compose(base string) string {
	layers := make([]*lipgloss.Layer, 1, 1+len(f.layers))
	layers[0] = lipgloss.NewLayer(base)
	return lipgloss.NewCompositor(append(layers, f.layers...)...).Render()
}

func foregroundBounds(layers []*lipgloss.Layer) []image.Rectangle {
	bounds := make([]image.Rectangle, 0, len(layers))
	for _, layer := range layers {
		bounds = append(bounds, foregroundLayerBounds(layer))
	}
	return bounds
}

func imageLayers(layers []*lipgloss.Layer) []tuiimage.ForegroundLayer {
	out := make([]tuiimage.ForegroundLayer, len(layers))
	for i, layer := range layers {
		out[i].Bounds = foregroundLayerBounds(layer)
		if layer.GetID() == dialog.AttachmentPreviewLayerID {
			out[i].ImageContent = layer.GetContent()
		}
	}
	return out
}

func foregroundLayerBounds(layer *lipgloss.Layer) image.Rectangle {
	x, y := layer.GetX(), layer.GetY()
	return image.Rect(x, y, x+layer.Width(), y+layer.Height())
}
