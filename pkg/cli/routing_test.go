package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
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
	calls  atomic.Int32
}

func (j *fixedJudge) Evaluate(context.Context, any) (*evaluator.Result, error) {
	j.calls.Add(1)
	return j.result, j.err
}

type routedRun struct {
	rt     runtime.Runtime
	router *fixedModel
	quick  *fixedModel
	deep   *fixedModel
	judge  *fixedJudge
}

// newRoutedRuntime builds root -> {quick, deep} routed by judge before any agent answers.
func newRoutedRuntime(t *testing.T, judge *fixedJudge) *routedRun {
	t.Helper()
	r := &routedRun{
		router: &fixedModel{reply: "router must not answer"},
		quick:  &fixedModel{reply: "quick answer"},
		deep:   &fixedModel{reply: "deep answer"},
		judge:  judge,
	}
	hook := latest.HookDefinition{
		Type: hooks.HookTypeEvaluator, Evaluator: "route",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"simple": "quick", "complex": "deep"}, MinProbability: 0.6},
	}
	root := agent.New("root", "root", agent.WithModel(r.router),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"quick", "deep"}, DefaultAgent: "quick"}),
		agent.WithHooks(&latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{hook}}))
	tm := team.New(
		team.WithAgents(root,
			agent.New("quick", "quick", agent.WithModel(r.quick)),
			agent.New("deep", "deep", agent.WithModel(r.deep))),
		team.WithEvaluators(map[string]evaluator.Evaluator{"route": judge}))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionCompaction(false), runtime.WithModelStore(noPricing{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	r.rt = rt
	return r
}

func complexJudge() *fixedJudge {
	return &fixedJudge{result: &evaluator.Result{
		Type: "choice", Model: "judge", Choice: "complex",
		Probabilities: map[string]float64{"simple": 0.05, "complex": 0.95},
		Usage:         evaluator.Usage{InputTokens: 5},
	}}
}

// The headless text path routes before any agent answers and prints only the
// chosen agent's answer.
func TestRunHeadlessRoutesBeforeAnyAgentAnswers(t *testing.T) {
	t.Parallel()
	r := newRoutedRuntime(t, complexJudge())

	var out bytes.Buffer
	sess := session.New(session.WithNonInteractive(true))
	err := Run(t.Context(), NewPrinter(&out), Config{}, r.rt, sess, []string{"Diagnose this deadlock"})

	require.NoError(t, err)
	assert.Contains(t, out.String(), "deep answer")
	assert.NotContains(t, out.String(), "quick answer")
	assert.NotContains(t, out.String(), "router must not answer")
	assert.Zero(t, r.router.calls.Load(), "the entry agent must never call its model when it routes")
	assert.Zero(t, r.quick.calls.Load())
	assert.EqualValues(t, 1, r.deep.calls.Load())
	assert.EqualValues(t, 1, r.judge.calls.Load())

	decisions := sess.RoutingDecisionHistory()
	require.Len(t, decisions, 1)
	assert.Equal(t, "deep", decisions[0].ToAgent)
	assert.Equal(t, "before_agent_run", decisions[0].Phase)
}

// --json prints every runtime event, so a consumer can see the decision.
func TestRunHeadlessJSONStreamIncludesRoutingEvents(t *testing.T) {
	t.Parallel()
	r := newRoutedRuntime(t, complexJudge())

	var out bytes.Buffer
	sess := session.New(session.WithNonInteractive(true))
	err := Run(t.Context(), NewPrinter(&out), Config{OutputJSON: true}, r.rt, sess, []string{"Diagnose this deadlock"})
	require.NoError(t, err)

	types := map[string]map[string]any{}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &event), "each output line must be valid JSON: %q", line)
		if kind, ok := event["type"].(string); ok {
			types[kind] = event
		}
	}

	route := types["agent_route"]
	require.NotNil(t, route, "the route must be streamed")
	assert.Equal(t, "root", route["from_agent"])
	assert.Equal(t, "deep", route["to_agent"])
	assert.Equal(t, "complex", route["selected"])

	decision, _ := types["routing_decision"]["decision"].(map[string]any)
	require.NotNil(t, decision, "the persisted decision must be streamed too")
	assert.Equal(t, "deep", decision["to_agent"])
	assert.Equal(t, "before_agent_run", decision["phase"])
	assert.NotContains(t, out.String(), "Diagnose this deadlock\"}", "the request must not be copied into the decision record")
}

// A failed routing decision must fail the headless run, so scripts see a
// non-zero exit status instead of a silently wrong agent answering.
func TestRunHeadlessRoutingFailureReturnsError(t *testing.T) {
	t.Parallel()
	r := newRoutedRuntime(t, &fixedJudge{err: &evaluator.TerminalError{Err: errors.New("budget exceeded")}})

	for name, cfg := range map[string]Config{"text": {}, "json": {OutputJSON: true}} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			sess := session.New(session.WithNonInteractive(true))
			err := Run(t.Context(), NewPrinter(&out), cfg, r.rt, sess, []string{"anything"})

			require.Error(t, err)
			assert.Zero(t, r.quick.calls.Load(), "no agent may answer after a failed decision")
			assert.Zero(t, r.deep.calls.Load())
			assert.Zero(t, r.router.calls.Load())
		})
	}
}

// Low confidence still produces an answer headless, from the default agent.
func TestRunHeadlessLowConfidenceUsesDefaultAgent(t *testing.T) {
	t.Parallel()
	unsure := &fixedJudge{result: &evaluator.Result{
		Type: "choice", Model: "judge", Choice: "complex",
		Probabilities: map[string]float64{"simple": 0.45, "complex": 0.55},
	}}
	r := newRoutedRuntime(t, unsure)

	var out bytes.Buffer
	sess := session.New(session.WithNonInteractive(true))
	require.NoError(t, Run(t.Context(), NewPrinter(&out), Config{}, r.rt, sess, []string{"hmm"}))

	assert.Contains(t, out.String(), "quick answer", "below min_probability the default agent answers")
	decisions := sess.RoutingDecisionHistory()
	require.Len(t, decisions, 1)
	assert.Equal(t, hooks.FallbackBelowThreshold, decisions[0].FallbackReason)
}

// Continuation also works headless: a finished draft is routed onward and
// both agents' answers are printed in order.
func TestRunHeadlessCompletionRoutingContinuesToReviewer(t *testing.T) {
	t.Parallel()
	drafter := &fixedModel{reply: "draft answer"}
	reviewer := &fixedModel{reply: "reviewed answer"}
	hook := latest.HookDefinition{
		Type: hooks.HookTypeEvaluator, Evaluator: "review",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"review": "reviewer", "ready": "reviewer"}, MinProbability: 0.6},
	}
	root := agent.New("drafter", "drafter", agent.WithModel(drafter),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}, DefaultAgent: "reviewer"}),
		agent.WithHooks(&latest.HooksConfig{AfterAgentComplete: latest.HookDefinitions{hook}}))
	judge := &fixedJudge{result: &evaluator.Result{
		Type: "choice", Model: "judge", Choice: "review",
		Probabilities: map[string]float64{"review": 0.9, "ready": 0.1},
		Usage:         evaluator.Usage{InputTokens: 5},
	}}
	tm := team.New(
		team.WithAgents(root, agent.New("reviewer", "reviewer", agent.WithModel(reviewer))),
		team.WithEvaluators(map[string]evaluator.Evaluator{"review": judge}))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionCompaction(false), runtime.WithModelStore(noPricing{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	var out bytes.Buffer
	sess := session.New(session.WithNonInteractive(true))
	require.NoError(t, Run(t.Context(), NewPrinter(&out), Config{}, rt, sess, []string{"write something"}))

	text := out.String()
	require.Contains(t, text, "draft answer")
	require.Contains(t, text, "reviewed answer")
	assert.Less(t, strings.Index(text, "draft answer"), strings.Index(text, "reviewed answer"), "the draft is printed before its review")
	assert.EqualValues(t, 1, drafter.calls.Load())
	assert.EqualValues(t, 1, reviewer.calls.Load())

	decisions := sess.RoutingDecisionHistory()
	require.Len(t, decisions, 1)
	assert.Equal(t, "after_agent_complete", decisions[0].Phase)
	assert.Equal(t, "reviewer", decisions[0].ToAgent)
}
