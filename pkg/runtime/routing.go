package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
)

const (
	// maxRouteTransitions bounds hook routes and forced handoffs in one
	// invocation, even after static cycle validation.
	maxRouteTransitions = 100
	// maxRoutingConversation bounds the prior visible messages sent to control hooks.
	maxRoutingConversation = 10

	agentSwitchKindRoute = "route"
)

// routeState is the per-invocation state of a hook-routed conversation. It
// lives on the run loop, never on shared agent definitions or evaluator clients.
type routeState struct {
	invocationID string
	input        string
	conversation []hooks.ConversationMessage
	// previousOutput is the last successfully completed step's answer.
	previousOutput string
	step           int
	transitions    int
	// activated names the agent whose before_agent_run has already fired for
	// the current activation.
	activated string
}

func newRouteState(sess *session.Session) *routeState {
	exchanges := sess.VisibleExchanges()
	state := &routeState{invocationID: uuid.NewV4().String()}
	if n := len(exchanges); n > 0 && exchanges[n-1].Role == chat.MessageRoleUser {
		state.input = exchanges[n-1].Content
		exchanges = exchanges[:n-1]
	}
	if len(exchanges) > maxRoutingConversation {
		exchanges = exchanges[len(exchanges)-maxRoutingConversation:]
	}
	for _, m := range exchanges {
		state.conversation = append(state.conversation, hooks.ConversationMessage{Role: string(m.Role), Content: m.Content})
	}
	return state
}

func (s *routeState) stepID() string { return "step-" + strconv.Itoa(s.step) }

func (s *routeState) countTransition() *routeStop {
	s.transitions++
	if s.transitions > maxRouteTransitions {
		return &routeStop{message: fmt.Sprintf("routing exceeded the limit of %d transitions in one invocation", maxRouteTransitions)}
	}
	return nil
}

// routeStop describes why routing ended the stream.
type routeStop struct {
	message string
	// hookBlocked is true when a control hook blocked or failed; otherwise the
	// runtime rejected the routing decision.
	hookBlocked bool
}

// routeOutcome is what completion routing decided for a finished agent.
type routeOutcome int

const (
	routeFinished routeOutcome = iota
	routeContinued
	routeStopped
)

// hasControlHooks reports whether a declares a before_agent_run or after_agent_complete hook.
func (r *LocalRuntime) hasControlHooks(a *agent.Agent) bool {
	exec := r.hooksExec(a)
	return exec != nil && (exec.Has(hooks.EventBeforeAgentRun) || exec.Has(hooks.EventAfterAgentComplete))
}

// beginRouting opts a root session into hook routing and restarts it at the
// shared entry agent. Routing never mutates the shared agent, so the entry the
// user selected survives every request. It returns nil for non-routed sessions.
func (r *LocalRuntime) beginRouting(sess *session.Session) *routeState {
	if sess.AgentName != "" {
		return nil
	}
	if !sess.Routed() && !r.hasControlHooks(r.agents.Current()) {
		return nil
	}
	sess.SetRouted()
	sess.SetRouteAgent("")
	return newRouteState(sess)
}

// restartRouting starts a new invocation at the entry agent for input that
// arrived while the previous one was running.
func (r *LocalRuntime) restartRouting(sess *session.Session, ls *loopState) {
	if ls.route == nil {
		return
	}
	sess.SetRouteAgent("")
	ls.route = newRouteState(sess)
}

// routingUnsupported rejects control hooks outside a routed root conversation
// rather than silently ignoring them.
func (r *LocalRuntime) routingUnsupported(route *routeState, a *agent.Agent) *routeStop {
	if route != nil || !r.hasControlHooks(a) {
		return nil
	}
	return &routeStop{message: fmt.Sprintf(
		"agent %q declares before_agent_run/after_agent_complete routing hooks, which are supported only in a root conversation whose entry agent declares routing hooks (pinned sub-sessions and handoffs from non-routed agents are unsupported)",
		a.Name())}
}

// activateAgent fires before_agent_run once for a new activation and follows
// route transitions, returning the agent that should actually run.
func (r *LocalRuntime) activateAgent(ctx context.Context, sess *session.Session, a *agent.Agent, route *routeState, events EventSink) (*agent.Agent, *routeStop) {
	for route.activated != a.Name() {
		route.step++
		route.activated = a.Name()

		result := r.dispatchControlHook(ctx, sess, a, hooks.EventBeforeAgentRun, route, "", events)
		next, stop := r.resolveTransition(a, result, route)
		if next == nil {
			r.recordUnroutedDecision(sess, a, route, string(hooks.EventBeforeAgentRun), result, stop, events)
		}
		if stop != nil {
			return a, stop
		}
		if next == nil {
			return a, nil
		}
		r.applyRoute(ctx, sess, a, next, route, string(hooks.EventBeforeAgentRun), result, false, events)
		a = next
	}
	return a, nil
}

// routeCompletion fires after_agent_complete, or applies the agent's forced
// handoff, for an agent that completed successfully with content.
func (r *LocalRuntime) routeCompletion(ctx context.Context, sess *session.Session, a *agent.Agent, content string, ls *loopState, events EventSink) (routeOutcome, *routeStop) {
	route := ls.route
	if strings.TrimSpace(content) == "" {
		return routeFinished, nil
	}

	var next *agent.Agent
	var result *hooks.Result
	phase := string(hooks.EventAfterAgentComplete)
	if exec := r.hooksExec(a); exec != nil && exec.Has(hooks.EventAfterAgentComplete) {
		result = r.dispatchControlHook(ctx, sess, a, hooks.EventAfterAgentComplete, route, content, events)
		var stop *routeStop
		next, stop = r.resolveTransition(a, result, route)
		if next == nil {
			r.recordUnroutedDecision(sess, a, route, phase, result, stop, events)
		}
		if stop != nil {
			return routeStopped, stop
		}
	} else if forced := a.ForceHandoff(); forced != nil {
		phase = agentSwitchKindForceHandoff
		if stop := route.countTransition(); stop != nil {
			return routeStopped, stop
		}
		next = forced
	}
	if next == nil {
		return routeFinished, nil
	}
	route.previousOutput = content
	r.applyRoute(ctx, sess, a, next, route, phase, result, true, events)
	return routeContinued, nil
}

// dispatchControlHook runs a control event with evaluator accounting attached,
// so routing assessments are charged like tool-dispatch ones.
func (r *LocalRuntime) dispatchControlHook(ctx context.Context, sess *session.Session, a *agent.Agent, event hooks.EventType, route *routeState, output string, events EventSink) *hooks.Result {
	r.ensureBudget()
	ctx = context.WithValue(ctx, evaluatorAccountingKey{}, &evaluatorAccounting{r: r, sess: sess, a: a, events: events})
	return r.dispatchHook(ctx, a, event, &hooks.Input{
		SessionID:      sess.ID,
		AgentName:      a.Name(),
		InvocationID:   route.invocationID,
		StepID:         route.stepID(),
		TaskInput:      route.input,
		PreviousOutput: route.previousOutput,
		Output:         output,
		Conversation:   route.conversation,
	}, events)
}

// resolveTransition authorizes a hook result against a's routing declaration.
// It returns the destination, nil when the hook asked for no route, or a stop.
func (r *LocalRuntime) resolveTransition(a *agent.Agent, result *hooks.Result, route *routeState) (*agent.Agent, *routeStop) {
	if result == nil {
		return nil, nil
	}
	if !result.Allowed {
		return nil, &routeStop{message: result.Message, hookBlocked: true}
	}
	if result.Transition == nil {
		return nil, nil
	}
	target := result.Transition.Agent
	if !a.Routing().Allows(target) {
		return nil, &routeStop{message: fmt.Sprintf("agent %q is not allowed to route to %q", a.Name(), target)}
	}
	next, err := r.team.Agent(target)
	if err != nil {
		return nil, &routeStop{message: fmt.Sprintf("routing target %q: %v", target, err)}
	}
	if stop := route.countTransition(); stop != nil {
		return nil, stop
	}
	return next, nil
}

// applyRoute switches the session to next. A completion route appends the same
// implicit handoff note as force_handoff so the next model call never starts
// on a dangling assistant message; an entry route replaces the activation and
// leaves the user's request as the last message.
func (r *LocalRuntime) applyRoute(ctx context.Context, sess *session.Session, from, next *agent.Agent, route *routeState, phase string, result *hooks.Result, afterCompletion bool, events EventSink) {
	kind := agentSwitchKindRoute
	if phase == agentSwitchKindForceHandoff {
		kind = agentSwitchKindForceHandoff
	}
	slog.InfoContext(ctx, "Routing conversation", "from_agent", from.Name(), "to_agent", next.Name(),
		"session_id", sess.ID, "invocation_id", route.invocationID, "step_id", route.stepID(), "phase", phase)

	r.executeOnAgentSwitchHooks(ctx, from, sess.ID, from.Name(), next.Name(), kind)
	r.switchSessionAgent(sess, next.Name())
	if afterCompletion {
		sess.AddMessage(session.ImplicitUserMessage(forcedHandoffNote(from.Name())))
	}

	event := &AgentRouteEvent{
		Type: "agent_route", SessionID: sess.ID, InvocationID: route.invocationID, StepID: route.stepID(),
		Phase: phase, FromAgent: from.Name(), ToAgent: next.Name(),
		AgentContext: newAgentContext(from.Name()),
	}
	action := hooks.TransitionActionRoute
	if phase == agentSwitchKindForceHandoff {
		action = agentSwitchKindForceHandoff
	}
	decision := r.recordRoutingDecision(sess, from.Name(), next.Name(), route, phase, action, "", result, events)
	event.Evaluator, event.Selected, event.Model = decision.Evaluator, decision.Selected, decision.Model
	event.Probability, event.FallbackReason = decision.Probability, decision.FallbackReason
	events.Emit(event)
	if event.FallbackReason != "" {
		events.Emit(Warning(fmt.Sprintf("Routing fell back to %q (%s).", next.Name(), event.FallbackReason), from.Name()))
	}
}

// Routing decision actions beyond route and force_handoff.
const (
	routingActionNone    = "none"
	routingActionBlocked = "blocked"
)

// recordRoutingDecision stores the decision as a session item, so it survives
// reloads and exports. toAgent is empty when no route was taken.
func (r *LocalRuntime) recordRoutingDecision(sess *session.Session, fromAgent, toAgent string, route *routeState, phase, action, reason string, result *hooks.Result, events EventSink) *session.RoutingDecision {
	decision := &session.RoutingDecision{
		ID: uuid.NewV4().String(), InvocationID: route.invocationID, StepID: route.stepID(),
		Phase: phase, FromAgent: fromAgent, ToAgent: toAgent, Action: action, Reason: reason, CreatedAt: r.now(),
	}
	if result != nil {
		decision.Evaluator = result.Metadata["evaluator"]
		decision.Selected = result.Metadata["evaluator_choice"]
		decision.Model = result.Metadata["evaluator_model"]
		decision.FallbackReason = result.Metadata["fallback_reason"]
		if p, err := strconv.ParseFloat(result.Metadata["evaluator_probability"], 64); err == nil {
			decision.Probability = &p
		}
	}
	sess.AddRoutingDecision(decision)
	events.Emit(&RoutingDecisionEvent{
		Type: "routing_decision", SessionID: sess.ID, Decision: decision,
		AgentContext: newAgentContext(fromAgent),
	})
	return decision
}

// recordUnroutedDecision records a control hook that ran but did not change the agent.
func (r *LocalRuntime) recordUnroutedDecision(sess *session.Session, a *agent.Agent, route *routeState, phase string, result *hooks.Result, stop *routeStop, events EventSink) {
	if result == nil {
		return
	}
	action, reason := routingActionNone, ""
	if stop != nil {
		action, reason = routingActionBlocked, stop.message
	}
	r.recordRoutingDecision(sess, a.Name(), "", route, phase, action, reason, result, events)
}

// switchSessionAgent makes name the active agent. Routed sessions switch only
// their own session; others keep the shared-agent behavior handoffs always had.
func (r *LocalRuntime) switchSessionAgent(sess *session.Session, name string) {
	if sess.Routed() {
		sess.SetRouteAgent(name)
		return
	}
	if !sess.TryAgentHandoff(name) && sess.AgentName == "" {
		r.setCurrentAgent(name)
	}
}

// stopRouting reports a routing stop to the user and classifies the stream exit.
// Budget exhaustion and cancellation take precedence: they are terminal, not hook failures.
func (r *LocalRuntime) stopRouting(ctx context.Context, sess *session.Session, a *agent.Agent, stop *routeStop, events EventSink) string {
	switch {
	case ctx.Err() != nil:
		return turnEndReasonCanceled
	case r.enforceBudget(ctx, sess, a, events) == iterationStop:
		return turnEndReasonBudgetExceeded
	case stop.hookBlocked:
		r.emitHookDrivenShutdown(ctx, a, sess, stop.message, events)
		return turnEndReasonHookBlocked
	}
	events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeRoutingFailed, stop.message))
	r.notifyError(ctx, a, sess.ID, stop.message)
	return turnEndReasonError
}

// routingDefaultAgent returns the agent's configured fallback route.
func (r *LocalRuntime) routingDefaultAgent(agentName string) string {
	a, err := r.team.Agent(agentName)
	if err != nil {
		return ""
	}
	return a.Routing().DefaultAgent
}

// AgentRouteEvent reports a routing transition without the task text.
type AgentRouteEvent struct {
	AgentContext

	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	InvocationID string `json:"invocation_id"`
	StepID       string `json:"step_id"`
	// Phase is before_agent_run, after_agent_complete, or force_handoff.
	Phase     string `json:"phase"`
	FromAgent string `json:"from_agent"`
	ToAgent   string `json:"to_agent"`
	// Evaluator fields are empty for command selectors and forced handoffs.
	Evaluator      string   `json:"evaluator,omitempty"`
	Selected       string   `json:"selected,omitempty"`
	Probability    *float64 `json:"probability,omitempty"`
	Model          string   `json:"model,omitempty"`
	FallbackReason string   `json:"fallback_reason,omitempty"`
}

func (e *AgentRouteEvent) GetSessionID() string { return e.SessionID }

// RoutingDecisionEvent reports a persisted control decision, including those
// that left the active agent unchanged.
type RoutingDecisionEvent struct {
	AgentContext

	Type      string                   `json:"type"`
	SessionID string                   `json:"session_id"`
	Decision  *session.RoutingDecision `json:"decision"`
}

func (e *RoutingDecisionEvent) GetSessionID() string { return e.SessionID }
