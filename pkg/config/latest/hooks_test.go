package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHooksConfigClone(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		var cfg *HooksConfig
		assert.Nil(t, cfg.Clone())
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		cfg := &HooksConfig{}
		cloned := cfg.Clone()
		assert.Equal(t, cfg, cloned)
		assert.NotSame(t, cfg, cloned)
	})

	t.Run("nested data", func(t *testing.T) {
		t.Parallel()
		cfg := &HooksConfig{
			TurnStart: HookDefinitions{{
				Type: "builtin", Command: "add_context", Args: []string{"original"},
				Env: map[string]string{"KEY": "original"},
			}},
			PreToolUse: HookMatcherConfigs{{
				Matcher: "shell", PreemptYolo: new(true),
				Hooks: HookDefinitions{{Type: "builtin", Command: "add_context", Args: []string{"matched"}}},
			}},
			ToolGuard: HookMatcherConfigs{{Hooks: HookDefinitions{{
				Type: "evaluator", Evaluator: "safe",
				EvaluatorPolicy: &EvaluatorPolicy{Decisions: map[string]string{"true": "allow"}, MinProbability: 0.9, Fallback: "deny"},
			}}}},
			BeforeAgentRun: HookDefinitions{{
				Type: "evaluator", Evaluator: "route",
				RoutingPolicy: &RoutingPolicy{Routes: map[string]string{"coding": "coder"}, MinProbability: 0.8},
			}},
		}
		cloned := cfg.Clone()
		require.Equal(t, cfg, cloned)
		cloned.TurnStart[0].Command = "changed"
		cloned.TurnStart[0].Args[0] = "changed"
		cloned.TurnStart[0].Env["KEY"] = "changed"
		cloned.PreToolUse[0].Matcher = "changed"
		*cloned.PreToolUse[0].PreemptYolo = false
		cloned.PreToolUse[0].Hooks[0].Args[0] = "changed"
		cloned.ToolGuard[0].Hooks[0].EvaluatorPolicy.Decisions["true"] = "deny"
		cloned.ToolGuard[0].Hooks[0].EvaluatorPolicy.MinProbability = 0.1
		cloned.BeforeAgentRun[0].RoutingPolicy.Routes["coding"] = "changed"
		cloned.BeforeAgentRun[0].RoutingPolicy.MinProbability = 0.1

		assert.Equal(t, "add_context", cfg.TurnStart[0].Command)
		assert.Equal(t, []string{"original"}, cfg.TurnStart[0].Args)
		assert.Equal(t, map[string]string{"KEY": "original"}, cfg.TurnStart[0].Env)
		assert.Equal(t, "shell", cfg.PreToolUse[0].Matcher)
		assert.True(t, *cfg.PreToolUse[0].PreemptYolo)
		assert.Equal(t, []string{"matched"}, cfg.PreToolUse[0].Hooks[0].Args)
		assert.Equal(t, &EvaluatorPolicy{Decisions: map[string]string{"true": "allow"}, MinProbability: 0.9, Fallback: "deny"}, cfg.ToolGuard[0].Hooks[0].EvaluatorPolicy)
		assert.Equal(t, &RoutingPolicy{Routes: map[string]string{"coding": "coder"}, MinProbability: 0.8}, cfg.BeforeAgentRun[0].RoutingPolicy)
	})
}

func TestHooksEventsEmpty(t *testing.T) {
	t.Parallel()

	for _, cfg := range []*HooksConfig{
		nil,
		{},
		{PreToolUse: HookMatcherConfigs{}, SessionStart: HookDefinitions{}},
	} {
		for name := range cfg.Events() {
			t.Errorf("unexpected event %q", name)
		}
		assert.True(t, cfg.IsEmpty())
	}
}

func TestHooksEvents(t *testing.T) {
	t.Parallel()

	cfg := &HooksConfig{
		PreToolUse: HookMatcherConfigs{
			{Matcher: "shell", PreemptYolo: new(true), Hooks: HookDefinitions{{Command: "check"}}},
			{Matcher: "edit_file", Hooks: HookDefinitions{{Command: "validate"}}},
		},
		PostToolUse:  HookMatcherConfigs{},
		SessionStart: HookDefinitions{{Command: "setup"}, {Command: "log"}},
	}

	var names []string
	var matchers []HookMatcherConfigs
	for name, hooks := range cfg.Events() {
		names = append(names, name)
		matchers = append(matchers, hooks)
	}

	assert.Equal(t, []string{"pre_tool_use", "session_start"}, names)
	assert.Equal(t, []HookMatcherConfigs{
		cfg.PreToolUse,
		{{Hooks: cfg.SessionStart}},
	}, matchers)
	assert.False(t, cfg.IsEmpty())
}

func TestHooksEventsStopEarly(t *testing.T) {
	t.Parallel()

	cfg := &HooksConfig{
		PreToolUse:   HookMatcherConfigs{{Hooks: HookDefinitions{{Command: "check"}}}},
		SessionStart: HookDefinitions{{Command: "setup"}},
	}

	calls := 0
	cfg.Events()(func(name string, matchers HookMatcherConfigs) bool {
		calls++
		assert.Equal(t, "pre_tool_use", name)
		assert.Equal(t, cfg.PreToolUse, matchers)
		return false
	})
	assert.Equal(t, 1, calls)
}

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

func TestModelGuardConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		event, schema string
		valid         bool
	}{
		{"prompt_file_guard", "guard_decision", true},
		{"prompt_file_guard", "", false},
		{"prompt_file_guard", "pre_tool_use_decision", false},
		{"skill_content_guard", "guard_decision", true},
		{"skill_content_guard", "", false},
		{"skill_content_guard", "pre_tool_use_decision", false},
		{"post_tool_use", "guard_decision", true},
		{"post_tool_use", "pre_tool_use_decision", false},
		{"turn_start", "guard_decision", false},
		{"turn_start", "", true},
	} {
		t.Run(tc.event+"/"+tc.schema, func(t *testing.T) {
			t.Parallel()
			var cfg HooksConfig
			require.NoError(t, yaml.Unmarshal([]byte(tc.event+`: [{type: model, model: test/model, prompt: check, system_prompt: policy, schema: "`+tc.schema+`"}]`), &cfg))
			// Tool-matched events use matcher entries rather than a flat list.
			if tc.event == "post_tool_use" {
				cfg.PostToolUse = HookMatcherConfigs{{Hooks: HookDefinitions{{Type: "model", Model: "test/model", Prompt: "check", Schema: tc.schema}}}}
			}
			err := cfg.Validate()
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
