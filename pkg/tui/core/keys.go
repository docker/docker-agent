package core

import (
	"log/slog"
	"strings"
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

type keyField struct {
	binding *key.Binding
	help    string
}

func validateKeys(keys []string, action string, boundKeys map[string]string) []string {
	var validKeys []string
	for _, k := range keys {
		kStr := strings.TrimSpace(k)
		if kStr == "" || strings.Contains(kStr, " ") {
			slog.Warn("Invalid key string ignored", "action", action, "key", k)
			continue
		}

		if existingAction, exists := boundKeys[kStr]; exists {
			slog.Warn("Keybinding conflict detected", "key", kStr, "action", action, "conflicts_with", existingAction)
		} else {
			boundKeys[kStr] = action
		}

		validKeys = append(validKeys, kStr)
	}
	return validKeys
}

// applyUserKeybindings loops through user-defined keybindings and overrides the defaults.
// Basic string validation and key conflict detection is applied, any issues are logged.
func applyUserKeybindings(bindings []userconfig.Keybinding, actionMap map[string]keyField) {
	boundKeys := make(map[string]string)

	for _, b := range bindings {
		if len(b.Keys) == 0 {
			slog.Warn("Keybinding ignored: no keys specified", "action", b.Action)
			continue
		}

		if f, ok := actionMap[b.Action]; ok {
			validKeys := validateKeys(b.Keys, b.Action, boundKeys)

			if len(validKeys) > 0 {
				*f.binding = key.NewBinding(key.WithKeys(validKeys...), key.WithHelp(validKeys[0], f.help))
			}
		} else {
			slog.Warn("Unrecognized keybinding action", "action", b.Action)
		}
	}
}

// buildKeys merges user config overrides with the defaults to produce a KeyMap.
// This is separated from GetKeys() to allow testing with mock settings.
func buildKeys(settings *userconfig.Settings) KeyMap {
	keys := DefaultKeyMap()

	if settings != nil && settings.Keybindings != nil {
		actionMap := map[string]keyField{
			"quit":                     {&keys.Quit, "quit"},
			"switch_focus":             {&keys.SwitchFocus, "switch focus"},
			"commands":                 {&keys.Commands, "commands"},
			"help":                     {&keys.Help, "help"},
			"toggle_yolo":              {&keys.ToggleYolo, "toggle yolo mode"},
			"toggle_hide_tool_results": {&keys.ToggleHideToolResults, "toggle hide tool results"},
			"cycle_agent":              {&keys.CycleAgent, "cycle agent"},
			"model_picker":             {&keys.ModelPicker, "model picker"},
			"clear_queue":              {&keys.ClearQueue, "clear queue"},
			"suspend":                  {&keys.Suspend, "suspend"},
			"toggle_sidebar":           {&keys.ToggleSidebar, "toggle sidebar"},
			"edit_external":            {&keys.EditExternal, "edit in external editor"},
			"history_search":           {&keys.HistorySearch, "history search"},
		}

		applyUserKeybindings(*settings.Keybindings, actionMap)
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
