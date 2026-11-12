package latest

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
)

// Control events let a hook select the next agent.
const (
	EventBeforeAgentRun     = "before_agent_run"
	EventAfterAgentComplete = "after_agent_complete"
)

var controlEvents = []string{EventBeforeAgentRun, EventAfterAgentComplete}

// AgentRouting declares the local agents an agent's control hooks may select.
type AgentRouting struct {
	// AllowedAgents lists every agent a hook on this agent may route to.
	AllowedAgents []string `json:"allowed_agents,omitempty" yaml:"allowed_agents,omitempty"`
	// DefaultAgent receives the route when an evaluator assessment is uncertain or fails.
	DefaultAgent string `json:"default_agent,omitempty" yaml:"default_agent,omitempty"`
}

// RoutingPolicy maps a choice evaluator's outcomes to agents.
type RoutingPolicy struct {
	// Routes maps every evaluator choice to an allowed agent.
	Routes map[string]string `json:"routes" yaml:"routes"`
	// MinProbability is the lowest selected-choice probability that routes via Routes.
	MinProbability float64 `json:"min_probability" yaml:"min_probability"`
}

// Validate checks a policy independently of its evaluator and agent.
func (p *RoutingPolicy) Validate() error {
	if p == nil {
		return errors.New("routing_policy is required")
	}
	if len(p.Routes) == 0 {
		return errors.New("routing_policy.routes must not be empty")
	}
	if math.IsNaN(p.MinProbability) || math.IsInf(p.MinProbability, 0) || p.MinProbability <= 0 || p.MinProbability > 1 {
		return errors.New("routing_policy.min_probability must be greater than 0 and at most 1")
	}
	for choice, agent := range p.Routes {
		if strings.TrimSpace(choice) == "" || strings.TrimSpace(agent) == "" {
			return errors.New("routing_policy.routes must not contain empty choices or agents")
		}
	}
	return nil
}

// controlHooks returns the distinct hooks configured for a control event.
func (h *HooksConfig) controlHooks(event string) []HookDefinition {
	if h == nil {
		return nil
	}
	var configured HookDefinitions
	switch event {
	case EventBeforeAgentRun:
		configured = h.BeforeAgentRun
	case EventAfterAgentComplete:
		configured = h.AfterAgentComplete
	}
	var distinct []HookDefinition
	for _, hook := range configured {
		if !slices.ContainsFunc(distinct, func(existing HookDefinition) bool { return reflect.DeepEqual(existing, hook) }) {
			distinct = append(distinct, hook)
		}
	}
	return distinct
}

// HasControlHooks reports whether h configures any routing control event.
func (h *HooksConfig) HasControlHooks() bool {
	return h != nil && (len(h.BeforeAgentRun) > 0 || len(h.AfterAgentComplete) > 0)
}

// validateRouting checks routing declarations, control-hook composition, and
// the combined graph of possible hook and force_handoff transitions.
func (t *Config) validateRouting() error {
	byName := make(map[string]*AgentConfig, len(t.Agents))
	for i := range t.Agents {
		byName[t.Agents[i].Name] = &t.Agents[i]
	}
	edges := make(map[string][]string, len(t.Agents))
	routed := false
	for i := range t.Agents {
		a := &t.Agents[i]
		routed = routed || a.Routing != nil || a.Hooks.HasControlHooks()
	}
	if !routed {
		return nil // force_handoff cycles are validated with their own diagnostics
	}
	for i := range t.Agents {
		a := &t.Agents[i]
		targets, err := t.validateAgentRouting(a, byName)
		if err != nil {
			return fmt.Errorf("agents.%s: %w", a.Name, err)
		}
		edges[a.Name] = targets
	}
	return rejectRoutingCycles(t.Agents, edges)
}

func (t *Config) validateAgentRouting(a *AgentConfig, byName map[string]*AgentConfig) ([]string, error) {
	var edges []string
	if a.ForceHandoff != "" && a.ForceHandoff != a.Name { // self handoffs have their own diagnostic
		if _, local := byName[a.ForceHandoff]; local {
			edges = append(edges, a.ForceHandoff)
		}
	}

	var selectors []HookDefinition
	for _, event := range controlEvents {
		hooks := a.Hooks.controlHooks(event)
		if len(hooks) > 1 {
			return nil, fmt.Errorf("hooks.%s: at most one selector is allowed after merging, found %d distinct definitions", event, len(hooks))
		}
		selectors = append(selectors, hooks...)
	}
	if len(selectors) == 0 {
		if a.Routing != nil {
			return nil, errors.New("routing requires a before_agent_run or after_agent_complete hook")
		}
		return edges, nil
	}
	if a.Routing == nil {
		return nil, errors.New("before_agent_run and after_agent_complete hooks require routing.allowed_agents")
	}
	if a.Harness != nil {
		return nil, errors.New("routing hooks are not supported on harness agents")
	}
	if len(a.Hooks.AfterAgentComplete) > 0 && a.ForceHandoff != "" {
		return nil, errors.New("hooks.after_agent_complete cannot be combined with force_handoff")
	}

	allowed := a.Routing.AllowedAgents
	if len(allowed) == 0 {
		return nil, errors.New("routing.allowed_agents must not be empty")
	}
	seen := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		switch {
		case seen[name]:
			return nil, fmt.Errorf("routing.allowed_agents lists %q more than once", name)
		case name == a.Name:
			return nil, fmt.Errorf("routing.allowed_agents must not include the agent itself (%q)", name)
		}
		seen[name] = true
		target, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("routing.allowed_agents references unknown local agent %q", name)
		}
		if target.Harness != nil {
			return nil, fmt.Errorf("routing.allowed_agents: harness agent %q cannot be a route target", name)
		}
	}
	if def := a.Routing.DefaultAgent; def != "" && !seen[def] {
		return nil, fmt.Errorf("routing.default_agent %q must be in routing.allowed_agents", def)
	}

	for _, hook := range selectors {
		targets, err := t.validateSelector(a, hook, seen)
		if err != nil {
			return nil, err
		}
		edges = append(edges, targets...)
	}
	return edges, nil
}

// validateSelector returns the agents a selector can route to.
func (t *Config) validateSelector(a *AgentConfig, hook HookDefinition, allowed map[string]bool) ([]string, error) {
	if hook.Type == "model" {
		return nil, errors.New("model hooks cannot select routes; use a command or choice evaluator")
	}
	if hook.Type != "evaluator" {
		if hook.RoutingPolicy != nil {
			return nil, errors.New("routing_policy requires type evaluator")
		}
		return slices.Sorted(maps.Keys(allowed)), nil
	}
	if hook.EvaluatorPolicy != nil {
		return nil, errors.New("evaluator hooks on before_agent_run and after_agent_complete use routing_policy, not evaluator_policy")
	}
	if a.Routing.DefaultAgent == "" {
		return nil, errors.New("routing.default_agent is required when an evaluator selects the route")
	}
	def, ok := t.Evaluators[hook.Evaluator]
	if !ok {
		return nil, fmt.Errorf("unknown evaluator %q", hook.Evaluator)
	}
	if def.Type != "choice" {
		return nil, fmt.Errorf("evaluator %q: routing requires a choice evaluator, got %q", hook.Evaluator, def.Type)
	}
	policy := hook.RoutingPolicy
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	for choice := range policy.Routes {
		if _, ok := def.Choices[choice]; !ok {
			return nil, fmt.Errorf("routing_policy.routes: unknown evaluator choice %q", choice)
		}
	}
	for choice := range def.Choices {
		if _, ok := policy.Routes[choice]; !ok {
			return nil, fmt.Errorf("routing_policy.routes: missing route for evaluator choice %q", choice)
		}
	}
	targets := []string{a.Routing.DefaultAgent}
	for choice, agent := range policy.Routes {
		if agent == a.Name {
			return nil, fmt.Errorf("routing_policy.routes: choice %q routes to the agent itself", choice)
		}
		if !allowed[agent] {
			return nil, fmt.Errorf("routing_policy.routes: choice %q routes to %q, which is not in routing.allowed_agents", choice, agent)
		}
		targets = append(targets, agent)
	}
	slices.Sort(targets)
	return slices.Compact(targets), nil
}

func rejectRoutingCycles(agents Agents, edges map[string][]string) error {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(edges))
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case visiting:
			return fmt.Errorf("routing cycle detected involving agent %q", name)
		case done:
			return nil
		}
		state[name] = visiting
		for _, next := range edges[name] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, a := range agents {
		if err := visit(a.Name); err != nil {
			return err
		}
	}
	return nil
}
