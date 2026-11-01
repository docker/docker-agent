package plan

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator binds file imports and exports to the workspace while sharing plans
// and change notifications with every other agent in the process.
func Creator(_ context.Context, _ latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	var workingDir string
	if runConfig != nil {
		workingDir = runConfig.WorkingDir
	}
	workingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, fmt.Errorf("resolving plan working directory: %w", err)
	}
	return &workspaceToolSet{shared: sharedToolSet(), workingDir: workingDir}, nil
}

type workspaceToolSet struct {
	shared     *ToolSet
	workingDir string
}

var (
	_ tools.ToolSet   = (*workspaceToolSet)(nil)
	_ tools.Unwrapper = (*workspaceToolSet)(nil)
)

// Unwrap preserves the shared notifier's identity for runtime deduplication.
func (t *workspaceToolSet) Unwrap() tools.ToolSet { return t.shared }

func (t *workspaceToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	all, err := t.shared.Tools(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		switch all[i].Name {
		case ToolNameUpdatePlanFromFile:
			all[i].Handler = tools.NewHandler(func(ctx context.Context, args UpdatePlanFromFileArgs) (*tools.ToolCallResult, error) {
				args.Path = t.resolvePath(args.Path)
				return t.shared.updatePlanFromFile(ctx, args)
			})
		case ToolNameExportPlanToFile:
			all[i].Handler = tools.NewHandler(func(ctx context.Context, args ExportPlanToFileArgs) (*tools.ToolCallResult, error) {
				args.Path = t.resolvePath(args.Path)
				return t.shared.exportPlanToFile(ctx, args)
			})
		}
	}
	return all, nil
}

func (t *workspaceToolSet) resolvePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(t.workingDir, path)
}
