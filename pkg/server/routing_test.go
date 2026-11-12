package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type fixedModel struct {
	reply string
	calls atomic.Int32
}

func (m *fixedModel) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/mock-model") }

func (m *fixedModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	m.calls.Add(1)
	return &oneShotStream{reply: m.reply}, nil
}

func (m *fixedModel) BaseConfig() base.Config { return base.Config{} }
func (m *fixedModel) MaxTokens() int          { return 0 }

type oneShotStream struct {
	reply string
	step  int
}

func (s *oneShotStream) Recv() (chat.MessageStreamResponse, error) {
	s.step++
	switch s.step {
	case 1:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: s.reply}}}}, nil
	case 2:
		return chat.MessageStreamResponse{
			Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}},
			Usage:   &chat.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	}
	return chat.MessageStreamResponse{}, errors.New("EOF")
}

func (s *oneShotStream) Close() {}

type noPricing struct{ runtime.ModelStore }

func (noPricing) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) { return nil, nil }

type fixedJudge struct {
	result *evaluator.Result
	err    error
}

func (j *fixedJudge) Evaluate(context.Context, any) (*evaluator.Result, error) {
	return j.result, j.err
}

type routedServer struct {
	sm     *SessionManager
	store  session.Store
	sess   *session.Session
	router *fixedModel
	quick  *fixedModel
	deep   *fixedModel
}

// newRoutedServer attaches a real routed runtime to a session manager, the way
// `serve api` serves a loaded team, backed by the manager's own session store.
func newRoutedServer(t *testing.T, judge *fixedJudge) *routedServer {
	t.Helper()
	s := &routedServer{
		router: &fixedModel{reply: "router must not answer"},
		quick:  &fixedModel{reply: "quick answer"},
		deep:   &fixedModel{reply: "deep answer"},
		store:  session.NewInMemorySessionStore(),
		sess:   session.New(session.WithNonInteractive(true)),
	}
	require.NoError(t, s.store.AddSession(t.Context(), s.sess))

	hook := latest.HookDefinition{
		Type: hooks.HookTypeEvaluator, Evaluator: "route",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"simple": "quick", "complex": "deep"}, MinProbability: 0.6},
	}
	root := agent.New("root", "root", agent.WithModel(s.router),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"quick", "deep"}, DefaultAgent: "quick"}),
		agent.WithHooks(&latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{hook}}))
	tm := team.New(
		team.WithAgents(root,
			agent.New("quick", "quick", agent.WithModel(s.quick)),
			agent.New("deep", "deep", agent.WithModel(s.deep))),
		team.WithEvaluators(map[string]evaluator.Evaluator{"route": judge}))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm,
		runtime.WithSessionCompaction(false), runtime.WithModelStore(noPricing{}), runtime.WithSessionStore(s.store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	s.sm = NewSessionManager(t.Context(), config.Sources{}, s.store, 0, &config.RuntimeConfig{})
	s.sm.AttachRuntime(t.Context(), s.sess.ID, rt, s.sess)
	return s
}

func (s *routedServer) run(t *testing.T, message string) []runtime.Event {
	t.Helper()
	ch, err := s.sm.RunSession(t.Context(), s.sess.ID, "agent", "root", []api.Message{{Role: chat.MessageRoleUser, Content: message}}, "")
	require.NoError(t, err)
	var events []runtime.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func complexJudge() *fixedJudge {
	return &fixedJudge{result: &evaluator.Result{
		Type: "choice", Model: "judge", Choice: "complex",
		Probabilities: map[string]float64{"simple": 0.05, "complex": 0.95},
		Usage:         evaluator.Usage{InputTokens: 5},
	}}
}

// The API server routes before any agent answers, streams the route to
// clients, and stores the decision in the server's session store.
func TestRunSessionRoutesAndPersistsDecision(t *testing.T) {
	t.Parallel()
	s := newRoutedServer(t, complexJudge())

	events := s.run(t, "Diagnose this deadlock")

	var routes []*runtime.AgentRouteEvent
	var decisions []*runtime.RoutingDecisionEvent
	for _, ev := range events {
		switch e := ev.(type) {
		case *runtime.AgentRouteEvent:
			routes = append(routes, e)
		case *runtime.RoutingDecisionEvent:
			decisions = append(decisions, e)
		}
	}
	require.Len(t, routes, 1, "clients must see the route")
	assert.Equal(t, "deep", routes[0].ToAgent)
	require.Len(t, decisions, 1, "clients must see the decision")

	assert.Zero(t, s.router.calls.Load(), "the entry agent must not answer")
	assert.Zero(t, s.quick.calls.Load())
	assert.EqualValues(t, 1, s.deep.calls.Load())

	stored, err := s.store.GetSession(t.Context(), s.sess.ID)
	require.NoError(t, err)
	history := stored.RoutingDecisionHistory()
	require.Len(t, history, 1, "the decision must be stored by the server")
	assert.Equal(t, "deep", history[0].ToAgent)
	assert.Equal(t, "before_agent_run", history[0].Phase)
	assert.Equal(t, "complex", history[0].Selected)
}

// Every request through the server restarts at the entry agent, and the
// session keeps one decision per request.
func TestRunSessionEachRequestRoutesAgain(t *testing.T) {
	t.Parallel()
	s := newRoutedServer(t, complexJudge())

	s.run(t, "first")
	s.run(t, "second")

	assert.EqualValues(t, 2, s.deep.calls.Load())
	assert.Zero(t, s.router.calls.Load())
	stored, err := s.store.GetSession(t.Context(), s.sess.ID)
	require.NoError(t, err)
	assert.Len(t, stored.RoutingDecisionHistory(), 2)
}

// A failed decision surfaces as an error event and no agent answers.
func TestRunSessionRoutingFailureIsReportedAndStopsRun(t *testing.T) {
	t.Parallel()
	s := newRoutedServer(t, &fixedJudge{err: &evaluator.TerminalError{Err: errors.New("budget exceeded")}})

	events := s.run(t, "anything")

	var errs []*runtime.ErrorEvent
	for _, ev := range events {
		if e, ok := ev.(*runtime.ErrorEvent); ok {
			errs = append(errs, e)
		}
	}
	require.NotEmpty(t, errs, "clients must be told the decision failed")
	assert.Zero(t, s.quick.calls.Load())
	assert.Zero(t, s.deep.calls.Load())
	assert.Zero(t, s.router.calls.Load())
}
