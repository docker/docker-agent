//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/memory/database/inmemory"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/api/client"
	"github.com/docker/docker-agent/pkg/tools/builtin/calculator"
	"github.com/docker/docker-agent/pkg/tools/builtin/datetime"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/memory"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/openapi"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/random"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessioncontext"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// browserToolsets builds the toolset registry for one session: remote MCP
// servers plus the builtins that need neither a process, a filesystem nor a
// socket. HTTP tools go through the same SSRF-guarded transport as MCP, so
// they need toolProxy (or allow_private_ips) in a browser.
//
// A registry is built per session because todo (shared: true), plan and
// memory keep state: the agents of one team share it, two sessions never do.
// rag indexes the session's documents in place of files.
func browserToolsets(documents map[string][]byte) teamloader.ToolsetRegistry {
	return teamloader.NewToolsetRegistry(browserToolsetCreators(documents))
}

func browserToolsetCreators(documents map[string][]byte) map[string]teamloader.ToolsetCreator {
	sharedTodos := sync.OnceValue(func() *todo.ToolSet { return todo.New() })
	plans := sync.OnceValue(func() tools.ToolSet {
		// Plans live in memory and there are no files to move them through.
		return teamloader.WithToolsExcludeFilter(
			plan.New(plan.WithStorage(plan.NewMemoryStorage())),
			plan.ToolNameUpdatePlanFromFile, plan.ToolNameExportPlanToFile,
		)
	})
	memories := sync.OnceValue(inmemory.New)

	creators := map[string]teamloader.ToolsetCreator{
		"datetime":   teamloader.Creator(datetime.CreateToolSet),
		"mcp":        mcpCreator,
		"calculator": teamloader.Creator(calculator.CreateToolSet),
		"think":      teamloader.Creator(think.CreateToolSet),
		"random":     teamloader.Creator(random.CreateToolSet),
		"todo": teamloader.CreatorFromToolset(func(ts latest.Toolset) (tools.ToolSet, error) {
			if ts.Shared != latest.SharedScopeNone {
				return sharedTodos(), nil
			}
			return todo.New(), nil
		}),
		"plan": teamloader.Creator(func() (tools.ToolSet, error) { return plans(), nil }),
		"memory": func(_ context.Context, ts latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			if err := checkMemory(ts); err != nil {
				return nil, err
			}
			return browserMemory(memories()), nil
		},
		"user_prompt":     teamloader.Creator(userprompt.CreateToolSet),
		"session_context": teamloader.Creator(sessioncontext.CreateToolSet),
		"fetch":           fetch.Creator,
		"api":             client.Creator(js.NewJsExpander),
		"openapi":         openapiCreator,
		"model_picker":    teamloader.CreatorFromToolset(modelpicker.CreateToolSet),
	}
	registerRAG(creators, documents)
	return creators
}

// memoryPersistenceClaim opens the memory toolset's instructions and is
// false in the browser; browserMemory swaps it for browserMemoryScope and
// keeps the recall/store guidance.
const (
	memoryPersistenceClaim = "You have persistent memory that survives across sessions."
	browserMemoryScope     = "You have memory scoped to this session: it survives turns and a restart of the session, but is lost when the session is closed, a new session is created or the page is reloaded."
)

func browserMemory(db memory.DB) tools.ToolSet {
	ts := memory.New(db)
	return teamloader.WithInstructions(ts, strings.Replace(ts.Instructions(), memoryPersistenceClaim, browserMemoryScope, 1))
}

func mcpCreator(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	if err := checkRemoteMCP(toolset); err != nil {
		return nil, err
	}
	return mcptools.Creator(ctx, toolset, parentDir, runConfig, configName)
}

func openapiCreator(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	if err := checkOpenAPI(ctx, toolset, runConfig.EnvProvider()); err != nil {
		return nil, err
	}
	return openapi.Creator(ctx, toolset, parentDir, runConfig, configName)
}

// checkBrowserToolset rejects toolset declarations the browser build cannot
// honour, so they fail the load instead of being skipped with a warning.
func checkBrowserToolset(ctx context.Context, ts latest.Toolset, env environment.Provider, documents map[string][]byte) error {
	switch ts.Type {
	case "mcp":
		return checkRemoteMCP(ts)
	case "memory":
		return checkMemory(ts)
	case "openapi":
		return checkOpenAPI(ctx, ts, env)
	case "rag":
		return checkRAG(ts, documents)
	}
	return nil
}

// checkRemoteMCP accepts only MCP toolsets that connect to a remote server:
// stdio servers and catalog references need a host process, and the
// local-only fields would otherwise be ignored silently.
func checkRemoteMCP(ts latest.Toolset) error {
	switch {
	case ts.Command != "" || len(ts.Args) > 0:
		return errors.New("stdio MCP servers need a host process; only remote servers are supported in the browser")
	case ts.Ref != "":
		return errors.New("MCP catalog references need the Docker MCP gateway; only remote servers are supported in the browser")
	case ts.Remote.URL == "":
		return errors.New("mcp toolset requires remote.url in the browser")
	case ts.WorkingDir != "" || len(ts.Env) > 0 || ts.Config != nil || ts.Version != "" || ts.Path != "":
		return errors.New("working_dir, env, config, version and path only apply to local MCP servers")
	}
	return nil
}

// checkMemory rejects a database path: memories live in the session.
func checkMemory(ts latest.Toolset) error {
	if ts.Path != "" {
		return errors.New("memory path: local files are not available in the browser; memories are kept in memory for the session")
	}
	return nil
}

// checkOpenAPI accepts only specs fetched over HTTP(S). The URL is expanded
// with the session env first, like the toolset itself does.
func checkOpenAPI(ctx context.Context, ts latest.Toolset, env environment.Provider) error {
	spec := js.NewJsExpander(env).Expand(ctx, ts.URL, nil)
	u, err := url.Parse(spec)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("openapi url %q: only http(s) specs can be fetched in the browser", spec)
	}
	return nil
}
