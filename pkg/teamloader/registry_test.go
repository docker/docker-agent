package teamloader

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/lsp"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	mcptool "github.com/docker/docker-agent/pkg/tools/mcp"
)

func testToolsetRegistry() ToolsetRegistry {
	return NewToolsetRegistry(map[string]ToolsetCreator{
		"shell": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return shell.CreateToolSet(ctx, toolset, runConfig)
		},
		"mcp": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return mcptool.CreateToolSet(ctx, toolset, runConfig)
		},
		"fetch": func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return fetch.CreateToolSet(toolset, runConfig)
		},
		"lsp": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return lsp.CreateToolSet(ctx, toolset, runConfig)
		},
	})
}

func TestCreateShellTool(t *testing.T) {
	toolset := latest.Toolset{
		Type: "shell",
	}

	registry := testToolsetRegistry()

	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
}

func TestCreateMCPTool_CommandNotFound_CreatesToolsetAnyway(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	toolset := latest.Toolset{
		Type:    "mcp",
		Command: "./bin/nonexistent-mcp-server",
	}

	registry := testToolsetRegistry()

	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
	assert.Equal(t, "mcp(stdio cmd=./bin/nonexistent-mcp-server)", tools.DescribeToolSet(tool))
}

func TestCreateMCPTool_BareCommandNotFound_CreatesToolsetAnyway(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	toolset := latest.Toolset{
		Type:    "mcp",
		Command: "some-nonexistent-mcp-binary",
	}

	registry := testToolsetRegistry()

	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
	assert.Equal(t, "mcp(stdio cmd=some-nonexistent-mcp-binary)", tools.DescribeToolSet(tool))
} // TestCreateMCPTool_WorkingDir_ReachesSubprocess verifies that working_dir is
// wired all the way through createMCPTool to the underlying stdio command (N5).
func TestCreateMCPTool_WorkingDir_ReachesSubprocess(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	// Create a real temporary directory so the existence check passes.
	customDir := t.TempDir()
	agentDir := t.TempDir()

	toolset := latest.Toolset{
		Type:       "mcp",
		Command:    "some-nonexistent-mcp-binary",
		WorkingDir: customDir,
	}

	registry := testToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: agentDir},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	rawTool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, rawTool)

	// Assert the CWD reached the inner stdio command.
	ts, ok := tools.As[*mcptool.Toolset](rawTool)
	require.True(t, ok, "expected *mcp.Toolset")
	assert.Equal(t, customDir, ts.WorkingDir())
}

// TestCreateMCPTool_RelativeWorkingDir_ResolvedAgainstAgentDir verifies that a
// relative working_dir is resolved against the agent's working directory.
func TestCreateMCPTool_RelativeWorkingDir_ResolvedAgainstAgentDir(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	agentDir := t.TempDir()
	// Create the subdirectory so the existence check passes.
	subDir := filepath.Join(agentDir, "tools", "mcp")
	require.NoError(t, os.MkdirAll(subDir, 0o700))

	toolset := latest.Toolset{
		Type:       "mcp",
		Command:    "some-nonexistent-mcp-binary",
		WorkingDir: "tools/mcp",
	}

	registry := testToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: agentDir},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	rawTool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, rawTool)

	ts, ok := tools.As[*mcptool.Toolset](rawTool)
	require.True(t, ok, "expected *mcp.Toolset")
	assert.Equal(t, subDir, ts.WorkingDir())
}

// TestCreateMCPTool_NonexistentWorkingDir_ReturnsError verifies that a
// non-existent working_dir surfaces a clear error at tool-creation time (S1).
func TestCreateMCPTool_NonexistentWorkingDir_ReturnsError(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	// Use a path that is guaranteed not to exist by creating a tempdir and
	// appending a non-existent subdir (avoids flakes on hosts where a
	// hard-coded path might coincidentally exist).
	nonExistent := filepath.Join(t.TempDir(), "missing")

	toolset := latest.Toolset{
		Type:       "mcp",
		Command:    "some-nonexistent-mcp-binary",
		WorkingDir: nonExistent,
	}

	registry := testToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	_, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "working_dir")
	assert.Contains(t, err.Error(), "does not exist")
}

// TestCreateLSPTool_WorkingDir_ReachesHandler verifies that working_dir is
// wired all the way through createLSPTool to the LSP handler (N5).
func TestCreateLSPTool_WorkingDir_ReachesHandler(t *testing.T) {
	// Create a real temporary directory so the existence check passes.
	customDir := t.TempDir()
	agentDir := t.TempDir()

	toolset := latest.Toolset{
		Type:       "lsp",
		Command:    "gopls",
		WorkingDir: customDir,
	}

	registry := testToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: agentDir},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	rawTool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, rawTool)

	lspTool, ok := rawTool.(*lsp.ToolSet)
	require.True(t, ok, "expected *lsp.ToolSet")
	assert.Equal(t, customDir, lspTool.WorkingDir())
}

// TestCreateMCPTool_RefRemote_WorkingDir_ReturnsError verifies that when a
// ref-based MCP resolves to a remote server at runtime, setting working_dir
// returns a clear error rather than silently discarding the field.
func TestCreateMCPTool_RefRemote_WorkingDir_ReturnsError(t *testing.T) {
	// The "docker:remote-server" ref is seeded as type "remote" in TestMain.
	toolset := latest.Toolset{
		Type:       "mcp",
		Ref:        "docker:remote-server",
		WorkingDir: "./workspace",
	}

	registry := testToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	_, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "working_dir is not supported")
	assert.Contains(t, err.Error(), "remote server")
}

// TestCreateMCPTool_RefRemote_NoWorkingDir_Succeeds verifies that a ref-based
// MCP that resolves to a remote server still works fine when working_dir is
// not set (the common case — regression guard).
func TestCreateMCPTool_RefRemote_NoWorkingDir_Succeeds(t *testing.T) {
	// The "docker:remote-server" ref is seeded as type "remote" in TestMain.
	toolset := latest.Toolset{
		Type: "mcp",
		Ref:  "docker:remote-server",
	}

	registry := testToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
}
