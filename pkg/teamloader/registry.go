package teamloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
	"github.com/docker/cagent/pkg/gateway"
	"github.com/docker/cagent/pkg/js"
	"github.com/docker/cagent/pkg/memory/database/sqlite"
	"github.com/docker/cagent/pkg/path"
	"github.com/docker/cagent/pkg/paths"
	"github.com/docker/cagent/pkg/toolinstall"
	"github.com/docker/cagent/pkg/tools"
	"github.com/docker/cagent/pkg/tools/a2a"
	"github.com/docker/cagent/pkg/tools/builtin"
	"github.com/docker/cagent/pkg/tools/mcp"
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

// CreateTool creates a toolset using the registered creator for the given type
func (r *ToolsetRegistry) CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
	creator, ok := r.Get(toolset.Type)
	if !ok {
		return nil, fmt.Errorf("unknown toolset type: %s", toolset.Type)
	}
	return creator(ctx, toolset, parentDir, runConfig, agentName)
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
	return r
}

func createTodoTool(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if toolset.Shared {
		return builtin.NewSharedTodoTool(), nil
	}
	return builtin.NewTodoTool(), nil
}

func createTasksTool(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	toolsetPath := toolset.Path
	if toolsetPath == "" {
		toolsetPath = "tasks.json"
	}

	var basePath string
	if filepath.IsAbs(toolsetPath) {
		basePath = ""
	} else if wd := runConfig.WorkingDir; wd != "" {
		basePath = wd
	} else {
		basePath = parentDir
	}

	validatedPath, err := path.ValidatePathInDirectory(toolsetPath, basePath)
	if err != nil {
		return nil, fmt.Errorf("invalid tasks storage path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(validatedPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create tasks storage directory: %w", err)
	}

	return builtin.NewTasksTool(validatedPath), nil
}

func createMemoryTool(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	var validatedMemoryPath string

	if toolset.Path != "" {
		// Explicit path provided - resolve relative to working dir or parent dir
		var basePath string
		if filepath.IsAbs(toolset.Path) {
			basePath = ""
		} else if wd := runConfig.WorkingDir; wd != "" {
			basePath = wd
		} else {
			basePath = parentDir
		}

		var err error
		validatedMemoryPath, err = path.ValidatePathInDirectory(toolset.Path, basePath)
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

	return builtin.NewMemoryToolWithPath(db, validatedMemoryPath), nil
}

func createThinkTool(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return builtin.NewThinkTool(), nil
}

func createShellTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
	}
	env = append(env, os.Environ()...)

	return builtin.NewShellTool(env, runConfig), nil
}

func createScriptTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if len(toolset.Shell) == 0 {
		return nil, fmt.Errorf("shell is required for script toolset")
	}

	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
	}
	env = append(env, os.Environ()...)
	return builtin.NewScriptShellTool(toolset.Shell, env)
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

	var opts []builtin.FileSystemOpt

	// Handle ignore_vcs configuration (default to true)
	ignoreVCS := true
	if toolset.IgnoreVCS != nil {
		ignoreVCS = *toolset.IgnoreVCS
	}
	opts = append(opts, builtin.WithIgnoreVCS(ignoreVCS))

	// Handle post-edit commands
	if len(toolset.PostEdit) > 0 {
		postEditConfigs := make([]builtin.PostEditConfig, len(toolset.PostEdit))
		for i, pe := range toolset.PostEdit {
			postEditConfigs[i] = builtin.PostEditConfig{
				Path: pe.Path,
				Cmd:  pe.Cmd,
			}
		}
		opts = append(opts, builtin.WithPostEditCommands(postEditConfigs))
	}

	return builtin.NewFilesystemTool(wd, opts...), nil
}

func createAPITool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if toolset.APIConfig.Endpoint == "" {
		return nil, fmt.Errorf("api tool requires an endpoint in api_config")
	}

	expander := js.NewJsExpander(runConfig.EnvProvider())
	toolset.APIConfig.Endpoint = expander.Expand(ctx, toolset.APIConfig.Endpoint, nil)
	toolset.APIConfig.Headers = expander.ExpandMap(ctx, toolset.APIConfig.Headers)

	return builtin.NewAPITool(toolset.APIConfig, expander), nil
}

func createFetchTool(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	var opts []builtin.FetchToolOption
	if toolset.Timeout > 0 {
		timeout := time.Duration(toolset.Timeout) * time.Second
		opts = append(opts, builtin.WithTimeout(timeout))
	}
	return builtin.NewFetchTool(opts...), nil
}

func createMCPTool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	envProvider := runConfig.EnvProvider()

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
			return mcp.NewRemoteToolset(toolset.Name, serverSpec.Remote.URL, serverSpec.Remote.TransportType, nil), nil
		}

		env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), envProvider)
		if err != nil {
			return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
		}

		envProvider := environment.NewMultiProvider(
			environment.NewEnvListProvider(env),
			envProvider,
		)

		return mcp.NewGatewayToolset(ctx, toolset.Name, mcpServerName, toolset.Config, envProvider, runConfig.WorkingDir)

	// STDIO MCP Server from shell command
	case toolset.Command != "":
		// Auto-install missing command binary if needed
		resolvedCommand, err := toolinstall.EnsureCommand(ctx, toolset.Command, toolset.Version)
		if err != nil {
			return nil, fmt.Errorf("resolving command %q: %w", toolset.Command, err)
		}

		env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), envProvider)
		if err != nil {
			return nil, fmt.Errorf("failed to expand the tool's environment variables: %w", err)
		}
		env = append(env, os.Environ()...)

		// Prepend tools bin dir to PATH so child processes can find installed tools
		env = toolinstall.PrependBinDirToEnv(env)

		return mcp.NewToolsetCommand(toolset.Name, resolvedCommand, toolset.Args, env, runConfig.WorkingDir), nil

	// Remote MCP Server
	case toolset.Remote.URL != "":
		expander := js.NewJsExpander(envProvider)

		headers := expander.ExpandMap(ctx, toolset.Remote.Headers)
		url := expander.Expand(ctx, toolset.Remote.URL, nil)

		return mcp.NewRemoteToolset(toolset.Name, url, toolset.Remote.TransportType, headers), nil

	default:
		return nil, fmt.Errorf("mcp toolset requires either ref, command, or remote configuration")
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

	tool := builtin.NewLSPTool(resolvedCommand, toolset.Args, env, runConfig.WorkingDir)
	if len(toolset.FileTypes) > 0 {
		tool.SetFileTypes(toolset.FileTypes)
	}

	return tool, nil
}

func createUserPromptTool(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return builtin.NewUserPromptTool(), nil
}

func createOpenAPITool(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	expander := js.NewJsExpander(runConfig.EnvProvider())

	specURL := expander.Expand(ctx, toolset.URL, nil)
	headers := expander.ExpandMap(ctx, toolset.Headers)

	return builtin.NewOpenAPITool(specURL, headers), nil
}

func createModelPickerTool(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if len(toolset.Models) == 0 {
		return nil, fmt.Errorf("model_picker toolset requires at least one model")
	}
	return builtin.NewModelPickerTool(toolset.Models), nil
}
