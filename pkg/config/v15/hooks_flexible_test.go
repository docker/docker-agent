package v15

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookDefinitions_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		want    HookDefinitions
		wantErr string
	}{
		{
			name: "list form",
			yaml: "stop:\n  - type: command\n    command: echo one\n  - type: command\n    command: echo two\n",
			want: HookDefinitions{
				{Type: "command", Command: "echo one"},
				{Type: "command", Command: "echo two"},
			},
		},
		{
			name: "single mapping form",
			yaml: "stop:\n  type: command\n  command: echo one\n",
			want: HookDefinitions{{Type: "command", Command: "echo one"}},
		},
		{
			name:    "scalar is rejected",
			yaml:    "stop: echo one\n",
			wantErr: "hook event must be a hook or a list of hooks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg HooksConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &cfg)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Stop)
		})
	}
}

func TestHookMatcherConfigs_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	single := `
pre_tool_use:
  matcher: shell
  hooks:
    type: command
    command: echo check
`
	var cfg HooksConfig
	require.NoError(t, yaml.Unmarshal([]byte(single), &cfg))
	require.Len(t, cfg.PreToolUse, 1)
	assert.Equal(t, "shell", cfg.PreToolUse[0].Matcher)
	assert.Equal(t, HookDefinitions{{Type: "command", Command: "echo check"}}, cfg.PreToolUse[0].Hooks)

	var bad HooksConfig
	err := yaml.Unmarshal([]byte("pre_tool_use: shell\n"), &bad)
	require.ErrorContains(t, err, "hook event must be a matcher or a list of matchers")
}

func TestHookDefinitions_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	var fromList HooksConfig
	require.NoError(t, json.Unmarshal([]byte(`{"stop":[{"type":"command","command":"echo one"}]}`), &fromList))

	var fromMapping HooksConfig
	require.NoError(t, json.Unmarshal([]byte(`{"stop":{"type":"command","command":"echo one"}}`), &fromMapping))

	assert.Equal(t, fromList, fromMapping)
}

func TestHookDefinitions_MarshalAsList(t *testing.T) {
	t.Parallel()

	var cfg HooksConfig
	require.NoError(t, yaml.Unmarshal([]byte("stop:\n  type: command\n  command: echo one\n"), &cfg))

	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(out), "stop:\n- type: command\n")
}
