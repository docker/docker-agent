package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/latest"
)

const routingYAML = `
evaluators:
  task_route:
    provider: typesafe
    model: jev-latest
    type: choice
    instructions: Classify the request in input.
    choices:
      simple: Direct.
      complex: Hard.
agents:
  root:
    model: openai/gpt-5-mini
    instruction: route
    routing:
      allowed_agents: [quick, specialist]
      default_agent: quick
    hooks:
      before_agent_run:
        - type: evaluator
          evaluator: task_route
          routing_policy:
            routes: {simple: quick, complex: specialist}
            min_probability: 0.9
  quick:
    model: openai/gpt-5-mini
    instruction: quick
  specialist:
    model: openai/gpt-5
    instruction: specialist
`

const routingHCL = `
evaluator "task_route" {
  provider     = "typesafe"
  model        = "jev-latest"
  type         = "choice"
  instructions = "Classify the request in input."
  choices = {
    simple  = "Direct."
    complex = "Hard."
  }
}

agent "root" {
  model       = "openai/gpt-5-mini"
  instruction = "route"
  routing = {
    allowed_agents = ["quick", "specialist"]
    default_agent  = "quick"
  }
  hooks {
    before_agent_run {
      type      = "evaluator"
      evaluator = "task_route"
      routing_policy {
        routes         = { simple = "quick", complex = "specialist" }
        min_probability = 0.9
      }
    }
  }
}

agent "quick" {
  model       = "openai/gpt-5-mini"
  instruction = "quick"
}

agent "specialist" {
  model       = "openai/gpt-5"
  instruction = "specialist"
}
`

func TestRoutingLoadsFromYAMLAndHCL(t *testing.T) {
	t.Parallel()
	fromYAML, err := Load(t.Context(), NewBytesSource("routing.yaml", []byte(routingYAML)))
	require.NoError(t, err)
	fromHCL, err := Load(t.Context(), hcl.NewSource(NewBytesSource("routing.hcl", []byte(routingHCL))))
	require.NoError(t, err)

	root := fromHCL.Agents[0]
	require.NotNil(t, root.Routing, "HCL must not discard routing")
	assert.Equal(t, fromYAML.Agents[0].Routing, root.Routing)
	assert.Equal(t, fromYAML.Agents[0].Hooks.BeforeAgentRun, root.Hooks.BeforeAgentRun)
	assert.Equal(t, fromYAML.Evaluators, fromHCL.Evaluators)
}

func TestRoutingHCLBlockFormIsRejectedNotDiscarded(t *testing.T) {
	t.Parallel()
	// `routing {}` blocks already mean model routing rules, so agent routing is attribute-only.
	const src = `
agent "root" {
  model       = "openai/gpt-5-mini"
  instruction = "route"
  routing {
    allowed_agents = ["quick"]
  }
}
`
	_, err := Load(t.Context(), hcl.NewSource(NewBytesSource("routing.hcl", []byte(src))))
	require.Error(t, err)
}

func TestMergeHooksKeepsControlEvents(t *testing.T) {
	t.Parallel()
	base := &latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{{Type: "command", Command: "a"}}}
	cli := &latest.HooksConfig{
		BeforeAgentRun:     latest.HookDefinitions{{Type: "command", Command: "b"}},
		AfterAgentComplete: latest.HookDefinitions{{Type: "command", Command: "c"}},
	}
	merged := MergeHooks(base, cli)
	assert.Len(t, merged.BeforeAgentRun, 2)
	assert.Len(t, merged.AfterAgentComplete, 1)
	assert.Len(t, base.BeforeAgentRun, 1, "inputs are not mutated")
}

func TestInheritedControlHooksAreNotSilentlyDropped(t *testing.T) {
	t.Parallel()
	cfg, err := Load(t.Context(), NewBytesSource("routing.yaml", []byte(routingYAML)))
	require.NoError(t, err)

	// A global selector joins the agent's own selector: two distinct selectors conflict.
	MergeAgentHooks(cfg, &latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{{Type: "command", Command: "./global.sh"}}})
	require.ErrorContains(t, cfg.ValidateEvaluators(), "at most one selector")

	// An inherited selector on an agent without routing is reported, never ignored.
	plain := latest.Config{Agents: latest.Agents{{Name: "root", Model: "openai/gpt-5-mini"}}}
	MergeAgentHooks(&plain, &latest.HooksConfig{AfterAgentComplete: latest.HookDefinitions{{Type: "command", Command: "./global.sh"}}})
	assert.ErrorContains(t, plain.ValidateEvaluators(), "require routing.allowed_agents")
}

func TestRoutingRequirements(t *testing.T) {
	t.Parallel()
	cfg, err := Load(t.Context(), NewBytesSource("routing.yaml", []byte(routingYAML)))
	require.NoError(t, err)
	reqs := Requires(cfg)
	assert.Equal(t, []string{"agents.root.routing"}, reqs.Features[FeatureAgentRouting])
	assert.Contains(t, reqs.Features, FeatureHooks)
	assert.Contains(t, reqs.Features, FeatureEvaluators)

	var unmet *UnsupportedError
	require.ErrorAs(t, reqs.Check(func(string) bool { return true }, func(string) bool { return true },
		[]Feature{FeatureHooks, FeatureEvaluators}), &unmet)
	require.Len(t, unmet.Features, 1)
	assert.Equal(t, string(FeatureAgentRouting), unmet.Features[0].Name)
}
