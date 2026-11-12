package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
)

// Fallback reasons reported in route metadata. They are categories, never
// provider error text, so events cannot leak credentials or task content.
const (
	FallbackEvaluatorFailed = "evaluator_failed"
	FallbackInvalidResult   = "invalid_result"
	FallbackUnknownChoice   = "unknown_choice"
	FallbackTie             = "tie"
	FallbackBelowThreshold  = "below_threshold"
)

const probabilitySumTolerance = 1e-3

// routingEvaluatorHandler turns a choice assessment into a route transition.
type routingEvaluatorHandler struct {
	name         string
	lookup       func(agentName, evaluatorName string) (evaluator.Evaluator, bool)
	defaultAgent func(agentName string) string
	policy       latest.RoutingPolicy
}

func (h *routingEvaluatorHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("decode hook input: %w", err)
	}
	if !EventContract(in.HookEventName).Control {
		return HandlerResult{ExitCode: -1}, errors.New("routing evaluator hooks are only supported on before_agent_run and after_agent_complete")
	}
	if h.defaultAgent == nil || h.defaultAgent(in.AgentName) == "" {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("evaluator hook: agent %q has no routing.default_agent", in.AgentName)
	}
	client, ok := h.lookup(in.AgentName, h.name)
	if !ok || client == nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("evaluator hook: unknown evaluator %q for agent %q", h.name, in.AgentName)
	}

	result, err := client.Evaluate(ctx, assessmentState(&in))
	if err != nil {
		// Cancellation, budgets and invalid accounting end the run; only
		// ordinary provider failures may fall back to the default route.
		if ctx.Err() != nil || evaluator.IsTerminal(err) {
			return HandlerResult{ExitCode: -1}, fmt.Errorf("evaluator %q: %w", h.name, err)
		}
		slog.DebugContext(ctx, "Routing evaluator failed; using default route", "evaluator", h.name, "error", err)
		result = nil
	}

	decision := h.decide(result, err != nil)
	if decision.fallback != "" {
		decision.agent = h.defaultAgent(in.AgentName)
		slog.WarnContext(ctx, "Routing evaluator fell back to the default agent",
			"evaluator", h.name, "agent", in.AgentName, "default_agent", decision.agent, "reason", decision.fallback)
	}
	return HandlerResult{Output: &Output{HookSpecificOutput: &HookSpecificOutput{
		HookEventName: in.HookEventName,
		Transition:    &Transition{Action: TransitionActionRoute, Agent: decision.agent},
		Metadata:      h.metadata(decision, result),
	}}}, nil
}

// assessmentState projects the task context the evaluator may see: no IDs,
// system messages, or tool transcripts.
func assessmentState(in *Input) map[string]any {
	state := map[string]any{"input": in.TaskInput}
	if in.PreviousOutput != "" {
		state["previous_output"] = in.PreviousOutput
	}
	if in.Output != "" {
		state["output"] = in.Output
	}
	if len(in.Conversation) > 0 {
		state["conversation"] = in.Conversation
	}
	return state
}

type routeDecision struct {
	agent       string
	selected    string
	probability *float64
	fallback    string
}

// decide validates the complete probability distribution against the policy
// and returns the mapped agent, or a fallback reason when the answer cannot be
// trusted. failed reports that no assessment was obtained.
func (h *routingEvaluatorHandler) decide(result *evaluator.Result, failed bool) routeDecision {
	switch {
	case failed:
		return routeDecision{fallback: FallbackEvaluatorFailed}
	case result == nil || result.Type != "choice":
		return routeDecision{fallback: FallbackInvalidResult}
	}

	d := routeDecision{selected: result.Choice}
	if len(result.Probabilities) != len(h.policy.Routes) {
		d.fallback = FallbackInvalidResult
		return d
	}
	var total float64
	for choice := range h.policy.Routes {
		p, present := result.Probabilities[choice]
		if !present || math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			d.fallback = FallbackInvalidResult
			return d
		}
		total += p
	}
	if math.Abs(total-1) > probabilitySumTolerance {
		d.fallback = FallbackInvalidResult
		return d
	}
	target, known := h.policy.Routes[result.Choice]
	if !known {
		d.fallback = FallbackUnknownChoice
		return d
	}
	p := result.Probabilities[result.Choice]
	d.probability = &p
	for choice, other := range result.Probabilities {
		if choice != result.Choice && other >= p {
			d.fallback = FallbackTie
			return d
		}
	}
	if p < h.policy.MinProbability {
		d.fallback = FallbackBelowThreshold
		return d
	}
	d.agent = target
	return d
}

func (h *routingEvaluatorHandler) metadata(d routeDecision, result *evaluator.Result) map[string]string {
	metadata := map[string]string{"evaluator": h.name}
	if d.selected != "" {
		metadata["evaluator_choice"] = d.selected
	}
	if d.probability != nil {
		metadata["evaluator_probability"] = strconv.FormatFloat(*d.probability, 'g', -1, 64)
	}
	if result != nil {
		if result.Type != "" {
			metadata["evaluator_type"] = result.Type
		}
		if strings.TrimSpace(result.Model) != "" {
			metadata["evaluator_model"] = result.Model
		}
	}
	if d.fallback != "" {
		metadata["fallback_reason"] = d.fallback
	}
	return metadata
}
