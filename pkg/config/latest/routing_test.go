package latest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const routingSource = `evaluators:
  task_route:
    provider: typesafe
    model: jev-latest
    type: choice
    instructions: Classify the work required by the request in input.
    choices:
      simple: Direct explanations.
      complex: Hard debugging.
      unclear: Missing information.
agents:
  root:
    model: openai/gpt-5-mini
    routing:
      allowed_agents: [quick, specialist, clarifier]
      default_agent: clarifier
    hooks:
      before_agent_run:
        - type: evaluator
          evaluator: task_route
          routing_policy:
            routes: {simple: quick, complex: specialist, unclear: clarifier}
            min_probability: 0.85
  quick:
    model: openai/gpt-5-mini
  specialist:
    model: openai/gpt-5
    force_handoff: reviewer
  reviewer:
    model: openai/gpt-5
  clarifier:
    model: openai/gpt-5-mini
`

func TestRoutingConfigRoundTrip(t *testing.T) {
	t.Parallel()
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(routingSource), &cfg))

	root := cfg.Agents[0]
	assert.Equal(t, &AgentRouting{AllowedAgents: []string{"quick", "specialist", "clarifier"}, DefaultAgent: "clarifier"}, root.Routing)
	policy := root.Hooks.BeforeAgentRun[0].RoutingPolicy
	assert.Equal(t, map[string]string{"simple": "quick", "complex": "specialist", "unclear": "clarifier"}, policy.Routes)
	assert.InDelta(t, 0.85, policy.MinProbability, 1e-9)
	assert.False(t, root.Hooks.IsEmpty())
	assert.True(t, root.Hooks.HasControlHooks())

	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	var decoded Config
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NoError(t, decoded.Validate())
	assert.Equal(t, cfg.Agents[0].Routing, decoded.Agents[0].Routing)
	assert.Equal(t, cfg.Agents[0].Hooks, decoded.Agents[0].Hooks)
}

func TestRoutingConfigValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, old, replacement, want string }{
		{"valid baseline", "", "", ""},
		{"unknown field", "min_probability: 0.85", "min_probability: 0.85\n            sneaky: true", "hook event must be"},
		{"unknown evaluator", "evaluator: task_route", "evaluator: missing", "unknown evaluator"},
		{"missing route", "unclear: clarifier}", "}", "missing route for evaluator choice"},
		{"extra route", "unclear: clarifier}", "unclear: clarifier, bogus: quick}", "unknown evaluator choice"},
		{"destination not allowed", "complex: specialist", "complex: reviewer", "not in routing.allowed_agents"},
		{"unknown allowed agent", "allowed_agents: [quick,", "allowed_agents: [ghost, quick,", "unknown local agent"},
		{"self route", "allowed_agents: [quick,", "allowed_agents: [root, quick,", "must not include the agent itself"},
		{"duplicate allowed", "allowed_agents: [quick,", "allowed_agents: [quick, quick,", "more than once"},
		{"default outside allowlist", "default_agent: clarifier", "default_agent: reviewer", "must be in routing.allowed_agents"},
		{"missing default", "      default_agent: clarifier\n", "", "routing.default_agent is required"},
		{"empty allowlist", "allowed_agents: [quick, specialist, clarifier]", "allowed_agents: []", "must not be empty"},
		{"threshold zero", "min_probability: 0.85", "min_probability: 0", "min_probability"},
		{"threshold above one", "min_probability: 0.85", "min_probability: 1.5", "min_probability"},
		{
			"evaluator policy on control event", "routing_policy:\n            routes: {simple: quick, complex: specialist, unclear: clarifier}\n            min_probability: 0.85",
			"evaluator_policy:\n            decisions: {simple: allow}\n            min_probability: 0.85\n            fallback: ask", "routing_policy, not evaluator_policy",
		},
		{
			"model hook", "type: evaluator\n          evaluator: task_route\n          routing_policy:\n            routes: {simple: quick, complex: specialist, unclear: clarifier}\n            min_probability: 0.85",
			"type: model\n          model: openai/gpt-5-mini\n          prompt: route it", "model hooks cannot select routes",
		},
		{
			"non-choice evaluator", "type: choice\n    instructions: Classify the work required by the request in input.\n    choices:\n      simple: Direct explanations.\n      complex: Hard debugging.\n      unclear: Missing information.",
			"type: boolean\n    instructions: Is it hard?", "requires a choice evaluator",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := routingSource
			if tc.old != "" {
				require.Contains(t, src, tc.old)
				src = strings.Replace(src, tc.old, tc.replacement, 1)
			}
			var cfg Config
			err := yaml.Unmarshal([]byte(src), &cfg)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRoutingCompositionValidation(t *testing.T) {
	t.Parallel()
	parse := func(t *testing.T, edit func(*Config)) error {
		t.Helper()
		var cfg Config
		require.NoError(t, yaml.Unmarshal([]byte(routingSource), &cfg))
		edit(&cfg)
		return cfg.Validate()
	}
	command := HookDefinition{Type: "command", Command: "./select.sh"}

	t.Run("one distinct selector per event", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) { c.Agents[0].Hooks.BeforeAgentRun = append(c.Agents[0].Hooks.BeforeAgentRun, command) })
		require.ErrorContains(t, err, "at most one selector")
	})
	t.Run("identical selectors deduplicate", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) {
			c.Agents[0].Hooks.BeforeAgentRun = append(c.Agents[0].Hooks.BeforeAgentRun, c.Agents[0].Hooks.BeforeAgentRun[0])
		})
		require.NoError(t, err)
	})
	t.Run("hooks require routing", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) { c.Agents[0].Routing = nil })
		require.ErrorContains(t, err, "require routing.allowed_agents")
	})
	t.Run("routing requires hooks", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) { c.Agents[0].Hooks = nil })
		require.ErrorContains(t, err, "routing requires a before_agent_run or after_agent_complete hook")
	})
	t.Run("completion hook conflicts with force_handoff", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) {
			c.Agents[2].Routing = &AgentRouting{AllowedAgents: []string{"reviewer"}}
			c.Agents[2].Hooks = &HooksConfig{AfterAgentComplete: HookDefinitions{command}}
		})
		require.ErrorContains(t, err, "cannot be combined with force_handoff")
	})
	t.Run("command selector may route to any allowed agent", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) {
			c.Agents[0].Hooks = &HooksConfig{BeforeAgentRun: HookDefinitions{command}}
			c.Agents[0].Routing = &AgentRouting{AllowedAgents: []string{"quick"}}
		})
		require.NoError(t, err)
	})
	t.Run("harness agents cannot be route targets", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) { c.Agents[1].Harness = &HarnessConfig{} })
		require.ErrorContains(t, err, "harness agent")
	})
	t.Run("hook cycles are rejected", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) {
			c.Agents[1].Routing = &AgentRouting{AllowedAgents: []string{"root"}}
			c.Agents[1].ForceHandoff = ""
			c.Agents[1].Hooks = &HooksConfig{AfterAgentComplete: HookDefinitions{command}}
		})
		require.ErrorContains(t, err, "routing cycle")
	})
	t.Run("cycles through forced handoffs are rejected", func(t *testing.T) {
		t.Parallel()
		err := parse(t, func(c *Config) { c.Agents[3].ForceHandoff = "root" })
		require.ErrorContains(t, err, "routing cycle")
	})
	t.Run("routing policy is rejected on tool_guard", func(t *testing.T) {
		t.Parallel()
		hooks := &HooksConfig{ToolGuard: HookMatcherConfigs{{Hooks: HookDefinitions{{
			Type: "evaluator", Evaluator: "task_route",
			RoutingPolicy: &RoutingPolicy{Routes: map[string]string{"a": "b"}, MinProbability: 0.5},
		}}}}}
		require.ErrorContains(t, hooks.Validate(), "use evaluator_policy, not routing_policy")
	})
	t.Run("model hooks are rejected on control events", func(t *testing.T) {
		t.Parallel()
		hooks := &HooksConfig{AfterAgentComplete: HookDefinitions{{Type: "model", Model: "openai/gpt-5-mini", Prompt: "route"}}}
		require.ErrorContains(t, hooks.Validate(), "model hooks cannot select routes")
	})
	t.Run("routing policy requires an evaluator hook", func(t *testing.T) {
		t.Parallel()
		hooks := &HooksConfig{BeforeAgentRun: HookDefinitions{{Type: "command", Command: "x", RoutingPolicy: &RoutingPolicy{}}}}
		require.ErrorContains(t, hooks.Validate(), "evaluator fields require type evaluator")
	})
	t.Run("plain configs are untouched", func(t *testing.T) {
		t.Parallel()
		var cfg Config
		require.NoError(t, yaml.Unmarshal([]byte("agents:\n  root:\n    model: openai/gpt-5-mini\n    force_handoff: root\n"), &cfg))
	})
}
