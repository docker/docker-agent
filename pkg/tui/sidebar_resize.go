package tui

import (
	"image"

	"charm.land/lipgloss/v2"
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

func foregroundLayerBounds(layer *lipgloss.Layer) image.Rectangle {
	x, y := layer.GetX(), layer.GetY()
	return image.Rect(x, y, x+layer.Width(), y+layer.Height())
}
