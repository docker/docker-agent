package core

import (
	"fmt"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// KeyMapHelp interface for components that provide help
type KeyMapHelp interface {
	Help() help.KeyMap
}

// simpleHelp implements help.KeyMap with simple key bindings
type simpleHelp struct {
	list []key.Binding
}

// NewSimpleHelp creates a new simple help system
func NewSimpleHelp(list []key.Binding) help.KeyMap {
	return &simpleHelp{
		list,
	}
}

// ShortHelp implements help.KeyMap
func (s *simpleHelp) ShortHelp() []key.Binding {
	return s.list
}

// FullHelp implements help.KeyMap
func (s *simpleHelp) FullHelp() [][]key.Binding {
	return nil
}

// CmdHandler creates a command that returns the given message
func CmdHandler(msg tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return msg
	}
}

// Resolve retrieves a dependency of type T from the given tea.Model.
//
// This function provides a type-safe way to access dependencies (such as *app.App,
// *service.SessionState, chat.Page, or editor.Editor) from a model that implements
// the Resolve(any) any method. The model acts as a dependency provider, allowing
// command handlers and other components to access shared state without tight coupling.
//
// Usage:
//
//	app := core.Resolve[*app.App](model)
//	sessionState := core.Resolve[*service.SessionState](model)
//	chatPage := core.Resolve[chat.Page](model)
//
// Panics if the model does not implement Resolve or cannot provide the requested type.
//
// # Experimental
//
// Notice: This function is EXPERIMENTAL and may be changed or removed in a
// later release.
func Resolve[T any](m tea.Model) T {
	if r, ok := m.(interface{ Resolve(key any) any }); ok {
		if v := r.Resolve(any((*T)(nil))); v != nil {
			return v.(T)
		}
		panic(fmt.Sprintf("tui/core: model cannot provide type %T", *new(T)))
	}
	panic("tui/core: model does not implement Resolve(key any) any")
}
