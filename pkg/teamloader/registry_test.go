package teamloader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/lsp"
	mcptool "github.com/docker/docker-agent/pkg/tools/mcp"
)

func TestCreateShellTool(t *testing.T) {
	toolset := latest.Toolset{
		Type: "shell",
	}

	registry := NewDefaultToolsetRegistry()

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

	registry := NewDefaultToolsetRegistry()

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

	registry := NewDefaultToolsetRegistry()

	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
	assert.Equal(t, "mcp(stdio cmd=some-nonexistent-mcp-binary)", tools.DescribeToolSet(tool))
}

func TestResolveToolsetWorkingDir(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		name              string
		toolsetWorkingDir string
		agentWorkingDir   string
		want              string
	}{
		{
			name:              "empty toolset dir returns agent dir",
			toolsetWorkingDir: "",
			agentWorkingDir:   "/workspace",
			want:              "/workspace",
		},
		{
			name:              "absolute toolset dir is returned as-is",
			toolsetWorkingDir: "/tmp/mcp",
			agentWorkingDir:   "/workspace",
			want:              "/tmp/mcp",
		},
		{
			name:              "relative toolset dir is joined with agent dir",
			toolsetWorkingDir: "./backend",
			agentWorkingDir:   "/workspace",
			want:              "/workspace/backend",
		},
		{
			name:              "bare relative dir joined with agent dir",
			toolsetWorkingDir: "tools/mcp",
			agentWorkingDir:   "/workspace",
			want:              "/workspace/tools/mcp",
		},
		{
			name:              "relative toolset dir with empty agent dir returns toolset dir unchanged",
			toolsetWorkingDir: "./backend",
			agentWorkingDir:   "",
			want:              "./backend",
		},
		{
			name:              "both empty returns empty",
			toolsetWorkingDir: "",
			agentWorkingDir:   "",
			want:              "",
		},
		// Tilde expansion tests (B2)
		{
			name:              "tilde expands to home dir",
			toolsetWorkingDir: "~/projects/app",
			agentWorkingDir:   "/workspace",
			want:              filepath.Join(home, "projects", "app"),
		},
		{
			name:              "bare tilde expands to home dir",
			toolsetWorkingDir: "~",
			agentWorkingDir:   "/workspace",
			want:              home,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveToolsetWorkingDir(tt.toolsetWorkingDir, tt.agentWorkingDir)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolveToolsetWorkingDir_EnvVarExpansion tests env-var expansion separately
// because t.Setenv is incompatible with t.Parallel on the parent test.
func TestResolveToolsetWorkingDir_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_REGISTRY_CWD_VAR", "/custom/path")

	got := resolveToolsetWorkingDir("${TEST_REGISTRY_CWD_VAR}/app", "/workspace")
	assert.Equal(t, "/custom/path/app", got)
}

// TestCreateMCPTool_WorkingDir_ReachesSubprocess verifies that working_dir is
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

	registry := NewDefaultToolsetRegistry()
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

	registry := NewDefaultToolsetRegistry()
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

	registry := NewDefaultToolsetRegistry()
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

	registry := NewDefaultToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: agentDir},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	rawTool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, rawTool)

	lspTool, ok := rawTool.(*lsp.Tool)
	require.True(t, ok, "expected *lsp.Tool")
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

	registry := NewDefaultToolsetRegistry()
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

	registry := NewDefaultToolsetRegistry()
	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
}
