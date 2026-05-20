package workflowrun

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockRunner struct {
	agentName string
	output    string
}

func (m *mockRunner) CurrentAgentName() string {
	return m.agentName
}

func (m *mockRunner) SetCurrentAgent(agentName string) error {
	m.agentName = agentName
	return nil
}

func (m *mockRunner) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event, 1)

	// Add an assistant message so GetLastAssistantMessageContent returns the output
	sess.AddMessage(&session.Message{
		AgentName: m.agentName,
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: m.output,
		},
	})

	close(ch)
	return ch
}

func TestLocalExecutor_Run(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	runner := &mockRunner{output: "success"}
	exec := NewLocalExecutor(runner)

	cfg := &workflow.Workflow{
		Steps: []workflow.Step{
			{
				ID:   "step1",
				Type: workflow.StepTypeAgent,
				Name: "test_agent",
			},
		},
	}

	sess := session.New()
	events := make(chan Event, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stepCtx, err := exec.Run(ctx, cfg, sess, events)
	require.NoError(t, err)
	require.NotNil(t, stepCtx)

	out, ok := stepCtx.GetOutput("step1")
	assert.True(t, ok)
	assert.Equal(t, "success", out.Output)
}

func TestLocalExecutor_RunParallel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &mockRunner{output: "parallel_success"}
	exec := NewLocalExecutor(runner)

	cfg := &workflow.Workflow{
		Steps: []workflow.Step{
			{
				ID:   "parallel_step",
				Type: workflow.StepTypeParallel,
				Steps: []workflow.Step{
					{ID: "sub1", Type: workflow.StepTypeAgent, Name: "agent1"},
					{ID: "sub2", Type: workflow.StepTypeAgent, Name: "agent2"},
				},
			},
		},
	}

	sess := session.New()
	events := make(chan Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stepCtx, err := exec.Run(ctx, cfg, sess, events)
	require.NoError(t, err)
	require.NotNil(t, stepCtx)

	po, ok := stepCtx.GetParallelOutput("parallel_step")
	require.True(t, ok)
	require.Len(t, po.Steps, 2)
	assert.Equal(t, "parallel_success", po.Steps["sub1"].Output)
	assert.Equal(t, "parallel_success", po.Steps["sub2"].Output)
}
