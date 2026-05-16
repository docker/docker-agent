package layout

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Sizeable represents components that can be resized
type Sizeable interface {
	SetSize(width, height int) tea.Cmd
}

// Focusable represents components that can receive focus
type Focusable interface {
	Focus() tea.Cmd
	Blur() tea.Cmd
}

type Positionable interface {
	SetPosition(x, y int) tea.Cmd
}

// Help represents components that provide help information
type Help interface {
	Bindings() []key.Binding
	Help() help.KeyMap
}

// Model is the base interface for all TUI models
type Model interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Model, tea.Cmd)
	View() string
	Sizeable
}

// CollapsedViewer is implemented by components that provide a simplified view
// for use in collapsed reasoning blocks.
type CollapsedViewer interface {
	CollapsedView() string
}
