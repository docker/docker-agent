package markdown

// Renderer is an interface for markdown renderers.
type Renderer interface {
	Render(input string) (string, error)
}

// NewRendererWithoutCopyIcon creates a markdown renderer that does not draw
// the per-code-block copy affordance. Use it for surfaces that never
// hit-test clicks on the icon, so no dead copy button is shown.
func NewRendererWithoutCopyIcon(width int) Renderer {
	return NewFastRenderer(width).HideCopyIcon()
}
