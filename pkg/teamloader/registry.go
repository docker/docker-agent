package teamloader

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/memory/database/sqlite"
	"github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/toolinstall"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/a2a"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/api"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/lsp"
	"github.com/docker/docker-agent/pkg/tools/builtin/memory"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/openapi"
	builtinrag "github.com/docker/docker-agent/pkg/tools/builtin/rag"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/tasks"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

// ToolsetCreator is a function that creates a toolset based on the provided configuration.
// configName identifies the agent config file (e.g. "memory_agent" from "memory_agent.yaml").
type ToolsetCreator func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error)

// ToolsetRegistry manages the registration of toolset creators by type
type ToolsetRegistry struct {
	creators map[string]ToolsetCreator
}

// NewToolsetRegistry creates a new empty toolset registry
func NewToolsetRegistry() *ToolsetRegistry {
	return &ToolsetRegistry{
		creators: make(map[string]ToolsetCreator),
	}
}

// Register adds a new toolset creator for the given type
func (r *ToolsetRegistry) Register(toolsetType string, creator ToolsetCreator) {
	r.creators[toolsetType] = creator
}

// Get retrieves a toolset creator for the given type
func (r *ToolsetRegistry) Get(toolsetType string) (ToolsetCreator, bool) {
	creator, ok := r.creators[toolsetType]
	return creator, ok
}

// CreateTool creates a toolset using the registered creator for the given type.
//
// Every successful toolset is decorated with tools.WithName so status
// surfaces (the /tools dialog, error messages, …) always have a stable
// user-facing label. The decoration is a no-op for toolsets that
// already advertise a non-empty Name(): it only fills the gap left by
// built-in toolsets that don't take a `name:` field in YAML, replacing
// the previous fallback to fmt.Sprintf("%T", ts).
func (r *ToolsetRegistry) CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
	creator, ok := r.Get(toolset.Type)
	if !ok {
		return nil, fmt.Errorf("unknown toolset type: %s", toolset.Type)
	}
	ts, err := creator(ctx, toolset, parentDir, runConfig, agentName)
	if err != nil {
		return nil, err
	}
	return tools.WithName(ts, cmp.Or(toolset.Name, toolset.Type)), nil
}

func NewDefaultToolsetRegistry() *ToolsetRegistry {
	r := NewToolsetRegistry()
	// Register all built-in toolset creators
	r.Register("todo", createTodoTool)
	r.Register("tasks", createTasksTool)
	r.Register("memory", createMemoryTool)
	r.Register("think", createThinkTool)
	r.Register("shell", createShellTool)
	r.Register("script", createScriptTool)
	r.Register("filesystem", createFilesystemTool)
	r.Register("fetch", createFetchTool)
	r.Register("mcp", createMCPTool)
	r.Register("api", createAPITool)
	r.Register("a2a", createA2ATool)
	r.Register("lsp", createLSPTool)
	r.Register("user_prompt", createUserPromptTool)
	r.Register("openapi", createOpenAPITool)
	r.Register("model_picker", createModelPickerTool)
	r.Register("background_agents", createBackgroundAgentsTool)
	r.Register("rag", createRAGTool)
	return r
}

// checkDirExists returns an error if the given directory does not exist or is
// not a directory. toolsetType is used only in the error message.
func checkDirExists(dir, toolsetType string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("working_dir %q for %s toolset does not exist", dir, toolsetType)
		}
		return fmt.Errorf("working_dir %q for %s toolset: %w", dir, toolsetType, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working_dir %q for %s toolset is not a directory", dir, toolsetType)
	}
	return nil
}

// resolveToolsetWorkingDir returns the effective working directory for a toolset process.
//
// Resolution rules:
//   - If toolsetWorkingDir is empty, agentWorkingDir is returned unchanged.
//   - Shell patterns (~ and ${VAR}/$VAR) are expanded before any further processing.
//   - If the expanded path is absolute, it is returned as-is.
//   - If the expanded path is relative and agentWorkingDir is non-empty,
//     it is joined with agentWorkingDir and made absolute via filepath.Abs.
//   - If the expanded path is relative and agentWorkingDir is empty,
//     the relative path is returned unchanged (caller will inherit the process cwd).
//
// Note: unlike resolveToolsetPath, this helper does not enforce containment
// within the agent working directory. working_dir is treated like command/args —
// a trusted, operator-authored value where cross-tree references (e.g. a sibling
// module root in a monorepo) are intentional and must not be silently blocked.
func resolveToolsetWorkingDir(toolsetWorkingDir, agentWorkingDir string) string {
	if toolsetWorkingDir == "" {
		return agentWorkingDir
	}
	// Expand ~ and environment variables before path operations.
	toolsetWorkingDir = path.ExpandPath(toolsetWorkingDir)
	if filepath.IsAbs(toolsetWorkingDir) {
		return toolsetWorkingDir
	}
	if agentWorkingDir != "" {
		// filepath.Abs cleans the result and anchors the URI correctly
		// (avoids file://./backend-style LSP root URIs when the agent dir
		// is itself absolute, which is the normal case).
		abs, err := filepath.Abs(filepath.Join(agentWorkingDir, toolsetWorkingDir))
		if err == nil {
			return abs
		}
		// Fallback: return the joined path without Abs (should not happen in practice).
		return filepath.Join(agentWorkingDir, toolsetWorkingDir)
	}
	// agentWorkingDir is empty and path is relative: return as-is.
	// The child process will inherit the OS working directory.
	return toolsetWorkingDir
}

// resolveToolsetPath expands shell patterns (~, env vars) in the given path,
// then validates it relative to the working directory or parent directory.
func resolveToolsetPath(toolsetPath, parentDir string, runConfig *config.RuntimeConfig) (string, error) {
	toolsetPath = path.ExpandPath(toolsetPath)

	var basePath string
	if filepath.IsAbs(toolsetPath) {
		basePath = ""
	} else if wd := runConfig.WorkingDir; wd != "" {
		basePath = wd
	} else {
		basePath = parentDir
	}

	return path.ValidatePathInDirectory(toolsetPath, basePath)
}

func createTodoTool(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if toolset.Shared {
		return todo.NewSharedTodoTool(), nil
	}
	return todo.NewTodoTool(), nil
}

func createTasksTool(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	toolsetPath := toolset.Path
	if toolsetPath == "" {
		toolsetPath = "tasks.json"
	}

	validatedPath, err := resolveToolsetPath(toolsetPath, parentDir, runConfig)
	if err != nil {
		return nil, fmt.Errorf("invalid tasks storage path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(validatedPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create tasks storage directory: %w", err)
	}

	return tasks.NewTasksTool(validatedPath), nil
}

func createMemoryTool(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	var validatedMemoryPath string

	if toolset.Path != "" {
		var err error
		validatedMemoryPath, err = resolveToolsetPath(toolset.Path, parentDir, runConfig)
		if err != nil {
			return nil, fmt.Errorf("invalid memory database path: %w", err)
		}
	} else {
		// Default: ~/.cagent/memory/<configName>/memory.db
		if configName == "" {
			configName = "default"
		}
		validatedMemoryPath = filepath.Join(paths.GetDataDir(), "memory", configName, "memory.db")
	}

	if err := os.MkdirAll(filepath.Dir(validatedMemoryPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create memory database directory: %w", err)
	}

	db, err := sqlite.NewMemoryDatabase(validatedMemoryPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create memory database: %w", err)
	}

	return memory.NewMemoryToolWithPath(db, validatedMemoryPath), nil
}

func createThinkTool(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return think.NewThinkTool(), nil
}

func createShellTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
	}
	env = append(env, os.Environ()...)

	return shell.NewShellTool(env, runConfig), nil
}

func createScriptTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if len(toolset.Shell) == 0 {
		return nil, errors.New("shell is required for script toolset")
	}

	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
	}
	env = append(env, os.Environ()...)
	return shell.NewScriptShellTool(toolset.Shell, env)
}

func createFilesystemTool(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	wd := runConfig.WorkingDir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
	}

	var opts []filesystem.Opt

	// Handle ignore_vcs configuration (default to true)
	ignoreVCS := true
	if toolset.IgnoreVCS != nil {
		ignoreVCS = *toolset.IgnoreVCS
	}
	opts = append(opts, filesystem.WithIgnoreVCS(ignoreVCS))

	// Handle allow/deny lists for filesystem operations.
	// An empty / nil list preserves the default behaviour (no restriction).
	if len(toolset.AllowList) > 0 {
		opts = append(opts, filesystem.WithAllowList(toolset.AllowList))
	}
	if len(toolset.DenyList) > 0 {
		opts = append(opts, filesystem.WithDenyList(toolset.DenyList))
	}

	// Handle post-edit commands
	if len(toolset.PostEdit) > 0 {
		postEditConfigs := make([]filesystem.PostEditConfig, len(toolset.PostEdit))
		for i, pe := range toolset.PostEdit {
			postEditConfigs[i] = filesystem.PostEditConfig{
				Path: pe.Path,
				Cmd:  pe.Cmd,
			}
		}
		opts = append(opts, filesystem.WithPostEditCommands(postEditConfigs))
	}

	return filesystem.NewFilesystemTool(wd, opts...), nil
}

func createAPITool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if toolset.APIConfig.Endpoint == "" {
		return nil, errors.New("api tool requires an endpoint in api_config")
	}

	expander := js.NewJsExpander(runConfig.EnvProvider())
	toolset.APIConfig.Endpoint = expander.Expand(ctx, toolset.APIConfig.Endpoint, nil)
	toolset.APIConfig.Headers = expander.ExpandMap(ctx, toolset.APIConfig.Headers)

	return api.NewAPITool(toolset.APIConfig, expander), nil
}

func createFetchTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	// Expand ${env.X} in headers so secrets (API tokens, ...) can come from
	// the environment instead of being inlined in YAML — same behaviour as
	// openapi/a2a/mcp.remote/api headers. ExpandMap and WithHeaders are both
	// nil-safe, so no guard is needed when the user hasn't configured any.
	expander := js.NewJsExpander(runConfig.EnvProvider())

	var opts []fetch.ToolOption
	if toolset.Timeout > 0 {
		timeout := time.Duration(toolset.Timeout) * time.Second
		opts = append(opts, fetch.WithTimeout(timeout))
	}
	if len(toolset.AllowedDomains) > 0 {
		opts = append(opts, fetch.WithAllowedDomains(toolset.AllowedDomains))
	}
	if len(toolset.BlockedDomains) > 0 {
		opts = append(opts, fetch.WithBlockedDomains(toolset.BlockedDomains))
	}
	if toolset.AllowPrivateIPs {
		opts = append(opts, fetch.WithAllowPrivateIPs(true))
	}
	opts = append(opts, fetch.WithHeaders(expander.ExpandMap(ctx, toolset.Headers)))
	return fetch.NewFetchTool(opts...), nil
}

func createMCPTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	envProvider := runConfig.EnvProvider()

	// Resolve the working directory once; used for all subprocess-based branches.
	// Note: validation only rejects working_dir for toolsets with an explicit
	// remote.url. Ref-based MCPs (e.g. ref: docker:context7) pass validation
	// regardless, because their transport type is only known at runtime via the
	// MCP Catalog API. If such a ref resolves to a remote server at runtime, we
	// return an explicit error below rather than silently discarding the field.
	cwd := resolveToolsetWorkingDir(toolset.WorkingDir, runConfig.WorkingDir)

	// S1: validate the resolved directory exists (if one was specified) so we
	// surface a clear error now rather than a cryptic exec failure later.
	// Skip this check for ref-based toolsets whose transport type is not yet
	// known — the check would be premature and potentially wrong.
	if toolset.WorkingDir != "" && toolset.Ref == "" {
		if err := checkDirExists(cwd, "mcp"); err != nil {
			return nil, err
		}
	}

	switch {
	// MCP Server from the MCP Catalog, running with the MCP Gateway
	case toolset.Ref != "":
		mcpServerName := gateway.ParseServerRef(toolset.Ref)
		serverSpec, err := gateway.ServerSpec(ctx, mcpServerName)
		if err != nil {
			return nil, fmt.Errorf("fetching MCP server spec for %q: %w", mcpServerName, err)
		}

		// TODO(dga): until the MCP Gateway supports oauth with docker agent, we fetch the remote url and directly connect to it.
		if serverSpec.Type == "remote" {
			// working_dir cannot be validated at config-parse time for ref-based
			// MCPs because their transport type is only known here. Return a clear
			// error rather than silently discarding the field.
			if toolset.WorkingDir != "" {
				return nil, fmt.Errorf("working_dir is not supported for MCP toolset %q: ref %q resolves to a remote server (no local subprocess)",
					toolset.Name, toolset.Ref)
			}
			return mcp.NewRemoteToolset(toolset.Name, serverSpec.Remote.URL, serverSpec.Remote.TransportType, nil, nil, lifecyclePolicyFromConfig(toolset.Name, toolset.Lifecycle)), nil
		}

		// The ref resolves to a local subprocess — validate the working directory now.
		if toolset.WorkingDir != "" {
			if err := checkDirExists(cwd, "mcp"); err != nil {
				return nil, err
			}
		}

		env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), envProvider)
		if err != nil {
			return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
		}

		envProvider := environment.NewMultiProvider(
			environment.NewEnvListProvider(env),
			envProvider,
		)

		// Pass the resolved cwd so gateway-based MCPs also honour working_dir.
		return mcp.NewGatewayToolset(ctx, toolset.Name, mcpServerName, serverSpec.Secrets, toolset.Config, envProvider, cwd)

	// STDIO MCP Server from shell command
	case toolset.Command != "":
		// Auto-install missing command binary if needed.
		// If EnsureCommand fails (binary not on PATH, no aqua package, etc.),
		// treat as transient: create the toolset with the original command
		// and let mcp.Toolset.Start() retry on each conversation turn.
		resolvedCommand, err := toolinstall.EnsureCommand(ctx, toolset.Command, toolset.Version)
		if err != nil {
			slog.WarnContext(ctx, "MCP command not yet available, will retry on next turn",
				"command", toolset.Command, "error", err)
			resolvedCommand = toolset.Command
		}

		env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), envProvider)
		if err != nil {
			return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
		}
		env = append(env, os.Environ()...)

		// Prepend tools bin dir to PATH so child processes can find installed tools
		env = toolinstall.PrependBinDirToEnv(env)

		return mcp.NewToolsetCommand(toolset.Name, resolvedCommand, toolset.Args, env, cwd, lifecyclePolicyFromConfig(toolset.Name, toolset.Lifecycle)), nil

	// Remote MCP Server — working_dir is rejected at validation time for this
	// branch (explicit remote.url in config). Ref-based MCPs that resolve to
	// remote at runtime are handled with an explicit error in the Ref branch above.
	case toolset.Remote.URL != "":
		expander := js.NewJsExpander(envProvider)

		headers := expander.ExpandMap(ctx, toolset.Remote.Headers)
		url := expander.Expand(ctx, toolset.Remote.URL, nil)

		return mcp.NewRemoteToolset(toolset.Name, url, toolset.Remote.TransportType, headers, toolset.Remote.OAuth, lifecyclePolicyFromConfig(toolset.Name, toolset.Lifecycle)), nil

	default:
		return nil, errors.New("mcp toolset requires either ref, command, or remote configuration")
	}
}

func createA2ATool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	expander := js.NewJsExpander(runConfig.EnvProvider())

	headers := expander.ExpandMap(ctx, toolset.Headers)

	return a2a.NewToolset(toolset.Name, toolset.URL, headers), nil
}

func createLSPTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	// Auto-install missing command binary if needed
	resolvedCommand, err := toolinstall.EnsureCommand(ctx, toolset.Command, toolset.Version)
	if err != nil {
		return nil, fmt.Errorf("resolving command %q: %w", toolset.Command, err)
	}

	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
	}
	env = append(env, os.Environ()...)

	// Prepend tools bin dir to PATH so child processes can find installed tools
	env = toolinstall.PrependBinDirToEnv(env)

	cwd := resolveToolsetWorkingDir(toolset.WorkingDir, runConfig.WorkingDir)

	// S1: validate the resolved directory exists (if one was specified) so we
	// surface a clear error now rather than a cryptic exec failure later.
	if toolset.WorkingDir != "" {
		if err := checkDirExists(cwd, "lsp"); err != nil {
			return nil, err
		}
	}

	tool := lsp.NewLSPTool(resolvedCommand, toolset.Args, env, cwd, lifecyclePolicyFromConfig(toolset.Name, toolset.Lifecycle))
	if len(toolset.FileTypes) > 0 {
		tool.SetFileTypes(toolset.FileTypes)
	}

	return tool, nil
}

func createUserPromptTool(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return userprompt.NewUserPromptTool(), nil
}

func createOpenAPITool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	expander := js.NewJsExpander(runConfig.EnvProvider())

	specURL := expander.Expand(ctx, toolset.URL, nil)
	headers := expander.ExpandMap(ctx, toolset.Headers)

	return openapi.NewOpenAPITool(specURL, headers), nil
}

func createModelPickerTool(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if len(toolset.Models) == 0 {
		return nil, errors.New("model_picker toolset requires at least one model")
	}
	return modelpicker.NewModelPickerTool(toolset.Models), nil
}

func createBackgroundAgentsTool(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return agenttool.NewToolSet(), nil
}

func createRAGTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if toolset.RAGConfig == nil {
		return nil, errors.New("rag toolset requires rag_config (should have been resolved from ref)")
	}

	ragName := cmp.Or(toolset.Name, "rag")

	mgr, err := rag.NewManager(ctx, ragName, toolset.RAGConfig, rag.ManagersBuildConfig{
		ParentDir:     parentDir,
		ModelsGateway: runConfig.ModelsGateway,
		Env:           runConfig.EnvProvider(),
		Models:        runConfig.Models,
		Providers:     runConfig.Providers,
		RuntimeConfig: runConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create RAG manager: %w", err)
	}

	toolName := cmp.Or(mgr.ToolName(), ragName)
	return builtinrag.NewRAGTool(mgr, toolName), nil
}
