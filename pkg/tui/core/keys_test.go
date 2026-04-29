package core

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestBuildKeys_Defaults(t *testing.T) {
	keys := buildKeys(nil)

	// Verify defaults
	assert.Equal(t, []string{"ctrl+c"}, keys.Quit.Keys())
	assert.Equal(t, []string{"tab"}, keys.SwitchFocus.Keys())
	assert.Equal(t, []string{"ctrl+k"}, keys.Commands.Keys())
	assert.Equal(t, []string{"ctrl+h", "f1", "ctrl+?"}, keys.Help.Keys())
	assert.Equal(t, []string{"ctrl+y"}, keys.ToggleYolo.Keys())
	assert.Equal(t, []string{"ctrl+o"}, keys.ToggleHideToolResults.Keys())
	assert.Equal(t, []string{"ctrl+s"}, keys.CycleAgent.Keys())
	assert.Equal(t, []string{"ctrl+m"}, keys.ModelPicker.Keys())
	assert.Equal(t, []string{"ctrl+x"}, keys.ClearQueue.Keys())
	assert.Equal(t, []string{"ctrl+z"}, keys.Suspend.Keys())
	assert.Equal(t, []string{"ctrl+b"}, keys.ToggleSidebar.Keys())
	assert.Equal(t, []string{"ctrl+g"}, keys.EditExternal.Keys())
	assert.Equal(t, []string{"ctrl+r"}, keys.HistorySearch.Keys())
}

func TestBuildKeys_Overrides(t *testing.T) {
	settings := &userconfig.Settings{
		Keybindings: &[]userconfig.Keybindings{
			{Action: "quit", Keys: []string{"ctrl+q"}},
			{Action: "switch_focus", Keys: []string{"ctrl+t"}},
			{Action: "commands", Keys: []string{"f2", "ctrl+k"}},
			{Action: "unknown_action", Keys: []string{"ctrl+u"}}, // Should be ignored
		},
	}

	keys := buildKeys(settings)

	// Verify overrides
	assert.Equal(t, []string{"ctrl+q"}, keys.Quit.Keys())
	assert.Equal(t, []string{"ctrl+t"}, keys.SwitchFocus.Keys())

	// Verify arrays are maintained
	assert.Equal(t, []string{"f2", "ctrl+k"}, keys.Commands.Keys())

	// Verify defaults are preserved where not overridden
	assert.Equal(t, []string{"ctrl+h", "f1", "ctrl+?"}, keys.Help.Keys())
	assert.Equal(t, []string{"ctrl+y"}, keys.ToggleYolo.Keys())
	assert.Equal(t, []string{"ctrl+o"}, keys.ToggleHideToolResults.Keys())
	assert.Equal(t, []string{"ctrl+s"}, keys.CycleAgent.Keys())
	assert.Equal(t, []string{"ctrl+m"}, keys.ModelPicker.Keys())
	assert.Equal(t, []string{"ctrl+x"}, keys.ClearQueue.Keys())
	assert.Equal(t, []string{"ctrl+z"}, keys.Suspend.Keys())
	assert.Equal(t, []string{"ctrl+b"}, keys.ToggleSidebar.Keys())
	assert.Equal(t, []string{"ctrl+g"}, keys.EditExternal.Keys())
	assert.Equal(t, []string{"ctrl+r"}, keys.HistorySearch.Keys())
}

func TestBuildKeys_EmptySettings(t *testing.T) {
	settings := &userconfig.Settings{}
	keys := buildKeys(settings)

	// Verify defaults
	assert.Equal(t, []string{"ctrl+c"}, keys.Quit.Keys())
	assert.Equal(t, []string{"tab"}, keys.SwitchFocus.Keys())
}

func TestBuildKeys_EmptyKey(t *testing.T) {
	settings := &userconfig.Settings{
		Keybindings: &[]userconfig.Keybindings{
			{Action: "quit", Keys: []string{}}, // Should be ignored
		},
	}
	keys := buildKeys(settings)

	// Verify defaults remain
	assert.Equal(t, []string{"ctrl+c"}, keys.Quit.Keys())
}

func TestBuildKeys_FromYAML(t *testing.T) {
	yamlConfig := `
settings:
  keybindings:
    - action: "quit"
      keys: ["ctrl+q"]
    - action: "commands"
      keys: ["f2", "ctrl+k"]
    - action: "history_search"
      keys: ["ctrl+f"]
`

	var config userconfig.Config
	err := yaml.Unmarshal([]byte(yamlConfig), &config)
	require.NoError(t, err)

	keys := buildKeys(config.Settings)

	// Verify the keys loaded correctly from the YAML unmarshal
	assert.Equal(t, []string{"ctrl+q"}, keys.Quit.Keys())
	assert.Equal(t, []string{"f2", "ctrl+k"}, keys.Commands.Keys())
	assert.Equal(t, []string{"ctrl+f"}, keys.HistorySearch.Keys())

	// Verify defaults are preserved for missing YAML fields
	assert.Equal(t, []string{"tab"}, keys.SwitchFocus.Keys())
	assert.Equal(t, []string{"ctrl+h", "f1", "ctrl+?"}, keys.Help.Keys())
}
