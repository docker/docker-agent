package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

// scriptedProvider answers every call with a fixed reply and counts calls.
type scriptedProvider struct {
	reply string
	// stream, when set, replaces reply for every call (e.g. a structured-output tool call).
	stream func() chat.MessageStream
	calls  atomic.Int32
	mu     sync.Mutex
	seen   [][]chat.Message
}

func (p *scriptedProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/mock-model") }

func (p *scriptedProvider) CreateChatCompletionStream(_ context.Context, msgs []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.seen = append(p.seen, slices.Clone(msgs))
	p.mu.Unlock()
	if p.stream != nil {
		return p.stream(), nil
	}
	return newStreamBuilder().AddContent(p.reply).AddStopWithUsage(1, 1).Build(), nil
}

func (p *scriptedProvider) BaseConfig() base.Config { return base.Config{} }
func (p *scriptedProvider) MaxTokens() int          { return 0 }

func (p *scriptedProvider) lastPrompt() []chat.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		return nil
	}
	return p.seen[len(p.seen)-1]
}

// scriptedEvaluator returns queued answers and records the assessed state.
type scriptedEvaluator struct {
	mu      sync.Mutex
	answers []scriptedAnswer
	states  []any
	calls   atomic.Int32
}

type scriptedAnswer struct {
	result *evaluator.Result
	err    error
}

func (e *scriptedEvaluator) Evaluate(_ context.Context, state any) (*evaluator.Result, error) {
	e.calls.Add(1)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.states = append(e.states, state)
	if len(e.answers) == 0 {
		return nil, errors.New("no scripted answer")
	}
	a := e.answers[0]
	if len(e.answers) > 1 {
		e.answers = e.answers[1:]
	}
	return a.result, a.err
}

func choiceResult(choice string, probabilities map[string]float64) *evaluator.Result {
	cost := 0.001
	return &evaluator.Result{
		Type: "choice", Model: "judge-1", Choice: choice, Probabilities: probabilities,
		Usage: evaluator.Usage{InputTokens: 10, OutputTokens: 2}, Cost: &cost,
	}
}

var defaultProbabilities = func(winner string, p float64) map[string]float64 {
	out := map[string]float64{"simple": 0, "complex": 0, "unclear": 0}
	rest := (1 - p) / 2
	for k := range out {
		out[k] = rest
	}
	out[winner] = p
	return out
}

type routingFixture struct {
	rt        *LocalRuntime
	router    *scriptedProvider
	providers map[string]*scriptedProvider
	eval      *scriptedEvaluator
}

func evaluatorRoutingHooks(event string, minProbability float64) *latest.HooksConfig {
	hook := latest.HookDefinition{
		Type: hooks.HookTypeEvaluator, Evaluator: "task_route",
		RoutingPolicy: &latest.RoutingPolicy{
			Routes:         map[string]string{"simple": "quick", "complex": "specialist", "unclear": "clarifier"},
			MinProbability: minProbability,
		},
	}
	if event == latest.EventAfterAgentComplete {
		return &latest.HooksConfig{AfterAgentComplete: latest.HookDefinitions{hook}}
	}
	return &latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{hook}}
}

func newEvaluatorRoutingFixture(t *testing.T, answers ...scriptedAnswer) *routingFixture {
	t.Helper()
	return newRoutingFixtureFor(t, latest.EventBeforeAgentRun, nil, answers...)
}

// newRoutingFixtureFor wires root's evaluator selector on the given control event.
func newRoutingFixtureFor(t *testing.T, event string, opts []Opt, answers ...scriptedAnswer) *routingFixture {
	t.Helper()
	f := &routingFixture{
		router:    &scriptedProvider{reply: "router must not answer"},
		providers: map[string]*scriptedProvider{},
		eval:      &scriptedEvaluator{answers: answers},
	}
	var agents []*agent.Agent
	for _, name := range []string{"quick", "specialist", "clarifier"} {
		f.providers[name] = &scriptedProvider{reply: name + " answer"}
		agents = append(agents, agent.New(name, name+" instructions", agent.WithModel(f.providers[name])))
	}
	root := agent.New("root", "root instructions", agent.WithModel(f.router),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"quick", "specialist", "clarifier"}, DefaultAgent: "clarifier"}),
		agent.WithHooks(evaluatorRoutingHooks(event, 0.85)))
	tm := team.New(team.WithAgents(append([]*agent.Agent{root}, agents...)...),
		team.WithEvaluators(map[string]evaluator.Evaluator{"task_route": f.eval}))
	rt, err := NewLocalRuntime(t.Context(), tm, append([]Opt{WithSessionCompaction(false), WithModelStore(mockModelStore{})}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, rt.Close()) })
	f.rt = rt
	return f
}

func runRouted(t *testing.T, rt *LocalRuntime, sess *session.Session) []Event {
	t.Helper()
	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}
	return events
}

func routeEvents(events []Event) []*AgentRouteEvent {
	var out []*AgentRouteEvent
	for _, ev := range events {
		if r, ok := ev.(*AgentRouteEvent); ok {
			out = append(out, r)
		}
	}
	return out
}

func routedErrors(events []Event) []*ErrorEvent {
	var out []*ErrorEvent
	for _, ev := range events {
		if e, ok := ev.(*ErrorEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

func TestRouting_EvaluatorSelectsAgentWithoutRouterModelCall(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.9))})

	sess := session.New(session.WithUserMessage("Diagnose this deadlock"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.Zero(t, f.router.calls.Load(), "a routed-away agent must not call its model")
	assert.EqualValues(t, 1, f.providers["specialist"].calls.Load())
	assert.EqualValues(t, 1, f.eval.calls.Load(), "one selector call per activation")
	assert.Equal(t, "specialist answer", sess.GetLastAssistantMessageContent())

	routes := routeEvents(events)
	require.Len(t, routes, 1)
	assert.Equal(t, "root", routes[0].FromAgent)
	assert.Equal(t, "specialist", routes[0].ToAgent)
	assert.Equal(t, "before_agent_run", routes[0].Phase)
	assert.Equal(t, "task_route", routes[0].Evaluator)
	assert.Equal(t, "complex", routes[0].Selected)
	assert.Equal(t, "judge-1", routes[0].Model)
	require.NotNil(t, routes[0].Probability)
	assert.InDelta(t, 0.9, *routes[0].Probability, 1e-9)
	assert.Empty(t, routes[0].FallbackReason)

	assert.Equal(t, "root", f.rt.CurrentAgentName(t.Context()), "routing must not mutate the shared entry agent")
	assert.Equal(t, "specialist", sess.RouteAgent())
	var charges int
	for _, ev := range events {
		if _, ok := ev.(*EvaluationUsageEvent); ok {
			charges++
		}
	}
	assert.Equal(t, 1, charges, "the selector call is charged once")
	assert.InDelta(t, 0.001, sess.TotalCost(), 1e-9)
}

func TestRouting_EvaluatorSeesOnlyTheMinimalProjection(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: choiceResult("simple", defaultProbabilities("simple", 0.95))})

	sess := session.New(session.WithUserMessage("What does EXPOSE do?"), session.WithNonInteractive(true))
	sess.AddMessage(session.ImplicitUserMessage("internal handoff note"))
	runRouted(t, f.rt, sess)

	require.Len(t, f.eval.states, 1)
	state, ok := f.eval.states[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"input": "What does EXPOSE do?"}, state)
}

func TestRouting_FollowUpRequestKeepsVisibleConversationOnly(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t,
		scriptedAnswer{result: choiceResult("simple", defaultProbabilities("simple", 0.95))},
		scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.95))},
	)

	sess := session.New(session.WithUserMessage("What does EXPOSE do?"), session.WithNonInteractive(true))
	runRouted(t, f.rt, sess)
	assert.Equal(t, "quick", sess.RouteAgent())

	sess.AddMessage(session.UserMessage("Now design a cluster for it"))
	events := runRouted(t, f.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 2, f.eval.calls.Load(), "a new request reruns entry selection")
	assert.Zero(t, f.router.calls.Load())
	assert.EqualValues(t, 1, f.providers["specialist"].calls.Load())
	assert.Equal(t, "root", f.rt.CurrentAgentName(t.Context()))

	state, ok := f.eval.states[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Now design a cluster for it", state["input"])
	assert.Equal(t, []hooks.ConversationMessage{
		{Role: "user", Content: "What does EXPOSE do?"},
		{Role: "assistant", Content: "quick answer"},
	}, state["conversation"])
}

func TestRouting_EvaluatorFallbacks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer scriptedAnswer
		reason string
	}{
		{"below threshold", scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.5))}, hooks.FallbackBelowThreshold},
		{"tie", scriptedAnswer{result: choiceResult("complex", map[string]float64{"simple": 0.45, "complex": 0.45, "unclear": 0.1})}, hooks.FallbackTie},
		{"unknown choice", scriptedAnswer{result: choiceResult("other", map[string]float64{"simple": 0.05, "complex": 0.05, "unclear": 0.9})}, hooks.FallbackUnknownChoice},
		{"extra probability", scriptedAnswer{result: choiceResult("complex", map[string]float64{"simple": 0, "complex": 0.9, "unclear": 0.05, "x": 0.05})}, hooks.FallbackInvalidResult},
		{"invalid sum", scriptedAnswer{result: choiceResult("complex", map[string]float64{"simple": 0.1, "complex": 0.5, "unclear": 0.1})}, hooks.FallbackInvalidResult},
		{"wrong type", scriptedAnswer{result: &evaluator.Result{Type: "boolean"}}, hooks.FallbackInvalidResult},
		{"provider failure", scriptedAnswer{err: errors.New("503 from judge, secret-token")}, hooks.FallbackEvaluatorFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newEvaluatorRoutingFixture(t, tt.answer)

			sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
			events := runRouted(t, f.rt, sess)

			assert.Empty(t, routedErrors(events))
			assert.Zero(t, f.router.calls.Load())
			assert.EqualValues(t, 1, f.providers["clarifier"].calls.Load(), "fallback must use the default agent")
			routes := routeEvents(events)
			require.Len(t, routes, 1)
			assert.Equal(t, tt.reason, routes[0].FallbackReason)
			for _, ev := range events {
				if w, ok := ev.(*WarningEvent); ok {
					assert.NotContains(t, w.Message, "secret-token")
				}
			}
		})
	}
}

func TestRouting_TerminalEvaluatorErrorsNeverFallBack(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{err: &evaluator.TerminalError{Err: errors.New("budget exceeded")}})

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	require.Len(t, routedErrors(events), 1)
	assert.Equal(t, ErrorCodeHookBlocked, routedErrors(events)[0].Code)
	assert.Empty(t, routeEvents(events))
	for _, p := range f.providers {
		assert.Zero(t, p.calls.Load(), "no agent may run after a terminal evaluator error")
	}
	assert.Zero(t, f.router.calls.Load())
}

func TestRouting_CancelledRunNeverFallsBack(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{err: context.Canceled})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	for range f.rt.RunStream(ctx, sess) {
	}

	for _, p := range f.providers {
		assert.Zero(t, p.calls.Load())
	}
	assert.Zero(t, f.router.calls.Load())
}

// selectorTeam builds root/worker/reviewer agents whose routing is driven by
// in-process builtin selectors, with no evaluator involved.
type selectorTeam struct {
	rt        *LocalRuntime
	providers map[string]*scriptedProvider
	mu        sync.Mutex
	inputs    []hooks.Input
}

func (st *selectorTeam) recorded() []hooks.Input {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.inputs)
}

func newSelectorTeam(t *testing.T, selectors map[string]hooks.BuiltinFunc, configure func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt)) *selectorTeam {
	t.Helper()
	st := &selectorTeam{providers: map[string]*scriptedProvider{}}
	registry := hooks.NewRegistry()
	for name, fn := range selectors {
		require.NoError(t, registry.RegisterBuiltin(name, func(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
			st.mu.Lock()
			st.inputs = append(st.inputs, *in)
			st.mu.Unlock()
			return fn(ctx, in, args)
		}))
	}
	var agents []*agent.Agent
	for _, name := range []string{"root", "worker", "reviewer"} {
		st.providers[name] = &scriptedProvider{reply: name + " answer"}
		opts := []agent.Opt{agent.WithModel(st.providers[name])}
		cfg := &latest.HooksConfig{}
		configure(name, cfg, &opts)
		if !cfg.IsEmpty() {
			opts = append(opts, agent.WithHooks(cfg))
		}
		agents = append(agents, agent.New(name, name+" instructions", opts...))
	}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agents...)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithHooksRegistry(registry))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, rt.Close()) })
	st.rt = rt
	return st
}

func routeTo(agentName string) hooks.BuiltinFunc {
	return func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
			Transition: &hooks.Transition{Action: hooks.TransitionActionRoute, Agent: agentName},
		}}, nil
	}
}

func noRoute(context.Context, *hooks.Input, []string) (*hooks.Output, error) { return nil, nil }

func selectorHook(name string) latest.HookDefinitions {
	return latest.HookDefinitions{{Type: hooks.HookTypeBuiltin, Command: name}}
}

func TestRouting_CompletionRouteContinuesInSharedConversation(t *testing.T) {
	t.Parallel()
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"review": routeTo("reviewer")},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "worker" {
				cfg.AfterAgentComplete = selectorHook("review")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}}))
			}
		})
	require.NoError(t, st.rt.SetCurrentAgent(t.Context(), "worker"))

	sess := session.New(session.WithUserMessage("research this"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 1, st.providers["worker"].calls.Load())
	assert.EqualValues(t, 1, st.providers["reviewer"].calls.Load())
	assert.Equal(t, "reviewer answer", sess.GetLastAssistantMessageContent())
	assert.Equal(t, "worker", st.rt.CurrentAgentName(t.Context()), "the explicit entry agent, not root, is remembered")

	// The destination sees the previous transcript (shared history).
	var sawWorker bool
	for _, m := range st.providers["reviewer"].lastPrompt() {
		sawWorker = sawWorker || (m.Role == chat.MessageRoleAssistant && m.Content == "worker answer")
	}
	assert.True(t, sawWorker)

	var completion *hooks.Input
	for _, in := range st.recorded() {
		if in.HookEventName == hooks.EventAfterAgentComplete {
			completion = &in
			break
		}
	}
	require.NotNil(t, completion)
	assert.Equal(t, hooks.EventAfterAgentComplete, completion.HookEventName)
	assert.Equal(t, "research this", completion.TaskInput, "original input is preserved")
	assert.Equal(t, "worker answer", completion.Output)
	assert.Empty(t, completion.PreviousOutput)
	assert.NotEmpty(t, completion.InvocationID)
	assert.Equal(t, "step-1", completion.StepID)

	// A follow-up restarts at the entry agent.
	sess.AddMessage(session.UserMessage("again"))
	runRouted(t, st.rt, sess)
	assert.EqualValues(t, 2, st.providers["worker"].calls.Load())
}

func TestRouting_ForcedHandoffInRoutedSessionIsSessionLocal(t *testing.T) {
	t.Parallel()
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"enter": noRoute},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "root" {
				cfg.BeforeAgentRun = selectorHook("enter")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker"}}))
			}
		})
	root, err := st.rt.team.Agent("root")
	require.NoError(t, err)
	worker, err := st.rt.team.Agent("worker")
	require.NoError(t, err)
	agent.WithForceHandoff(worker)(root)

	sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 1, st.providers["root"].calls.Load())
	assert.EqualValues(t, 1, st.providers["worker"].calls.Load())
	assert.Equal(t, "root", st.rt.CurrentAgentName(t.Context()), "forced handoff must not mutate the shared agent")
	routes := routeEvents(events)
	require.Len(t, routes, 1)
	assert.Equal(t, agentSwitchKindForceHandoff, routes[0].Phase)
}

func TestRouting_ControlHooksFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		selector hooks.BuiltinFunc
		code     string
		message  string
	}{
		{"disallowed target", routeTo("reviewer"), ErrorCodeRoutingFailed, "not allowed to route to"},
		{"unknown target", routeTo("ghost"), ErrorCodeRoutingFailed, "not allowed to route to"},
		{"unknown action", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{Transition: &hooks.Transition{Action: "retry", Agent: "worker"}}}, nil
		}, ErrorCodeHookBlocked, "invalid transition action"},
		{"missing target", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{Transition: &hooks.Transition{Action: "route"}}}, nil
		}, ErrorCodeHookBlocked, "requires an agent"},
		{"hook crash", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			return nil, errors.New("selector crashed")
		}, ErrorCodeHookBlocked, "selector crashed"},
		{"block beats route", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			return &hooks.Output{
				Decision: hooks.DecisionBlockValue, Reason: "policy says no",
				HookSpecificOutput: &hooks.HookSpecificOutput{Transition: &hooks.Transition{Action: "route", Agent: "worker"}},
			}, nil
		}, ErrorCodeHookBlocked, "policy says no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"select": tt.selector},
				func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
					if name == "root" {
						cfg.BeforeAgentRun = selectorHook("select")
						*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker"}}))
					}
				})

			sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true))
			events := runRouted(t, st.rt, sess)

			errs := routedErrors(events)
			require.Len(t, errs, 1)
			assert.Equal(t, tt.code, errs[0].Code)
			assert.Contains(t, errs[0].Error, tt.message)
			assert.Empty(t, routeEvents(events))
			for name, p := range st.providers {
				assert.Zero(t, p.calls.Load(), "%s must not run after a failed routing decision", name)
			}
		})
	}
}

func TestRouting_CompletionHookNotFiredOnFailureOrEmptyCompletion(t *testing.T) {
	t.Parallel()
	var fired atomic.Int32
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"review": func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		fired.Add(1)
		return routeTo("reviewer")(nil, nil, nil)
	}}, func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
		if name == "root" {
			cfg.AfterAgentComplete = selectorHook("review")
			*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}}))
		}
	})

	st.providers["root"].reply = ""
	sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true))
	runRouted(t, st.rt, sess)
	assert.Zero(t, fired.Load(), "an empty completion must not continue")
	assert.Zero(t, st.providers["reviewer"].calls.Load())
}

func TestRouting_TransitionLimitStopsRunawayRoutes(t *testing.T) {
	t.Parallel()
	// Validation rejects cycles; the runtime bound is the backstop for configs built in code.
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"to_worker": routeTo("worker"), "to_root": routeTo("root")},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			switch name {
			case "root":
				cfg.AfterAgentComplete = selectorHook("to_worker")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker"}}))
			case "worker":
				cfg.AfterAgentComplete = selectorHook("to_root")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"root"}}))
			}
		})

	sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true), session.WithMaxIterations(500))
	events := runRouted(t, st.rt, sess)

	errs := routedErrors(events)
	require.Len(t, errs, 1)
	assert.Equal(t, ErrorCodeRoutingFailed, errs[0].Code)
	assert.Contains(t, errs[0].Error, "limit of 100 transitions")
	total := st.providers["root"].calls.Load() + st.providers["worker"].calls.Load()
	assert.EqualValues(t, maxRouteTransitions+1, total)
}

func TestRouting_PinnedSessionRejectsControlHooks(t *testing.T) {
	t.Parallel()
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"select": routeTo("worker")},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "root" {
				cfg.BeforeAgentRun = selectorHook("select")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker"}}))
			}
		})

	sess := session.New(session.WithUserMessage("go"), session.WithAgentName("root"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	errs := routedErrors(events)
	require.Len(t, errs, 1)
	assert.Equal(t, ErrorCodeRoutingFailed, errs[0].Code)
	assert.Contains(t, errs[0].Error, "pinned sub-sessions")
	assert.Zero(t, st.providers["root"].calls.Load())
	assert.Zero(t, st.providers["worker"].calls.Load())
}

func TestRouting_ConcurrentSessionsRouteIndependently(t *testing.T) {
	t.Parallel()
	byInput := func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		target := "worker"
		if in.TaskInput == "second" {
			target = "reviewer"
		}
		return routeTo(target)(nil, nil, nil)
	}
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"select": byInput},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "root" {
				cfg.BeforeAgentRun = selectorHook("select")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker", "reviewer"}}))
			}
		})

	first := session.New(session.WithUserMessage("first"), session.WithNonInteractive(true))
	second := session.New(session.WithUserMessage("second"), session.WithNonInteractive(true))
	var wg sync.WaitGroup
	for _, sess := range []*session.Session{first, second} {
		wg.Go(func() { runRouted(t, st.rt, sess) })
	}
	wg.Wait()

	assert.Equal(t, "worker answer", first.GetLastAssistantMessageContent())
	assert.Equal(t, "reviewer answer", second.GetLastAssistantMessageContent())
	assert.Equal(t, "root", st.rt.CurrentAgentName(t.Context()))
}

func TestRouting_InputQueuedWhileBusyStartsNewInvocationAtBoundary(t *testing.T) {
	t.Parallel()
	byInput := func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		target := "worker"
		if in.TaskInput == "queued" {
			target = "reviewer"
		}
		return routeTo(target)(nil, nil, nil)
	}
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"select": byInput},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "root" {
				cfg.BeforeAgentRun = selectorHook("select")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker", "reviewer"}}))
			}
		})
	require.NoError(t, st.rt.Steer(t.Context(), QueuedMessage{Content: "queued"}))

	sess := session.New(session.WithUserMessage("first"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 1, st.providers["worker"].calls.Load(), "queued input must not interrupt the first invocation")
	assert.EqualValues(t, 1, st.providers["reviewer"].calls.Load(), "queued input starts a new invocation at the entry agent")
	assert.Zero(t, st.providers["root"].calls.Load())
	assert.Equal(t, "reviewer answer", sess.GetLastAssistantMessageContent())

	var queuedSelection hooks.Input
	for _, in := range st.recorded() {
		if in.TaskInput == "queued" {
			queuedSelection = in
		}
	}
	assert.Equal(t, "queued", queuedSelection.TaskInput)
	assert.Equal(t, []hooks.ConversationMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "worker answer"},
	}, queuedSelection.Conversation)
}

func TestRouting_CachedCompletionFollowsCompletionRouting(t *testing.T) {
	t.Parallel()
	c, err := cache.New(cache.Config{Enabled: true})
	require.NoError(t, err)
	c.Store("research this", "cached worker answer")

	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"review": routeTo("reviewer")},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "worker" {
				cfg.AfterAgentComplete = selectorHook("review")
				*opts = append(*opts, agent.WithCache(c), agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}}))
			}
		})
	require.NoError(t, st.rt.SetCurrentAgent(t.Context(), "worker"))

	sess := session.New(session.WithUserMessage("research this"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.Zero(t, st.providers["worker"].calls.Load(), "the cache answered")
	assert.EqualValues(t, 1, st.providers["reviewer"].calls.Load(), "a cached completion must still route onward")
	var completions int
	for _, in := range st.recorded() {
		if in.HookEventName == hooks.EventAfterAgentComplete {
			completions++
			assert.Equal(t, "cached worker answer", in.Output)
		}
	}
	assert.Equal(t, 1, completions)
}

func TestRouting_PlainAgentsKeepSharedHandoffBehavior(t *testing.T) {
	t.Parallel()
	rt, sumProv := forceHandoffTeam(t,
		newStreamBuilder().AddContent("extracted").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("summary").AddStopWithUsage(1, 1).Build(),
	)
	sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true))
	runRouted(t, rt, sess)

	assert.False(t, sess.Routed())
	assert.Empty(t, sess.RouteAgent())
	assert.Equal(t, "summarizer", rt.CurrentAgentName(t.Context()))
	assert.Equal(t, 1, sumProv.handoffCallCount())
}

func TestRouting_UserMessageEventKeepsTheUserPosition(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.9))})

	sess := session.New(session.WithUserMessage("Diagnose this deadlock"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	require.NotEmpty(t, routeEvents(events), "the conversation must have been routed")
	var positions []int
	for _, ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok {
			positions = append(positions, ue.SessionPosition)
		}
	}
	assert.Equal(t, []int{0}, positions, "routing items appended before the event must not shift the user's position")
}

func TestRouting_FollowUpStreamStartRecordsEntryAgent(t *testing.T) {
	t.Parallel()

	f := newEvaluatorRoutingFixture(t,
		scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.9))},
		scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.4))},
	)
	rec := &recordingTelemetry{}
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	f.rt.telemetry = rec
	f.rt.tracer = tp.Tracer("test")

	sess := session.New(session.WithUserMessage("First complex request"), session.WithNonInteractive(true))
	runRouted(t, f.rt, sess)
	require.True(t, sess.Routed())
	require.Equal(t, "specialist", sess.RouteAgent(), "first request should leave RouteAgent on specialist")

	// Second request restarts at the entry agent (root) and stays there when
	// the evaluator confidence is below the routing threshold.
	sess.AddMessage(session.UserMessage("Follow-up on root"))
	runRouted(t, f.rt, sess)

	starts := rec.snapshot().sessionStarts
	require.Len(t, starts, 2)
	assert.Equal(t, "root", starts[0].AgentName)
	assert.Equal(t, "root", starts[1].AgentName, "follow-up session_start must record entry agent, not previous route")

	var sessionSpanAgents []string
	// sessionSpan.End() runs in a defer after finalizeEventChannel closes the
	// event channel, so wait briefly for both spans to flush.
	require.Eventually(t, func() bool {
		sessionSpanAgents = sessionSpanAgents[:0]
		for _, sp := range exporter.GetSpans() {
			if sp.Name != "runtime.session" {
				continue
			}
			for _, attr := range sp.Attributes {
				if string(attr.Key) == genai.AttrAgentNameRuntime {
					sessionSpanAgents = append(sessionSpanAgents, attr.Value.AsString())
				}
			}
		}
		return len(sessionSpanAgents) == 2
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"root", "root"}, sessionSpanAgents, "both runtime.session spans must be attributed to root")

	// Pinned children still keep their own session-local attribution.
	pinned := session.New(session.WithAgentName("specialist"))
	assert.Equal(t, "specialist", f.rt.streamStartAgentName(pinned))
}

func TestRouting_TransferReturnAtMaxIterationsPublishesRoutedAgent(t *testing.T) {
	t.Parallel()

	helperProv := &queueProvider{
		id: "test/helper-model",
		streams: []chat.MessageStream{
			newStreamBuilder().AddContent("helper-done").AddStopWithUsage(10, 5).Build(),
		},
	}

	var specialistTurns atomic.Int32
	specialistProv := &delegationFuncProvider{
		id: "test/specialist-model",
		fn: func(_ context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
			if specialistTurns.Add(1) == 1 {
				return newStreamBuilder().
					AddToolCallWithStop("tc_helper", transfertask.ToolNameTransferTask, `{"agent":"helper","task":"assist"}`).
					Build(), nil
			}
			return newStreamBuilder().AddContent("should-not-run").AddStopWithUsage(10, 5).Build(), nil
		},
	}

	eval := &scriptedEvaluator{
		answers: []scriptedAnswer{{result: choiceResult("complex", defaultProbabilities("complex", 0.9))}},
	}
	helper := agent.New("helper", "helper agent", agent.WithModel(helperProv))
	specialist := agent.New("specialist", "specialist agent",
		agent.WithModel(specialistProv),
		agent.WithSubAgents(helper),
		agent.WithToolSets(transfertask.New()),
	)
	root := agent.New("root", "root router",
		agent.WithModel(&scriptedProvider{reply: "root-reply"}),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"specialist"}, DefaultAgent: "specialist"}),
		agent.WithHooks(&latest.HooksConfig{
			BeforeAgentRun: latest.HookDefinitions{{
				Type:      hooks.HookTypeEvaluator,
				Evaluator: "task_route",
				RoutingPolicy: &latest.RoutingPolicy{
					Routes:         map[string]string{"complex": "specialist"},
					MinProbability: 0.7,
				},
			}},
		}),
	)

	tm := team.New(
		team.WithAgents(root, specialist, helper),
		team.WithEvaluators(map[string]evaluator.Evaluator{"task_route": eval}),
	)
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	sess := session.New(
		session.WithUserMessage("Diagnose this deadlock"),
		session.WithToolsApproved(true),
		session.WithMaxIterations(1),
		session.WithNonInteractive(true),
	)
	events := runRouted(t, rt, sess)

	var (
		sawMaxIterations bool
		lastAgentInfo    string
	)
	for _, ev := range events {
		switch e := ev.(type) {
		case *MaxIterationsReachedEvent:
			sawMaxIterations = true
		case *AgentInfoEvent:
			lastAgentInfo = e.AgentName
		}
	}

	assert.Equal(t, int32(1), specialistTurns.Load(), "specialist must stop at max_iterations without running Turn 2")
	assert.True(t, sawMaxIterations, "max iterations event should be emitted")
	assert.Equal(t, "root", rt.CurrentAgent().Name(), "shared entry agent must stay root")
	assert.Equal(t, "specialist", sess.RouteAgent(), "parent session must remain routed to specialist")
	assert.Equal(t, "specialist", lastAgentInfo, "final AgentInfo after transfer return must name the routed parent agent")
}
