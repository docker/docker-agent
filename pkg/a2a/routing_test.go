package a2a

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func answerStream(text string) *mockStream {
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: text}}}},
		{
			Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}},
			Usage:   &chat.Usage{InputTokens: 1, OutputTokens: 1},
		},
	}}
}

type countingProvider struct {
	mockProvider

	calls atomic.Int32
}

func (p *countingProvider) CreateChatCompletionStream(ctx context.Context, m []chat.Message, t []tools.Tool) (chat.MessageStream, error) {
	p.calls.Add(1)
	return p.mockProvider.CreateChatCompletionStream(ctx, m, t)
}

type judge struct {
	result *evaluator.Result
	err    error
}

func (j judge) Evaluate(context.Context, any) (*evaluator.Result, error) { return j.result, j.err }

func routedTeam(j judge) (*team.Team, *dagent.Agent, map[string]*countingProvider) {
	providers := map[string]*countingProvider{}
	newProvider := func(name string) *countingProvider {
		p := &countingProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "mock-model"), stream: answerStream(name + " answer")}}
		providers[name] = p
		return p
	}
	hook := latest.HookDefinition{
		Type: hooks.HookTypeEvaluator, Evaluator: "route",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"simple": "quick", "complex": "deep"}, MinProbability: 0.6},
	}
	root := dagent.New("root", "root", dagent.WithModel(newProvider("root")),
		dagent.WithRouting(dagent.Routing{AllowedAgents: []string{"quick", "deep"}, DefaultAgent: "quick"}),
		dagent.WithHooks(&latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{hook}}))
	tm := team.New(
		team.WithAgents(root,
			dagent.New("quick", "quick", dagent.WithModel(newProvider("quick"))),
			dagent.New("deep", "deep", dagent.WithModel(newProvider("deep")))),
		team.WithEvaluators(map[string]evaluator.Evaluator{"route": j}))
	return tm, root, providers
}

func complexJudge() judge {
	return judge{result: &evaluator.Result{
		Type: "choice", Model: "judge", Choice: "complex",
		Probabilities: map[string]float64{"simple": 0.05, "complex": 0.95},
		Usage:         evaluator.Usage{InputTokens: 5},
	}}
}

// The A2A adapter serves the routed agent's answer, not the entry agent's, and
// the decision is stored in the session.
func TestRunDockerAgent_RoutesBeforeAnyAgentAnswers(t *testing.T) {
	t.Parallel()

	tm, root, providers := routedTeam(complexJudge())
	store := newRecordingStore()
	ctx := newFakeInvocationContext(t.Context(), "a2a-routed", "Diagnose this deadlock")

	events := collectRunEvents(ctx, tm, root, store, session.SafetyPolicyRestricted)

	var out strings.Builder
	for _, e := range events {
		require.NoError(t, e.err)
		if e.event != nil && e.event.Content != nil && len(e.event.Content.Parts) > 0 {
			out.WriteString(e.event.Content.Parts[0].Text)
		}
	}
	text := out.String()
	assert.Contains(t, text, "deep answer")
	assert.NotContains(t, text, "root answer")
	assert.Zero(t, providers["root"].calls.Load(), "the entry agent must not answer")
	assert.Zero(t, providers["quick"].calls.Load())
	assert.EqualValues(t, 1, providers["deep"].calls.Load())

	stored, err := store.GetSession(t.Context(), "a2a-routed")
	require.NoError(t, err)
	history := stored.RoutingDecisionHistory()
	require.Len(t, history, 1, "the adapter must persist the decision")
	assert.Equal(t, "deep", history[0].ToAgent)
	assert.Equal(t, "before_agent_run", history[0].Phase)
}

// A failed decision is reported to the A2A client and no agent answers.
func TestRunDockerAgent_RoutingFailureIsReported(t *testing.T) {
	t.Parallel()

	tm, root, providers := routedTeam(judge{err: &evaluator.TerminalError{Err: errors.New("budget exceeded")}})
	ctx := newFakeInvocationContext(t.Context(), "a2a-failed", "anything")

	events := collectRunEvents(ctx, tm, root, newRecordingStore(), session.SafetyPolicyRestricted)

	var sawError bool
	for _, e := range events {
		sawError = sawError || e.err != nil
	}
	assert.True(t, sawError, "the client must be told the routing decision failed")
	for name, p := range providers {
		assert.Zero(t, p.calls.Load(), "%s must not answer", name)
	}
}
