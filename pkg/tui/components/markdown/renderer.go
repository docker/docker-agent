package markdown

// Renderer is an interface for markdown renderers.
type Renderer interface {
	Render(input string) (string, error)
}

// NewRenderer creates a new markdown renderer with the given width.
func NewRenderer(width int) Renderer {
	return NewFastRenderer(width)
}
