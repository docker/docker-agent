package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
)

// EvaluatorOption customizes [NewEvaluatorFactory].
type EvaluatorOption func(*evaluatorFactory)

// WithRoutingDefaults supplies each agent's configured fallback route, used
// when an evaluator assessment is uncertain or fails on a control event.
func WithRoutingDefaults(defaultAgent func(agentName string) string) EvaluatorOption {
	return func(f *evaluatorFactory) { f.defaultAgent = defaultAgent }
}

type evaluatorFactory struct {
	lookup       func(agentName, evaluatorName string) (evaluator.Evaluator, bool)
	defaultAgent func(agentName string) string
}

// NewEvaluatorFactory returns an evaluator hook factory backed by named
// evaluators: tool_guard hooks apply an evaluator policy, control-event hooks
// apply a routing policy. lookup resolves (agent name, evaluator name) when
// the hook runs. Register it under [HookTypeEvaluator] in the runtime's hook registry.
func NewEvaluatorFactory(lookup func(agentName, evaluatorName string) (evaluator.Evaluator, bool), opts ...EvaluatorOption) HandlerFactory {
	f := &evaluatorFactory{lookup: lookup}
	for _, opt := range opts {
		opt(f)
	}
	return f.build
}

func (f *evaluatorFactory) build(_ HandlerEnv, hook Hook) (Handler, error) {
	if strings.TrimSpace(hook.Evaluator) == "" {
		return nil, errors.New("evaluator hook requires a non-empty evaluator reference")
	}
	if f.lookup == nil {
		return nil, errors.New("evaluator hook: no evaluator lookup configured")
	}
	if hook.RoutingPolicy != nil {
		if hook.EvaluatorPolicy != nil {
			return nil, errors.New("evaluator hook: evaluator_policy and routing_policy are mutually exclusive")
		}
		if err := hook.RoutingPolicy.Validate(); err != nil {
			return nil, fmt.Errorf("evaluator hook: %w", err)
		}
		policy := *hook.RoutingPolicy
		policy.Routes = maps.Clone(policy.Routes)
		return &routingEvaluatorHandler{name: hook.Evaluator, lookup: f.lookup, defaultAgent: f.defaultAgent, policy: policy}, nil
	}
	if err := hook.EvaluatorPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("evaluator hook: %w", err)
	}
	policy := *hook.EvaluatorPolicy
	policy.Decisions = maps.Clone(policy.Decisions)
	return &evaluatorHandler{name: hook.Evaluator, lookup: f.lookup, policy: policy}, nil
}

type evaluatorHandler struct {
	name   string
	lookup func(agentName, evaluatorName string) (evaluator.Evaluator, bool)
	policy latest.EvaluatorPolicy
}

func (h *evaluatorHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("decode hook input: %w", err)
	}
	if in.HookEventName != EventToolGuard {
		return HandlerResult{ExitCode: -1}, errors.New("evaluator hooks are only supported on tool_guard")
	}
	client, ok := h.lookup(in.AgentName, h.name)
	if !ok || client == nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("evaluator hook: unknown evaluator %q for agent %q", h.name, in.AgentName)
	}
	// Do not send session identifiers, history, or unrelated hook fields.
	result, err := client.Evaluate(ctx, map[string]any{
		"tool_name":     in.ToolName,
		"tool_input":    in.ToolInput,
		"tool_category": in.ToolCategory,
	})
	if err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("evaluator %q: %w", h.name, err)
	}
	choice, probability, err := evaluatorOutcome(result)
	if err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("evaluator %q: %w", h.name, err)
	}

	decision := h.policy.Fallback
	reason := "Evaluator policy fallback applied."
	if mapped, ok := h.policy.Decisions[choice]; ok && probability >= h.policy.MinProbability {
		decision = mapped
		reason = "Evaluator policy matched the selected outcome."
	}
	metadata := map[string]string{
		"evaluator":             h.name,
		"evaluator_type":        result.Type,
		"evaluator_choice":      choice,
		"evaluator_probability": strconv.FormatFloat(probability, 'g', -1, 64),
	}
	if result.Model != "" {
		metadata["evaluator_model"] = result.Model
	}
	return HandlerResult{Output: &Output{HookSpecificOutput: &HookSpecificOutput{
		HookEventName:            EventToolGuard,
		PermissionDecision:       Decision(decision),
		PermissionDecisionReason: reason,
		Metadata:                 metadata,
	}}}, nil
}

func evaluatorOutcome(result *evaluator.Result) (string, float64, error) {
	if result == nil {
		return "", 0, errors.New("missing evaluator result")
	}
	var choice string
	var probability float64
	switch result.Type {
	case "boolean":
		if result.Probability == nil {
			return "", 0, errors.New("missing boolean probability")
		}
		probability = *result.Probability
	case "choice":
		if strings.TrimSpace(result.Choice) == "" {
			return "", 0, errors.New("missing evaluator choice")
		}
		choice = result.Choice
		var ok bool
		probability, ok = result.Probabilities[choice]
		if !ok {
			return "", 0, errors.New("missing selected choice probability")
		}
	default:
		return "", 0, errors.New("tool guards require a boolean or choice evaluator result")
	}
	if math.IsNaN(probability) || probability < 0 || probability > 1 {
		return "", 0, errors.New("invalid evaluator probability")
	}
	if result.Type == "boolean" {
		choice = strconv.FormatBool(probability >= 0.5)
		if probability < 0.5 {
			probability = 1 - probability
		}
	}
	return choice, probability, nil
}

func sameRoutingPolicy(a, b *latest.RoutingPolicy) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.MinProbability == b.MinProbability && maps.Equal(a.Routes, b.Routes)
}

func sameEvaluatorPolicy(a, b *latest.EvaluatorPolicy) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.MinProbability == b.MinProbability && a.Fallback == b.Fallback && maps.Equal(a.Decisions, b.Decisions)
}
