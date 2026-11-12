package team

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestForWorkingDirRebindsGraph(t *testing.T) {
	t.Parallel()
	var calls []string
	newWorkspaceAgent := func(name string) *agent.Agent {
		return agent.New(name, "", agent.WithWorkingDirFactory(func(_ context.Context, dir string) (*agent.Agent, error) {
			calls = append(calls, name+":"+dir)
			return agent.New(name, ""), nil
		}))
	}
	root, worker, target := newWorkspaceAgent("root"), newWorkspaceAgent("worker"), newWorkspaceAgent("target")
	imported := newWorkspaceAgent("imported-child")
	agent.WithSubAgents(worker, imported)(root)
	agent.WithSubAgents(worker)(target)
	agent.WithHandoffs(target)(worker)
	agent.WithForceHandoff(root)(target)
	agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker", "target"}, DefaultAgent: "worker"})(root)
	checker := permissions.NewCheckerFromRules([]string{"*"}, nil, nil)
	assessment := &workspaceEvaluator{}
	cfg := latest.AgentConfig{Name: "root", Instruction: "original config"}
	original := New(WithAgents(root, worker, target), WithPermissions(checker),
		WithEvaluators(map[string]evaluator.Evaluator{"test": assessment}),
		WithAgentConfigs(map[string]latest.AgentConfig{"root": cfg}), WithRuntimeSafety(latest.SafetyModeStrict))

	rebuilt, err := original.ForWorkingDir(t.Context(), "workspace")
	require.NoError(t, err)
	assert.Equal(t, original.AgentNames(), rebuilt.AgentNames())
	r, err := rebuilt.Agent("root")
	require.NoError(t, err)
	w, err := rebuilt.Agent("worker")
	require.NoError(t, err)
	h, err := rebuilt.Agent("target")
	require.NoError(t, err)
	assert.NotSame(t, root, r)
	assert.NotSame(t, worker, w)
	assert.NotSame(t, target, h)
	require.Len(t, r.SubAgents(), 2)
	assert.Same(t, w, r.SubAgents()[0])
	assert.NotSame(t, imported, r.SubAgents()[1])
	assert.Equal(t, []*agent.Agent{r, h}, w.Parents())
	assert.Equal(t, []*agent.Agent{h}, w.Handoffs())
	assert.Same(t, r, h.ForceHandoff())
	assert.Equal(t, root.Routing(), r.Routing())
	r.Routing().AllowedAgents[0] = "changed"
	assert.Equal(t, "worker", root.Routing().AllowedAgents[0])
	assert.Same(t, checker, rebuilt.Permissions())
	e, ok := rebuilt.Evaluator("test")
	require.True(t, ok)
	assert.Same(t, assessment, e)
	copiedCfg, ok := rebuilt.AgentConfig("root")
	require.True(t, ok)
	assert.Equal(t, cfg, copiedCfg)
	assert.Equal(t, latest.SafetyModeStrict, rebuilt.RuntimeSafety())
	assert.Equal(t, []string{"root:workspace", "worker:workspace", "target:workspace", "imported-child:workspace"}, calls)
	assert.Equal(t, []*agent.Agent{root, target}, worker.Parents())

	nested, err := rebuilt.ForWorkingDir(t.Context(), "nested")
	require.NoError(t, err)
	assert.Equal(t, original.AgentNames(), nested.AgentNames())
}

func TestForWorkingDirStopsLinkedAgents(t *testing.T) {
	t.Parallel()
	stopped := &stopRecordingToolSet{}
	child := agent.New("child", "", agent.WithWorkingDirFactory(func(context.Context, string) (*agent.Agent, error) {
		return agent.New("child", "", agent.WithToolSets(stopped)), nil
	}))
	root := agent.New("root", "", agent.WithSubAgents(child), agent.WithWorkingDirFactory(func(context.Context, string) (*agent.Agent, error) {
		return agent.New("root", ""), nil
	}))
	workspace, err := New(WithAgents(root)).ForWorkingDir(t.Context(), "workspace")
	require.NoError(t, err)
	rebuilt, err := workspace.DefaultAgent()
	require.NoError(t, err)
	require.Len(t, rebuilt.SubAgents(), 1)
	for _, ts := range rebuilt.SubAgents()[0].ToolSets() {
		require.NoError(t, ts.(*tools.StartableToolSet).Start(t.Context()))
	}
	require.NoError(t, workspace.StopToolSets(t.Context()))
	assert.EqualValues(t, 1, stopped.stops.Load())
}

func TestForWorkingDirFailsWithoutReturningPartialTeam(t *testing.T) {
	t.Parallel()
	stopped := &stopRecordingToolSet{}
	first := agent.New("first", "", agent.WithWorkingDirFactory(func(ctx context.Context, _ string) (*agent.Agent, error) {
		a := agent.New("first", "", agent.WithToolSets(stopped))
		for _, ts := range a.ToolSets() {
			require.NoError(t, ts.(*tools.StartableToolSet).Start(ctx))
		}
		return a, nil
	}))
	failure := errors.New("cannot isolate")
	last := agent.New("last", "", agent.WithWorkingDirFactory(func(context.Context, string) (*agent.Agent, error) {
		return nil, failure
	}))
	rebuilt, err := New(WithAgents(first, last)).ForWorkingDir(t.Context(), "workspace")
	require.ErrorIs(t, err, failure)
	assert.Nil(t, rebuilt)
	assert.EqualValues(t, 1, stopped.stops.Load())
}

type workspaceEvaluator struct{}

func (*workspaceEvaluator) Evaluate(context.Context, any) (*evaluator.Result, error) {
	return &evaluator.Result{}, nil
}
