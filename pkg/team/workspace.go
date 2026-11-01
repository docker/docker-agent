package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/tools"
)

// ForWorkingDir rebuilds the whole agent graph before reconnecting its links.
// A failed rebuild never returns a partially isolated team.
func (t *Team) ForWorkingDir(ctx context.Context, dir string) (_ *Team, err error) {
	originals := slices.Clone(t.agents)
	copies := make(map[*agent.Agent]*agent.Agent)
	var sources, built []*agent.Agent
	defer func() {
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err = errors.Join(err, New(WithAgents(built...)).Dispose(cleanupCtx))
		}
	}()

	// Imported agents can link to agents not listed as members of this team.
	for i := 0; i < len(originals); i++ {
		original := originals[i]
		if _, exists := copies[original]; exists {
			continue
		}
		rebuilt, err := original.ForWorkingDir(ctx, dir)
		if err != nil {
			return nil, err
		}
		sources = append(sources, original)
		copies[original] = rebuilt
		built = append(built, rebuilt)
		originals = append(originals, original.SubAgents()...)
		originals = append(originals, original.Handoffs()...)
		if target := original.ForceHandoff(); target != nil {
			originals = append(originals, target)
		}
	}

	for _, original := range sources {
		rebuilt := copies[original]
		var subs []*agent.Agent
		for _, sub := range original.SubAgents() {
			subs = append(subs, copies[sub])
		}
		agent.WithSubAgents(subs...)(rebuilt)
		var handoffs []*agent.Agent
		for _, target := range original.Handoffs() {
			handoffs = append(handoffs, copies[target])
		}
		agent.WithHandoffs(handoffs...)(rebuilt)
		agent.WithForceHandoff(copies[original.ForceHandoff()])(rebuilt)
		agent.WithRouting(original.Routing())(rebuilt)
	}

	members := make([]*agent.Agent, len(t.agents))
	for i, original := range t.agents {
		members[i] = copies[original]
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("creating workspace team: %w", err)
	}
	workspace := New(
		WithAgents(members...),
		WithPermissions(t.permissions),
		WithEvaluators(t.evaluators),
		WithAgentConfigs(t.agentConfigs),
		WithRuntimeSafety(t.runtimeSafety),
	)
	for _, original := range sources {
		if !slices.Contains(t.agents, original) {
			workspace.linkedAgents = append(workspace.linkedAgents, copies[original])
		}
	}
	return workspace, nil
}

// Dispose permanently releases all workspace resources after execution ends,
// including tools that were never started. StopToolSets remains restartable.
func (t *Team) Dispose(ctx context.Context) error {
	var toolsets []tools.ToolSet
	seen := make(map[*agent.Agent]bool)
	agents := slices.Concat(t.agents, t.linkedAgents)
	for i := 0; i < len(agents); i++ {
		a := agents[i]
		if a == nil || seen[a] {
			continue
		}
		seen[a] = true
		toolsets = append(toolsets, a.DisposableToolSets()...)
		agents = append(agents, a.SubAgents()...)
		agents = append(agents, a.Handoffs()...)
		agents = append(agents, a.ForceHandoff())
	}
	return tools.Dispose(ctx, toolsets...)
}
