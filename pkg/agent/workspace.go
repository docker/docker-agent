package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/docker/docker-agent/pkg/tools"
)

// ForWorkingDir builds an independent agent for dir, reusing provider clients
// and snapshotting runtime model and harness-effort overrides.
func (a *Agent) ForWorkingDir(ctx context.Context, dir string) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.workingDirFactory == nil {
		return nil, fmt.Errorf("agent %q has no working directory factory", a.name)
	}
	rebuilt, err := a.workingDirFactory(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("agent %q: creating workspace: %w", a.name, err)
	}
	if rebuilt == nil || rebuilt == a {
		return nil, fmt.Errorf("agent %q: working directory factory must return a new agent", a.name)
	}
	rebuilt.name = a.name
	rebuilt.workingDirFactory = a.workingDirFactory
	if overrides := a.modelOverrides.Load(); overrides != nil {
		models := slices.Clone(*overrides)
		rebuilt.modelOverrides.Store(&models)
	}
	if effort := a.harnessEffort.Load(); effort != nil {
		rebuilt.SetHarnessEffort(*effort)
	}
	return rebuilt, nil
}

// Dispose permanently releases all workspace tools, including never-started
// resources. Unlike StopToolSets, the disposed agent must not be reused.
func (a *Agent) Dispose(ctx context.Context) error {
	return tools.Dispose(ctx, a.DisposableToolSets()...)
}

// DisposableToolSets includes both ordinary and fork-skill lifecycle owners.
// Teams dispose them together so shared resources are released only once.
func (a *Agent) DisposableToolSets() []tools.ToolSet {
	var result []tools.ToolSet
	for _, ts := range slices.Concat(a.toolsets, a.ownedToolsets) {
		result = append(result, ts)
	}
	return result
}
