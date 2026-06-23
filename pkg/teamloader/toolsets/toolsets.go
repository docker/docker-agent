package toolsets

import (
	"context"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/a2a"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/api"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/lsp"
	"github.com/docker/docker-agent/pkg/tools/builtin/mcpcatalog"
	"github.com/docker/docker-agent/pkg/tools/builtin/memory"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/openapi"
	"github.com/docker/docker-agent/pkg/tools/builtin/rag"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/tasks"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

func NewDefaultToolsetRegistry() teamloader.ToolsetRegistry {
	return teamloader.NewToolsetRegistry(DefaultToolsetCreators())
}

func DefaultToolsetCreators() map[string]teamloader.ToolsetCreator {
	return map[string]teamloader.ToolsetCreator{
		"todo": func(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return todo.CreateToolSet(toolset)
		},
		"tasks": func(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return tasks.CreateToolSet(toolset, parentDir, runConfig)
		},
		"memory": func(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
			return memory.CreateToolSet(toolset, parentDir, runConfig, configName)
		},
		"think": func(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return think.CreateToolSet()
		},
		"shell": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return shell.CreateToolSet(ctx, toolset, runConfig)
		},
		"script": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return shell.CreateScriptToolSet(ctx, toolset, runConfig)
		},
		"filesystem": func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return filesystem.CreateToolSet(toolset, runConfig)
		},
		"fetch": func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return fetch.CreateToolSet(toolset, runConfig)
		},
		"mcp": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return mcp.CreateToolSet(ctx, toolset, runConfig)
		},
		"mcp_catalog": func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			var opts []mcpcatalog.Option
			if len(toolset.AllowedServers) > 0 {
				opts = append(opts, mcpcatalog.WithAllowedServers(toolset.AllowedServers))
			}
			if len(toolset.BlockedServers) > 0 {
				opts = append(opts, mcpcatalog.WithBlockedServers(toolset.BlockedServers))
			}
			return mcpcatalog.New(opts...), nil
		},
		"api": func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return api.CreateToolSet(toolset, runConfig)
		},
		"a2a": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return a2a.CreateToolSet(ctx, toolset, runConfig)
		},
		"lsp": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return lsp.CreateToolSet(ctx, toolset, runConfig)
		},
		"user_prompt": func(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return userprompt.CreateToolSet()
		},
		"openapi": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return openapi.CreateToolSet(ctx, toolset, runConfig)
		},
		"model_picker": func(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return modelpicker.CreateToolSet(toolset)
		},
		"background_agents": func(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return agenttool.CreateToolSet()
		},
		"rag": func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return rag.CreateToolSet(ctx, toolset, parentDir, runConfig)
		},
	}
}
