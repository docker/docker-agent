package statusbar

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// StatusBar displays key-binding help on the left and version info on the right.
// When the tab bar is hidden (single tab), it also shows a clickable "+ new tab" button.
type StatusBar struct {
	width int
	help  core.KeyMapHelp
	title string

	showNewTab   bool
	newTabStartX int
	newTabEndX   int

	cached     string
	cacheDirty bool
}

// Option is a functional option for configuring a StatusBar.
type Option func(*StatusBar)

// WithTitle sets a custom title for the status bar.
//
// If not provided, defaults to "docker agent".
func WithTitle(title string) Option {
	return func(s *StatusBar) {
		s.title = title
	}
}

// New creates a new StatusBar instance
func New(help core.KeyMapHelp, opts ...Option) StatusBar {
	s := StatusBar{
		help:       help,
		title:      "docker agent",
		cacheDirty: true,
	}

	for _, opt := range opts {
		opt(&s)
	}

	return s
}

// SetWidth sets the width of the status bar
func (s *StatusBar) SetWidth(width int) {
	if s.width != width {
		s.width = width
		s.cacheDirty = true
	}
}

// SetHelp sets the help provider for the status bar
func (s *StatusBar) SetHelp(help core.KeyMapHelp) {
	s.help = help
	s.cacheDirty = true
}

// SetShowNewTab controls whether the "+" button is shown.
func (s *StatusBar) SetShowNewTab(show bool) {
	if s.showNewTab != show {
		s.showNewTab = show
		s.cacheDirty = true
	}
}

// ClickedNewTab returns true if the given X coordinate hits the "+" button.
func (s *StatusBar) ClickedNewTab(x int) bool {
	return s.showNewTab && x >= s.newTabStartX && x < s.newTabEndX
}

// Height returns the rendered height of the status bar (always 1).
func (s *StatusBar) Height() int {
	return 1
}

// InvalidateCache clears all cached values.
func (s *StatusBar) InvalidateCache() {
	s.cacheDirty = true
}

// rebuild renders the full status bar line and computes click hitboxes.
func (s *StatusBar) rebuild() {
	s.cacheDirty = false
	s.newTabStartX = 0
	s.newTabEndX = 0

	// Build the styled right side: optional new-tab button + title.
	var rightW, newTabW int
	right := styles.MutedStyle.Render(s.title)

	if s.showNewTab {
		newTab := styles.MutedStyle.Render(" \u2502 ") +
			styles.HighlightWhiteStyle.Render("+") +
			styles.SecondaryStyle.Render(" new tab")
		newTabW = lipgloss.Width(newTab)
		right = newTab + "  " + right
		rightW = lipgloss.Width(right)
	} else {
		rightW = lipgloss.Width(right)
	}

	// Build the styled left side: help bindings (possibly truncated).
	const pad = 1
	maxHelpW := s.width - rightW - 2*pad - 1

	var left string
	var leftW int
	if s.help != nil {
		if help := s.help.Help(); help != nil {
			var parts []string
			for _, b := range help.ShortHelp() {
				if b.Help().Key != "" && b.Help().Desc != "" {
					parts = append(parts,
						styles.HighlightWhiteStyle.Render(b.Help().Key)+
							" "+
							styles.SecondaryStyle.Render(b.Help().Desc))
				}
			}
			if len(parts) > 0 && maxHelpW > 0 {
				helpStr := strings.Join(parts, "  ")
				helpW := lipgloss.Width(helpStr)
				if helpW > maxHelpW {
					helpStr = ansi.Truncate(helpStr, maxHelpW, "...")
					helpW = lipgloss.Width(helpStr)
				}
				left = " " + helpStr
				leftW = pad + helpW
			}
		}
	}

	gap := max(1, s.width-leftW-rightW-pad)

	if s.showNewTab {
		s.newTabStartX = leftW + gap
		s.newTabEndX = s.newTabStartX + newTabW
	}

	s.cached = left + strings.Repeat(" ", gap) + right + " "
}

// View renders the status bar.
//
// Layout: [ help text ...           (+ new tab)  docker agent VERSION ]
func (s *StatusBar) View() string {
	if s.cacheDirty {
		s.rebuild()
	}
	return s.cached
}
