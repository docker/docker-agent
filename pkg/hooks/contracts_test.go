package hooks

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks/events"
)

func TestEventContractsMatchConfigurationAndSchema(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../agent-schema.json")
	require.NoError(t, err)
	var schema struct {
		Definitions map[string]struct{ Properties map[string]json.RawMessage }
	}
	require.NoError(t, json.Unmarshal(data, &schema))
	contracts := map[string]events.Contract{}
	for c := range events.All() {
		require.NotContains(t, contracts, c.Name)
		contracts[c.Name] = c
		require.Contains(t, schema.Definitions["HooksConfig"].Properties, c.Name)
		if c.FailClosed {
			assert.True(t, c.CanBlock)
		}
	}
	cfg := &Config{}
	v := reflect.ValueOf(cfg).Elem()
	require.Len(t, contracts, v.NumField())
	for i := range v.NumField() {
		name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
		c, ok := contracts[name]
		require.True(t, ok, name)
		if c.ToolMatched {
			v.Field(i).Set(reflect.ValueOf([]MatcherConfig{{Hooks: []Hook{{Type: HookTypeCommand, Command: "true"}}}}).Convert(v.Field(i).Type()))
		} else {
			v.Field(i).Set(reflect.ValueOf([]Hook{{Type: HookTypeCommand, Command: "true"}}).Convert(v.Field(i).Type()))
		}
	}
	require.False(t, cfg.IsEmpty())
	require.NoError(t, cfg.Validate())
	exec := NewExecutor(cfg, "", nil)
	for name := range contracts {
		assert.True(t, exec.Has(EventType(name)), name)
	}
	assert.True(t, (*Config)(nil).IsEmpty())
}

func TestStrictOutputContractCapabilities(t *testing.T) {
	t.Parallel()

	for c := range events.All() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name    string
				out     *Output
				allowed bool
			}{
				{"block", &Output{Decision: "block"}, c.CanBlock},
				{"permission", &Output{HookSpecificOutput: &HookSpecificOutput{PermissionDecision: DecisionDeny}}, c.Permission()},
				{"input", &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedInput: map[string]any{}}}, c.Rewrite == events.RewriteToolInput},
				{"response", &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedToolResponse: new("")}}, c.Rewrite == events.RewriteToolResponse},
				{"context", NewAdditionalContextOutput(EventType(c.Name), "context"), c.Context},
				{"metadata", &Output{HookSpecificOutput: &HookSpecificOutput{Metadata: map[string]string{}}}, c.Metadata},
				{"summary", &Output{HookSpecificOutput: &HookSpecificOutput{Summary: "summary"}}, c.Summary},
			} {
				err := validateOutput(EventType(c.Name), tc.out, true)
				if tc.allowed {
					assert.NoError(t, err, tc.name)
				} else {
					assert.Error(t, err, tc.name)
				}
			}
		})
	}
}

func TestEventContractsDoNotSurfaceUnusedContext(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("context", func(_ context.Context, in *Input, _ []string) (*Output, error) {
		return NewAdditionalContextOutput(in.HookEventName, "unused"), nil
	}))
	for _, event := range []EventType{EventStop, EventPostToolUse, EventBeforeLLMCall} {
		exec := NewExecutorWithRegistry(configWithFlatHook(event, Hook{Type: HookTypeBuiltin, Command: "context"}), "", nil, registry)
		result, err := exec.Dispatch(t.Context(), event, &Input{})
		require.NoError(t, err)
		assert.Empty(t, result.AdditionalContext)
	}
}
