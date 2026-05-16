package sidebar

import "github.com/docker/docker-agent/pkg/tui/components/scrollbar"

// Layout constants for sidebar elements.
const (
	// treePrefixWidth is the width of tree-structure prefixes like "├ " and "└ ".
	treePrefixWidth = 2

	// starClickWidth is the clickable area width for the star indicator.
	starClickWidth = 2

	// verticalStarY is the Y position of the star in vertical mode.
	// Line 0: tab title, Line 1: TabStyle top padding, Line 2: star + title.
	verticalStarY = 2

	// minGap is the minimum gap between elements when laying out side-by-side.
	minGap = 2

	// DefaultWidth is the default sidebar width in vertical mode.
	DefaultWidth = 40

	// MinWidth is the minimum sidebar width before auto-collapsing.
	MinWidth = 20

	// MaxWidthPercent is the maximum sidebar width as a percentage of window.
	MaxWidthPercent = 0.5
)

// LayoutConfig defines the spacing and sizing parameters for the sidebar.
// All values are in terminal columns.
//
// Layout diagram for vertical mode:
//
//	|<-- outerWidth (m.width) ----------------------->|
//	|<-PL->|<-- contentWidth -->|<-SG->|<-SW->|
//	|      |   Tab Title────────|      |  │   |
//	|      |   Content here     |      |  │   |
//	|      |                    |      |  │   |
//
// PL = PaddingLeft, SG = ScrollbarGap, SW = scrollbar.Width
type LayoutConfig struct {
	// PaddingLeft is the space between the left edge of the sidebar and content.
	// This replaces the external wrapper padding that was previously in chat.go.
	PaddingLeft int
	// PaddingRight is the space between the content and the right edge (when no scrollbar).
	PaddingRight int
	// ScrollbarGap is the space between content and the scrollbar when visible.
	ScrollbarGap int
}

// DefaultLayoutConfig returns the default sidebar layout configuration.
func DefaultLayoutConfig() LayoutConfig {
	return LayoutConfig{
		PaddingLeft:  1,
		PaddingRight: 0,
		ScrollbarGap: 1,
	}
}

// Metrics holds the computed layout metrics for a given render pass.
type Metrics struct {
	// OuterWidth is the total width allocated to the sidebar.
	OuterWidth int
	// ContentWidth is the width available for actual content (tabs, text, etc.).
	ContentWidth int
	// ScrollbarVisible indicates whether the scrollbar will be rendered.
	ScrollbarVisible bool
	// config is the layout config used to compute these metrics.
	config LayoutConfig
}

// Compute calculates the layout metrics for the given outer width and scrollbar visibility.
func (cfg LayoutConfig) Compute(outerWidth int, scrollbarVisible bool) Metrics {
	contentWidth := outerWidth - cfg.PaddingLeft - cfg.PaddingRight
	if scrollbarVisible {
		contentWidth -= scrollbar.Width + cfg.ScrollbarGap
	}
	if contentWidth < 1 {
		contentWidth = 1
	}
	return Metrics{
		OuterWidth:       outerWidth,
		ContentWidth:     contentWidth,
		ScrollbarVisible: scrollbarVisible,
		config:           cfg,
	}
}

// PaddingLeft returns the left padding from the config.
func (m Metrics) PaddingLeft() int {
	return m.config.PaddingLeft
}

// PaddingRight returns the right padding from the config.
func (m Metrics) PaddingRight() int {
	return m.config.PaddingRight
}

// ScrollbarGap returns the scrollbar gap from the config.
func (m Metrics) ScrollbarGap() int {
	return m.config.ScrollbarGap
}
