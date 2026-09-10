package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHooksValidateContracts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, config, want string }{
		{"matcher", `pre_tool_use: [{matcher: "[", hooks: [{type: command, command: true}]}]`, "invalid matcher"},
		{"policy", `turn_start: [{type: command, command: true, on_error: blok}]`, "on_error must"},
		{"negative timeout", `turn_start: [{type: command, command: true, timeout: -1}]`, "timeout must"},
		{"unsupported block", `stop: [{type: command, command: true, on_error: block}]`, "not supported"},
		{"invalid lane", `post_tool_use: [{preempt_yolo: false, hooks: [{type: command, command: true}]}]`, "only valid on pre_tool_use"},
		{"strict output", `before_llm_call: [{type: command, command: true, strict_output: true, on_error: block}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var cfg HooksConfig
			require.NoError(t, yaml.Unmarshal([]byte(tc.config), &cfg))
			err := cfg.Validate()
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
				assert.True(t, cfg.BeforeLLMCall[0].StrictOutput)
			}
		})
	}
}
