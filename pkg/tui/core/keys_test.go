package core

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestBuildKeys_Defaults(t *testing.T) {
	t.Parallel()
	keys := buildKeys(nil)

	assert.Equal(t, []string{"ctrl+c"}, keys.Quit.Keys())
	assert.Equal(t, []string{"tab"}, keys.SwitchFocus.Keys())
	assert.Equal(t, []string{"ctrl+k"}, keys.Commands.Keys())
	assert.Equal(t, []string{"ctrl+h", "f1", "ctrl+?"}, keys.Help.Keys())
	assert.Equal(t, []string{"ctrl+r"}, keys.HistorySearch.Keys())
	assert.Equal(t, []string{"enter"}, keys.EditorSend.Keys())
	assert.Equal(t, []string{"ctrl+j"}, keys.EditorNewline.Keys())
}

func TestBuildKeys_Overrides(t *testing.T) {
	t.Parallel()
	settings := &userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "quit", Keys: []string{"ctrl+q"}},
			{Action: "commands", Keys: []string{"f2", "ctrl+k"}},
			{Action: "editor_newline", Keys: []string{"alt+enter"}},
			{Action: "editor_send", Keys: []string{"ctrl+d"}},
			{Action: "unknown_action", Keys: []string{"ctrl+u"}}, // ignored
		},
	}

	keys := buildKeys(settings)

	assert.Equal(t, []string{"ctrl+q"}, keys.Quit.Keys())
	assert.Equal(t, []string{"f2", "ctrl+k"}, keys.Commands.Keys())
	assert.Equal(t, []string{"alt+enter"}, keys.EditorNewline.Keys())
	assert.Equal(t, []string{"ctrl+d"}, keys.EditorSend.Keys())

	// The first override key drives the help label, capitalized for display.
	assert.Equal(t, "Ctrl+q", keys.Quit.Help().Key)

	// Untouched actions keep their defaults.
	assert.Equal(t, []string{"tab"}, keys.SwitchFocus.Keys())
	assert.Equal(t, []string{"ctrl+h", "f1", "ctrl+?"}, keys.Help.Keys())
}

func TestBuildKeys_EmptySettings(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{})
	assert.Equal(t, []string{"ctrl+c"}, keys.Quit.Keys())
	assert.Equal(t, []string{"enter"}, keys.EditorSend.Keys())
}

func TestBuildKeys_EmptyKeysIgnored(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "quit", Keys: []string{}},
		},
	})
	assert.Equal(t, []string{"ctrl+c"}, keys.Quit.Keys())
}

func TestBuildKeys_MalformedKeysIgnored(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "quit", Keys: []string{"ctrl+q", " ", "", "ctrl q"}},
		},
	})
	// Only the well-formed key survives.
	assert.Equal(t, []string{"ctrl+q"}, keys.Quit.Keys())
}

func TestBuildKeys_IntraConfigConflict(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "quit", Keys: []string{"ctrl+q"}},
			{Action: "suspend", Keys: []string{"ctrl+q"}}, // conflicts with quit, skipped
		},
	})
	assert.Equal(t, []string{"ctrl+q"}, keys.Quit.Keys())
	assert.Equal(t, []string{"ctrl+z"}, keys.Suspend.Keys()) // default preserved
}

// A custom key that collides with a non-remapped action's default must be
// rejected so it is never bound to two actions.
func TestBuildKeys_ConflictWithDefault(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "editor_send", Keys: []string{"ctrl+j"}}, // ctrl+j is newline's default
		},
	})
	assert.Equal(t, []string{"enter"}, keys.EditorSend.Keys())     // rejected, stays default
	assert.Equal(t, []string{"ctrl+j"}, keys.EditorNewline.Keys()) // default intact
}

// Reassigning a key away from its default action frees it for another action.
func TestBuildKeys_ReuseFreedKey(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "editor_newline", Keys: []string{"alt+enter"}}, // frees ctrl+j
			{Action: "editor_send", Keys: []string{"ctrl+j"}},       // now allowed
		},
	})
	assert.Equal(t, []string{"alt+enter"}, keys.EditorNewline.Keys())
	assert.Equal(t, []string{"ctrl+j"}, keys.EditorSend.Keys())
}

func TestValidateKey(t *testing.T) {
	t.Parallel()
	valid := []string{"ctrl+q", "q", "?", "enter", "tab", "esc", "space", "f1", "f13", "shift+enter", "alt+enter", "ctrl+?", "ctrl+shift+a", "up"}
	for _, k := range valid {
		assert.True(t, validateKey(k), "expected %q to be valid", k)
	}
	invalid := []string{"", " ", "ctrl q", "foobar", "ctrl+foobar", "entre", "f0", "f64", "bogus+a", "ctrl+", "+"}
	for _, k := range invalid {
		assert.False(t, validateKey(k), "expected %q to be invalid", k)
	}
}

// A typo'd key name must be rejected so it never silently replaces a critical
// action's working default (the lock-out footgun).
func TestBuildKeys_InvalidKeyNameKeepsDefault(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "editor_send", Keys: []string{"foobar"}},
		},
	})
	assert.Equal(t, []string{"enter"}, keys.EditorSend.Keys())
}

// Overriding onto a key reserved by a built-in shortcut is rejected.
func TestBuildKeys_ReservedKeyConflict(t *testing.T) {
	t.Parallel()
	keys := buildKeys(&userconfig.Settings{
		Keybindings: []userconfig.Keybinding{
			{Action: "commands", Keys: []string{"ctrl+t"}}, // ctrl+t is the new-tab shortcut
		},
	})
	assert.Equal(t, []string{"ctrl+k"}, keys.Commands.Keys())
}

func TestBuildKeys_FromYAML(t *testing.T) {
	t.Parallel()
	yamlConfig := `
settings:
  keybindings:
    - action: "quit"
      keys: ["ctrl+q"]
    - action: "editor_newline"
      keys: ["alt+enter"]
    - action: "history_search"
      keys: ["ctrl+f"]
`
	var config userconfig.Config
	require.NoError(t, yaml.Unmarshal([]byte(yamlConfig), &config))

	keys := buildKeys(config.Settings)

	assert.Equal(t, []string{"ctrl+q"}, keys.Quit.Keys())
	assert.Equal(t, []string{"alt+enter"}, keys.EditorNewline.Keys())
	assert.Equal(t, []string{"ctrl+f"}, keys.HistorySearch.Keys())
	assert.Equal(t, []string{"tab"}, keys.SwitchFocus.Keys())
}

func TestGetKeys_CacheAndReset(t *testing.T) {
	t.Parallel()
	ResetKeys()
	t.Cleanup(ResetKeys)

	first := GetKeys()
	second := GetKeys()
	assert.Equal(t, first.Quit.Keys(), second.Quit.Keys())

	ResetKeys() // should not panic and forces a rebuild on next call
	assert.NotEmpty(t, GetKeys().Quit.Keys())
}

func TestValidActions(t *testing.T) {
	t.Parallel()
	actions := ValidActions()
	assert.Contains(t, actions, "editor_send")
	assert.Contains(t, actions, "editor_newline")
	assert.Contains(t, actions, "quit")
	assert.Contains(t, actions, "close_tab")
	assert.Len(t, actions, 16)
}
