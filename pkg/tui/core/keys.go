package core

import (
	"sync"

	"charm.land/bubbles/v2/key"

	"github.com/docker/docker-agent/pkg/userconfig"
)

// KeyMap contains global keybindings used across the TUI
type KeyMap struct {
	Quit                  key.Binding
	SwitchFocus           key.Binding
	Commands              key.Binding
	Help                  key.Binding
	ToggleYolo            key.Binding
	ToggleHideToolResults key.Binding
	CycleAgent            key.Binding
	ModelPicker           key.Binding
	ClearQueue            key.Binding
	Suspend               key.Binding
	ToggleSidebar         key.Binding
	EditExternal          key.Binding
	HistorySearch         key.Binding
}

var (
	cachedKeys KeyMap
	keysOnce   sync.Once
)

// DefaultKeyMap returns the default keybindings
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit:                  key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
		SwitchFocus:           key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch focus")),
		Commands:              key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("ctrl+k", "commands")),
		Help:                  key.NewBinding(key.WithKeys("ctrl+h", "f1", "ctrl+?"), key.WithHelp("ctrl+h", "help")),
		ToggleYolo:            key.NewBinding(key.WithKeys("ctrl+y"), key.WithHelp("ctrl+y", "toggle yolo mode")),
		ToggleHideToolResults: key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("ctrl+o", "toggle hide tool results")),
		CycleAgent:            key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "cycle agent")),
		ModelPicker:           key.NewBinding(key.WithKeys("ctrl+m"), key.WithHelp("ctrl+m", "model picker")),
		ClearQueue:            key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "clear queue")),
		Suspend:               key.NewBinding(key.WithKeys("ctrl+z"), key.WithHelp("ctrl+z", "suspend")),
		ToggleSidebar:         key.NewBinding(key.WithKeys("ctrl+b"), key.WithHelp("ctrl+b", "toggle sidebar")),
		EditExternal:          key.NewBinding(key.WithKeys("ctrl+g"), key.WithHelp("ctrl+g", "edit in external editor")),
		HistorySearch:         key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "history search")),
	}
}

// buildKeys merges user config overrides with the defaults to produce a KeyMap.
// This is separated from GetKeys() to allow testing with mock settings.
func buildKeys(settings *userconfig.Settings) KeyMap {
	keys := DefaultKeyMap()

	if settings != nil && settings.Keybindings != nil {
		for _, b := range *settings.Keybindings {
			if len(b.Keys) == 0 {
				continue
			}

			usrKeys := b.Keys
			keyName := usrKeys[0]

			switch b.Action {
			case "quit":
				keys.Quit = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "quit"))
			case "switch_focus":
				keys.SwitchFocus = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "switch focus"))
			case "commands":
				keys.Commands = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "commands"))
			case "help":
				keys.Help = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "help"))
			case "toggle_yolo":
				keys.ToggleYolo = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "toggle yolo mode"))
			case "toggle_hide_tool_results":
				keys.ToggleHideToolResults = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "toggle hide tool results"))
			case "cycle_agent":
				keys.CycleAgent = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "cycle agent"))
			case "model_picker":
				keys.ModelPicker = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "model picker"))
			case "clear_queue":
				keys.ClearQueue = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "clear queue"))
			case "suspend":
				keys.Suspend = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "suspend"))
			case "toggle_sidebar":
				keys.ToggleSidebar = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "toggle sidebar"))
			case "edit_external":
				keys.EditExternal = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "edit in external editor"))
			case "history_search":
				keys.HistorySearch = key.NewBinding(key.WithKeys(usrKeys...), key.WithHelp(keyName, "history search"))
			}
		}
	}

	return keys
}

// GetKeys returns the current keybindings, merging user config overrides with defaults.
// The result is cached after the first call.
func GetKeys() KeyMap {
	keysOnce.Do(func() {
		cachedKeys = buildKeys(userconfig.Get())
	})

	return cachedKeys
}
