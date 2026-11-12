package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestResume_ImmediateConfirmation(t *testing.T) {
	t.Parallel()

	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "approve", true: "discard_stale_approval"}[stale], func(t *testing.T) {
			t.Parallel()

			executed := false
			available := recordingTool("the_tool", &executed)
			rt, root := newResumeTestRuntime(t, available)
			if stale {
				rt.Resume(t.Context(), ResumeApprove())
			}
			events := make(chan Event, 32)
			sink := EventSinkFunc(func(ev Event) {
				events <- ev
				if _, ok := ev.(*ToolCallConfirmationEvent); ok {
					decision := ResumeApprove()
					if stale {
						decision = ResumeReject("not approved")
					}
					rt.Resume(t.Context(), decision)
				}
			})

			runResumeTestCall(t, rt, root, available, sink)

			assert.Equal(t, !stale, executed)
			var response *ToolCallResponseEvent
			for _, ev := range drainEvents(events) {
				if res, ok := ev.(*ToolCallResponseEvent); ok {
					response = res
				}
			}
			require.NotNil(t, response)
			require.NotNil(t, response.Result)
			assert.Equal(t, stale, response.Result.IsError)
			if stale {
				assert.Contains(t, response.Response, "not approved")
			}
		})
	}
}

func TestResume_NestedConfirmationDiscardsEarlierDecision(t *testing.T) {
	t.Parallel()

	var rt *LocalRuntime
	var denied error
	commandRan := false
	available := []tools.Tool{{
		Name: "the_tool",
		Handler: func(ctx context.Context, _ tools.ToolCall, callRT tools.Runtime) (*tools.ToolCallResult, error) {
			// The outer approval has been consumed; queue a late duplicate before the nested prompt.
			rt.Resume(ctx, ResumeApprove())
			_, denied = callRT.ConfirmAndRun(ctx, tools.ConfirmedRun{ToolName: "nested_command", Args: map[string]any{}}, func(context.Context, tools.ConfirmedRun) (string, error) {
				commandRan = true
				return "ran", nil
			})
			return tools.ResultSuccess("done"), nil
		},
	}}
	rt, root := newResumeTestRuntime(t, available)
	prompts := 0
	sink := EventSinkFunc(func(ev Event) {
		if prompt, ok := ev.(*ToolCallConfirmationEvent); ok {
			prompts++
			if prompt.ToolCall.Function.Name == "the_tool" {
				rt.Resume(t.Context(), ResumeApprove())
			} else {
				rt.Resume(t.Context(), ResumeReject("nested denied"))
			}
		}
	})

	runResumeTestCall(t, rt, root, available, sink)

	assert.Equal(t, 2, prompts)
	assert.False(t, commandRan)
	require.ErrorIs(t, denied, tools.ErrConfirmationDenied)
	assert.Contains(t, denied.Error(), "nested denied")
}

func TestResume_MaxIterationsDiscardsEarlierDecision(t *testing.T) {
	t.Parallel()

	rt, root := newTestRuntime(t)
	rt.Resume(t.Context(), ResumeApprove())
	sink := EventSinkFunc(func(ev Event) {
		if _, ok := ev.(*MaxIterationsReachedEvent); ok {
			rt.Resume(t.Context(), ResumeReject("stop"))
		}
	})
	_, decision := rt.enforceMaxIterations(t.Context(), session.New(), root, 10, 10, sink)
	assert.Equal(t, iterationStop, decision)
}

func newResumeTestRuntime(t *testing.T, available []tools.Tool) (*LocalRuntime, *agent.Agent) {
	t.Helper()
	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/resume", stream: &mockStream{}}), agent.WithToolSets(newStubToolSet(nil, available, nil)))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt, root
}

func runResumeTestCall(t *testing.T, rt *LocalRuntime, root *agent.Agent, available []tools.Tool, sink EventSink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		rt.processToolCalls(ctx, session.New(), root, []tools.ToolCall{{ID: "outer", Function: tools.FunctionCall{Name: "the_tool", Arguments: "{}"}}}, available, sink)
		close(done)
	}()
	select {
	case <-done:
		require.NoError(t, ctx.Err(), "resume signal lost")
	case <-ctx.Done():
		<-done
		t.Fatal("resume signal lost")
	}
}

func TestResume_MaxIterationsWithoutWaitPreservesForegroundDecision(t *testing.T) {
	t.Parallel()

	for _, nonInteractive := range []bool{true, false} {
		t.Run(map[bool]string{true: "background", false: "cancelled"}[nonInteractive], func(t *testing.T) {
			t.Parallel()
			executed := false
			available := recordingTool("the_tool", &executed)
			rt, root := newResumeTestRuntime(t, available)
			sink := EventSinkFunc(func(ev Event) {
				if _, ok := ev.(*ToolCallConfirmationEvent); ok {
					rt.Resume(t.Context(), ResumeApprove())
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if !nonInteractive {
						cancel()
					}
					other := session.New(session.WithNonInteractive(nonInteractive))
					limitEvents := make(chan Event, 8)
					_, decision := rt.enforceMaxIterations(ctx, other, root, 10, 10, NewChannelSink(limitEvents))
					assert.Equal(t, iterationStop, decision)
					assert.IsType(t, &MaxIterationsReachedEvent{}, <-limitEvents)
				}
			})

			runResumeTestCall(t, rt, root, available, sink)
			assert.True(t, executed, "a max-iteration path that never waits must not consume another session's approval")
		})
	}
}
