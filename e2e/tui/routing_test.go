package tui_test

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

// routingJudge assesses a request as "complex" when it mentions a deadlock.
type routingJudge struct{ calls atomic.Int32 }

func (j *routingJudge) Evaluate(_ context.Context, state any) (*evaluator.Result, error) {
	j.calls.Add(1)
	input, _ := state.(map[string]any)["input"].(string)
	choice, other := "simple", "complex"
	if strings.Contains(strings.ToLower(input), "deadlock") {
		choice, other = "complex", "simple"
	}
	return &evaluator.Result{
		Type: "choice", Model: "judge", Choice: choice,
		Probabilities: map[string]float64{choice: 0.97, other: 0.03},
	}, nil
}

type routedToolset struct{ tool tools.Tool }

func (s routedToolset) Tools(context.Context) ([]tools.Tool, error) { return []tools.Tool{s.tool}, nil }

// newRoutedTUI builds the real TUI over a hook-routed team: root only selects,
// quick and specialist answer with scripted models.
func newRoutedTUI(t *testing.T, policy session.SafetyPolicy, specialistScripts ...[]chat.MessageStreamResponse) (*tuitest.Driver, *routingJudge) {
	t.Helper()
	isolateState(t)

	judge := &routingJudge{}
	root := agent.New("root", "Only routes.", agent.WithDescription("Router"),
		agent.WithModel(&scriptedProvider{id: "test/router", contextSize: 10000, scripts: [][]chat.MessageStreamResponse{contentScript("router must not answer", 1, 1)}}),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"quick", "specialist"}, DefaultAgent: "quick"}),
		agent.WithHooks(&latest.HooksConfig{BeforeAgentRun: latest.HookDefinitions{{
			Type: "evaluator", Evaluator: "task_route",
			RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"simple": "quick", "complex": "specialist"}, MinProbability: 0.85},
		}}}))
	quick := agent.New("quick", "Answer briefly.", agent.WithDescription("Quick answers"),
		agent.WithModel(&scriptedProvider{id: "test/quick", contextSize: 10000, scripts: [][]chat.MessageStreamResponse{contentScript("Quick answer.", 10, 5)}}))
	specialistOpts := []agent.Opt{
		agent.WithDescription("Specialist"),
		agent.WithModel(&scriptedProvider{id: "test/specialist", contextSize: 10000, scripts: specialistScripts}),
	}
	if len(specialistScripts) > 1 {
		tool := tools.Tool{
			Name: "routed_action", Description: "Perform a test action", Parameters: map[string]any{"type": "object"},
			Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("done"), nil
			},
		}
		specialistOpts = append(specialistOpts, agent.WithToolSets(routedToolset{tool: tool}))
	}
	specialist := agent.New("specialist", "Investigate.", specialistOpts...)

	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root, quick, specialist),
		team.WithEvaluators(map[string]evaluator.Evaluator{"task_route": judge})),
		runtime.WithCurrentAgent("root"), runtime.WithSessionCompaction(false), runtime.WithModelStore(stubModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	wd, _ := os.Getwd()
	application := app.New(t.Context(), rt, session.New(session.WithSafetyPolicy(policy)))
	return tuitest.New(t, tui.New(t.Context(), nil, application, wd, func() {}), 120, 40), judge
}

func TestRouting_EachRequestReentersTheEntryAgent(t *testing.T) {
	d, judge := newRoutedTUI(t, session.SafetyPolicyBalanced,
		contentScript("Specialist analysis.", 10, 5))

	d.Type("What does EXPOSE do?").Enter().WaitFor(tuitest.Contains("Quick answer."))
	d.Type("Diagnose this deadlock").Enter().WaitFor(tuitest.Contains("Specialist analysis."))

	d.Assert(tuitest.Absent("router must not answer"))
	assert.EqualValues(t, 2, judge.calls.Load(), "each request reruns the entry selector")
}

func TestRouting_ToolApprovalStillAppliesToRoutedAgents(t *testing.T) {
	d, _ := newRoutedTUI(t, session.SafetyPolicyStrict,
		toolCallScript("call-1", "routed_action", "{}", 10, 5),
		contentScript("Action completed.", 20, 5))

	d.Type("Diagnose this deadlock").Enter().WaitFor(tuitest.Contains("always allow routed_action"))
	d.Assert(tuitest.Absent("Action completed."))
	d.Type("y").WaitFor(tuitest.Contains("Action completed."))
}

type fixedReviewJudge struct{ calls atomic.Int32 }

func (j *fixedReviewJudge) Evaluate(context.Context, any) (*evaluator.Result, error) {
	j.calls.Add(1)
	return &evaluator.Result{
		Type: "choice", Model: "judge-1", Choice: "review",
		Probabilities: map[string]float64{"review": 0.95, "ready": 0.05},
		Usage:         evaluator.Usage{InputTokens: 3},
	}, nil
}

// A finished draft continues to the reviewer inside the native TUI, and both
// answers end up in the transcript.
func TestRouting_CompletionContinuesToReviewer(t *testing.T) {
	isolateState(t)

	judge := &fixedReviewJudge{}
	drafter := agent.New("drafter", "Draft an answer.", agent.WithDescription("Drafts"),
		agent.WithModel(&scriptedProvider{id: "test/drafter", contextSize: 10000, scripts: [][]chat.MessageStreamResponse{contentScript("Draft answer.", 10, 5)}}),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}, DefaultAgent: "reviewer"}),
		agent.WithHooks(&latest.HooksConfig{AfterAgentComplete: latest.HookDefinitions{{
			Type: "evaluator", Evaluator: "answer_review",
			RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"review": "reviewer", "ready": "reviewer"}, MinProbability: 0.85},
		}}}))
	reviewer := agent.New("reviewer", "Review the draft.", agent.WithDescription("Reviews"),
		agent.WithModel(&scriptedProvider{id: "test/reviewer", contextSize: 10000, scripts: [][]chat.MessageStreamResponse{contentScript("Reviewed answer.", 10, 5)}}))

	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(drafter, reviewer),
		team.WithEvaluators(map[string]evaluator.Evaluator{"answer_review": judge})),
		runtime.WithCurrentAgent("drafter"), runtime.WithSessionCompaction(false), runtime.WithModelStore(stubModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	wd, _ := os.Getwd()
	application := app.New(t.Context(), rt, session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced)))
	d := tuitest.New(t, tui.New(t.Context(), nil, application, wd, func() {}), 120, 40)

	d.Type("Write something").Enter().WaitFor(tuitest.Contains("Reviewed answer."))

	d.Assert(tuitest.Contains("Draft answer."))
	assert.EqualValues(t, 1, judge.calls.Load(), "the completion selector runs once")
}
