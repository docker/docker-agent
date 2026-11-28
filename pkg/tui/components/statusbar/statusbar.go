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
	width  int
	help   core.KeyMapHelp
	title  string
	status string

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

// SetStatus adds persistent status before the usual key hints.
func (s *StatusBar) SetStatus(status string) {
	if s.status != status {
		s.status = status
		s.cacheDirty = true
	}
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
	if !s.showNewTab {
		return false
	}
	if s.cacheDirty {
		s.rebuild()
	}
	return x >= s.newTabStartX && x < s.newTabEndX
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

	if s.width <= 0 {
		s.cached = ""
		return
	}

	pad := min(1, (s.width-1)/2)
	available := s.width - 2*pad
	right := styles.MutedStyle.Render(s.title)
	statusW := lipgloss.Width(s.status)
	var newTabOffset, newTabW int
	if s.showNewTab {
		plus := styles.HighlightWhiteStyle.Render("+")
		button := plus + styles.SecondaryStyle.Render(" new tab")
		separator := styles.MutedStyle.Render(" │ ")
		newTabW = lipgloss.Width(button)
		full := separator + button + "  " + right
		// Drop secondary chrome before dropping the action on narrow terminals.
		budget := available
		if statusW > 0 {
			budget -= statusW + 1
		}
		switch {
		case lipgloss.Width(full) <= budget:
			right = full
			newTabOffset = lipgloss.Width(separator)
		case newTabW <= budget:
			right = button
		case budget > 0:
			right, newTabW = plus, 1
		default:
			right, newTabW = "", 0
		}
	} else {
		right = ansi.Truncate(right, available, "")
	}

	rightW := lipgloss.Width(right)
	maxLeftW := available - rightW - 1
	if statusW > 0 && statusW > maxLeftW {
		// Give the pause notice priority over help and title on narrow terminals.
		padding := strings.Repeat(" ", pad)
		s.cached = padding + ansi.Truncate(s.status, available, "") + padding
		return
	}

	var left string
	if statusW > 0 {
		left = s.status
		maxLeftW -= statusW + 2
	}
	if s.help != nil && maxLeftW > 0 {
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
			if len(parts) > 0 {
				hints := ansi.Truncate(strings.Join(parts, "  "), maxLeftW, "...")
				if left != "" && hints != "" {
					left += "  "
				}
				left += hints
			}
		}
	}
	if left != "" {
		left = " " + left
	}
	leftW := lipgloss.Width(left)

	gap := max(0, s.width-leftW-rightW-pad)

	if newTabW > 0 {
		s.newTabStartX = leftW + gap + newTabOffset
		s.newTabEndX = s.newTabStartX + newTabW
	}

	s.cached = left + strings.Repeat(" ", gap) + right + strings.Repeat(" ", pad)
}

// View renders the status bar.
func (s *StatusBar) View() string {
	if s.cacheDirty {
		s.rebuild()
	}
	return s.cached
}
