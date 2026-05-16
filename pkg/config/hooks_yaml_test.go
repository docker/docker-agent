package config

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestHooksConfig_TurnStart_YAMLRoundTrip pins the schema contract: the
// turn_start key parses into HooksConfig.TurnStart and round-trips back out
// via YAML marshaling.
func TestHooksConfig_TurnStart_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const src = `
turn_start:
  - type: builtin
    command: add_date
  - type: command
    command: ./scripts/inject-context.sh
    timeout: 5
`

	var cfg latest.HooksConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))

	require.Len(t, cfg.TurnStart, 2)
	assert.Equal(t, "builtin", cfg.TurnStart[0].Type)
	assert.Equal(t, "add_date", cfg.TurnStart[0].Command)
	assert.Equal(t, "command", cfg.TurnStart[1].Type)
	assert.Equal(t, 5, cfg.TurnStart[1].Timeout)

	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(out), "turn_start:")
}

// TestHookDefinition_Args_YAMLRoundTrip pins that the args field decodes
// into HookDefinition.Args (used by builtins like add_prompt_files to
// receive per-hook parameters without polluting Command).
func TestHookDefinition_Args_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const src = `
turn_start:
  - type: builtin
    command: add_prompt_files
    args:
      - GUIDELINES.md
      - PROJECT.md
`

	var cfg latest.HooksConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))

	require.Len(t, cfg.TurnStart, 1)
	assert.Equal(t, []string{"GUIDELINES.md", "PROJECT.md"}, cfg.TurnStart[0].Args)
}

// TestHookDefinition_BuiltinTypeAccepted pins that "builtin" passes
// validation and that anything else outside {command, builtin} is
// rejected with a descriptive error.
func TestHookDefinition_BuiltinTypeAccepted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		src      string
		wantErr  bool
		errMatch string
	}{
		{
			name: "builtin accepted",
			src: `
turn_start:
  - type: builtin
    command: add_date
`,
		},
		{
			name: "command accepted",
			src: `
turn_start:
  - type: command
    command: echo hi
`,
		},
		{
			name: "unknown type rejected",
			src: `
turn_start:
  - type: webhook
    command: https://example.com
`,
			wantErr:  true,
			errMatch: "unsupported hook type 'webhook'",
		},
		{
			name: "builtin without name rejected",
			src: `
turn_start:
  - type: builtin
`,
			wantErr:  true,
			errMatch: "command must name the builtin to invoke",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.HooksConfig
			require.NoError(t, yaml.Unmarshal([]byte(tc.src), &cfg))

			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMatch)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestHooksConfig_IsEmptyConsidersTurnStart ensures the event slice
// participates in the IsEmpty check (otherwise the runtime's executor
// builder would short-circuit even when only turn_start is configured).
func TestHooksConfig_IsEmptyConsidersTurnStart(t *testing.T) {
	t.Parallel()

	cfg := &latest.HooksConfig{
		TurnStart: []latest.HookDefinition{
			{Type: "builtin", Command: "add_date"},
		},
	}
	assert.False(t, cfg.IsEmpty())
	assert.True(t, (&latest.HooksConfig{}).IsEmpty())
}

// TestHooksConfig_OnErrorAndOnMaxIterations_YAMLRoundTrip pins that the
// two structured-error events parse into their dedicated slices and that
// IsEmpty considers them.
func TestHooksConfig_OnErrorAndOnMaxIterations_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const src = `
on_error:
  - type: command
    command: ./scripts/log-error.sh
    timeout: 5
on_max_iterations:
  - type: command
    command: ./scripts/page-oncall.sh
`

	var cfg latest.HooksConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))

	require.Len(t, cfg.OnError, 1)
	require.Len(t, cfg.OnMaxIterations, 1)
	assert.Equal(t, "./scripts/log-error.sh", cfg.OnError[0].Command)
	assert.Equal(t, "./scripts/page-oncall.sh", cfg.OnMaxIterations[0].Command)

	// Either field alone keeps IsEmpty false.
	assert.False(t, (&latest.HooksConfig{OnError: cfg.OnError}).IsEmpty())
	assert.False(t, (&latest.HooksConfig{OnMaxIterations: cfg.OnMaxIterations}).IsEmpty())
}

// TestHooksConfig_BeforeAndAfterLLMCall_YAMLRoundTrip pins that the two
// LLM-call lifecycle events parse into their dedicated slices and that
// IsEmpty considers them.
func TestHooksConfig_BeforeAndAfterLLMCall_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const src = `
before_llm_call:
  - type: command
    command: ./scripts/audit-llm-call.sh
after_llm_call:
  - type: command
    command: ./scripts/check-response.sh
    timeout: 5
`

	var cfg latest.HooksConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))

	require.Len(t, cfg.BeforeLLMCall, 1)
	require.Len(t, cfg.AfterLLMCall, 1)
	assert.Equal(t, "./scripts/audit-llm-call.sh", cfg.BeforeLLMCall[0].Command)
	assert.Equal(t, "./scripts/check-response.sh", cfg.AfterLLMCall[0].Command)

	assert.False(t, (&latest.HooksConfig{BeforeLLMCall: cfg.BeforeLLMCall}).IsEmpty())
	assert.False(t, (&latest.HooksConfig{AfterLLMCall: cfg.AfterLLMCall}).IsEmpty())
}
