package chatserver

import (
	"context"
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
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type replyModel struct {
	reply string
	calls atomic.Int32
}

func (m *replyModel) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/mock-model") }

func (m *replyModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	m.calls.Add(1)
	return &replyStream{reply: m.reply}, nil
}

func (m *replyModel) BaseConfig() base.Config { return base.Config{} }
func (m *replyModel) MaxTokens() int          { return 0 }

type replyStream struct {
	reply string
	step  int
}

func (s *replyStream) Recv() (chat.MessageStreamResponse, error) {
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

func (s *replyStream) Close() {}

type unpriced struct{ runtime.ModelStore }

func (unpriced) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) { return nil, nil }

type stubJudge struct{ result *evaluator.Result }

func (j stubJudge) Evaluate(context.Context, any) (*evaluator.Result, error) { return j.result, nil }

func routedChatRuntime(t *testing.T) (runtime.Runtime, map[string]*replyModel) {
	t.Helper()
	models := map[string]*replyModel{
		"root":  {reply: "root answer"},
		"quick": {reply: "quick answer"},
		"deep":  {reply: "deep answer"},
	}
	hook := latest.HookDefinition{
		Type: hooks.HookTypeEvaluator, Evaluator: "route",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"simple": "quick", "complex": "deep"}, MinProbability: 0.6},
	}
	root := agent.New("root", "root", agent.WithModel(models["root"]),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"quick", "deep"}, DefaultAgent: "quick"}),
		agent.WithHooks(&latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{hook}}))
	tm := team.New(
		team.WithAgents(root,
			agent.New("quick", "quick", agent.WithModel(models["quick"])),
			agent.New("deep", "deep", agent.WithModel(models["deep"]))),
		team.WithEvaluators(map[string]evaluator.Evaluator{"route": stubJudge{result: &evaluator.Result{
			Type: "choice", Model: "judge", Choice: "complex",
			Probabilities: map[string]float64{"simple": 0.05, "complex": 0.95},
			Usage:         evaluator.Usage{InputTokens: 5},
		}}}))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionCompaction(false), runtime.WithModelStore(unpriced{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return rt, models
}

// The OpenAI-compatible endpoint answers with the routed agent, because the
// session it builds is an ordinary root session rather than a pinned one.
func TestRunAgentLoopRoutesBeforeAnyAgentAnswers(t *testing.T) {
	t.Parallel()
	rt, models := routedChatRuntime(t)

	sess := buildSession([]ChatCompletionMessage{{Role: "user", Content: "Diagnose this deadlock"}}, t.TempDir())
	require.NotNil(t, sess)
	assert.Empty(t, sess.AgentName, "a pinned session would silently disable routing")

	var out strings.Builder
	require.NoError(t, runAgentLoop(t.Context(), rt, sess, agentEmit{onContent: func(s string) { out.WriteString(s) }}))

	assert.Equal(t, "deep answer", out.String())
	assert.Zero(t, models["root"].calls.Load(), "the entry agent must not answer")
	assert.Zero(t, models["quick"].calls.Load())
	assert.EqualValues(t, 1, models["deep"].calls.Load())
	require.Len(t, sess.RoutingDecisionHistory(), 1)
}
