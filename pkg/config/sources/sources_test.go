package sources

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestResolveAgentFile_LocalFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	yamlFile := filepath.Join(tmpDir, "test-agent.yaml")
	yamlContent := `version: "1"
agents:
  root:
    model: openai/gpt-4o
    description: Test agent
    instruction: You are a test agent
`
	require.NoError(t, os.WriteFile(yamlFile, []byte(yamlContent), 0o644))

	resolved, err := resolve(yamlFile)
	require.NoError(t, err)

	absPath, err := filepath.Abs(yamlFile)
	require.NoError(t, err)
	assert.Equal(t, absPath, resolved)
}

func TestResolveAgentFile_EmptyIsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	resolved, err := resolve("")

	require.NoError(t, err)
	assert.Equal(t, "default", resolved)
}

func TestResolveAgentFile_DefaultIsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	resolved, err := resolve("default")

	require.NoError(t, err)
	assert.Equal(t, "default", resolved)
}

func TestResolveAgentFile_CoderIsCoder(t *testing.T) {
	t.Parallel()

	resolved, err := resolve("coder")

	require.NoError(t, err)
	assert.Equal(t, "coder", resolved)
}

func TestResolveAgentFile_ReplaceAliasWithActualFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Prepare an aliased file: alias -> [xxx]/pirate.yaml
	wd := t.TempDir()
	aliasedAgentFile := filepath.Join(wd, "pirate.yaml")
	require.NoError(t, os.WriteFile(aliasedAgentFile, []byte(`some config`), 0o644))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("other", &userconfig.Alias{Path: "another_file.yaml"}))
	require.NoError(t, cfg.SetAlias("alias", &userconfig.Alias{Path: aliasedAgentFile}))
	require.NoError(t, cfg.Save())

	resolved, err := resolve("alias")

	require.NoError(t, err)
	assert.Equal(t, aliasedAgentFile, resolved)
}

func TestResolveAgentFile_ReplaceDefaultAliasWithActualFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Prepare an aliased file: alias -> [xxx]/pirate.yaml
	wd := t.TempDir()
	aliasedAgentFile := filepath.Join(wd, "pirate.yaml")
	require.NoError(t, os.WriteFile(aliasedAgentFile, []byte(`some config`), 0o644))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("other", &userconfig.Alias{Path: "another_file.yaml"}))
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: aliasedAgentFile}))
	require.NoError(t, cfg.Save())

	resolved, err := resolve("default")

	require.NoError(t, err)
	assert.Equal(t, aliasedAgentFile, resolved)
}

func TestResolveAgentFile_ReplaceEmptyAliasWithActualFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Prepare an aliased file: alias -> [xxx]/pirate.yaml
	wd := t.TempDir()
	aliasedAgentFile := filepath.Join(wd, "pirate.yaml")
	require.NoError(t, os.WriteFile(aliasedAgentFile, []byte(`some config`), 0o644))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("other", &userconfig.Alias{Path: "another_file.yaml"}))
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: aliasedAgentFile}))
	require.NoError(t, cfg.Save())

	resolved, err := resolve("")

	require.NoError(t, err)
	assert.Equal(t, aliasedAgentFile, resolved)
}

func TestResolveAgentFile_Directory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Should resolve directory to absolute path
	resolved, err := resolve(tmpDir)
	require.NoError(t, err)

	absPath, err := filepath.Abs(tmpDir)
	require.NoError(t, err)
	assert.Equal(t, absPath, resolved)
}

func TestResolveAgentFile_DirectoryWithAgentYaml(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	agentContent := `version: "1"
agents:
  root:
    model: openai/gpt-4o
    description: Test agent in directory
    instruction: You are a test agent
`
	agentFile := filepath.Join(tmpDir, "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(agentContent), 0o644))

	// Resolving directory should return directory path
	resolved, err := resolve(tmpDir)
	require.NoError(t, err)

	absPath, err := filepath.Abs(tmpDir)
	require.NoError(t, err)
	assert.Equal(t, absPath, resolved)
}

func TestResolveAgentFile_NonExistentDirectory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	nonExistentDir := filepath.Join(tmpDir, "does-not-exist")

	// Should try to treat as OCI reference
	_, err := resolve(nonExistentDir)
	require.NoError(t, err)
}

func TestResolveAgentFile_DirectoryVsFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a directory
	subDir := filepath.Join(tmpDir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))

	// Create a file with same name but .yaml extension
	yamlFile := filepath.Join(tmpDir, "subdir.yaml")
	yamlContent := `version: "1"
agents:
  root:
    model: openai/gpt-4o
    description: Test agent file
    instruction: You are a test agent
`
	require.NoError(t, os.WriteFile(yamlFile, []byte(yamlContent), 0o644))

	// Resolve directory
	resolvedDir, err := resolve(subDir)
	require.NoError(t, err)
	absDirPath, err := filepath.Abs(subDir)
	require.NoError(t, err)
	assert.Equal(t, absDirPath, resolvedDir)

	// Resolve file
	resolvedFile, err := resolve(yamlFile)
	require.NoError(t, err)
	absFilePath, err := filepath.Abs(yamlFile)
	require.NoError(t, err)
	assert.Equal(t, absFilePath, resolvedFile)

	// They should be different paths
	assert.NotEqual(t, resolvedDir, resolvedFile)
}

func TestResolveAgentFile_EmptyDirectory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	emptyDir := filepath.Join(tmpDir, "empty")
	require.NoError(t, os.Mkdir(emptyDir, 0o755))

	// Should still resolve the directory path
	resolved, err := resolve(emptyDir)
	require.NoError(t, err)

	absPath, err := filepath.Abs(emptyDir)
	require.NoError(t, err)
	assert.Equal(t, absPath, resolved)
}

func TestResolveSources(t *testing.T) {
	t.Parallel()

	sources, err := ResolveSources("./testdata/v1.yaml", nil)
	require.NoError(t, err)

	assert.Len(t, sources, 1)
	require.Contains(t, sources, "v1")
}

func TestResolve_DefaultAliasOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Create an agent file
	agentFile := filepath.Join(t.TempDir(), "custom-agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    description: Custom agent
`), 0o644))

	// Set up alias for "default"
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: agentFile}))
	require.NoError(t, cfg.Save())

	// Resolve with "default" should return the aliased file
	source, err := Resolve("default", nil)
	require.NoError(t, err)
	assert.Equal(t, agentFile, source.Name())

	// Verify it reads the custom content
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(data), "Custom agent")
}

func TestResolve_CoderBuiltinAgent(t *testing.T) {
	t.Parallel()

	source, err := Resolve("coder", nil)
	require.NoError(t, err)
	assert.Equal(t, "coder", source.Name())

	// Verify it reads the embedded coder agent config
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(data), "Coding Agent")
}

func TestBuiltinAgentNames(t *testing.T) {
	t.Parallel()

	names := BuiltinAgentNames()

	assert.Contains(t, names, "default")
	assert.Contains(t, names, "coder")
}

func TestResolve_DefaultAliasToOCIReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias for "default" pointing to an OCI reference
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: "docker/gordon"}))
	require.NoError(t, cfg.Save())

	// Resolve with "default" should return an OCI source with the aliased reference
	source, err := Resolve("default", nil)
	require.NoError(t, err)
	assert.Equal(t, "docker/gordon", source.Name())
}

func TestResolveSources_DefaultAliasToOCIReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias for "default" pointing to an OCI reference
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: "docker/gordon"}))
	require.NoError(t, cfg.Save())

	// ResolveSources with "default" should return an OCI source with the aliased reference
	sources, err := ResolveSources("default", nil)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the OCI reference converted to filename
	source, ok := sources["docker_gordon.yaml"]
	require.True(t, ok, "expected source key 'docker_gordon.yaml', got keys: %v", sources)
	assert.Equal(t, "docker/gordon", source.Name())
}

func TestResolve_EmptyWithDefaultAliasOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Create an agent file
	agentFile := filepath.Join(t.TempDir(), "custom-agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    description: Custom agent via empty
`), 0o644))

	// Set up alias for "default"
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: agentFile}))
	require.NoError(t, cfg.Save())

	// Resolve with empty string should also use the "default" alias
	source, err := Resolve("", nil)
	require.NoError(t, err)
	assert.Equal(t, agentFile, source.Name())

	// Verify it reads the custom content
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(data), "Custom agent via empty")
}

func TestResolveSources_DefaultAliasOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Create an agent file
	agentFile := filepath.Join(t.TempDir(), "custom-agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    description: Custom agent for sources
`), 0o644))

	// Set up alias for "default"
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: agentFile}))
	require.NoError(t, cfg.Save())

	// ResolveSources with "default" should return the aliased file
	sources, err := ResolveSources("default", nil)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the filename without extension
	source, ok := sources["custom-agent"]
	require.True(t, ok, "expected source key 'custom-agent', got keys: %v", sources)

	// Verify it reads the custom content
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(data), "Custom agent for sources")
}

func TestResolveSources_EmptyWithDefaultAliasOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Create an agent file
	agentFile := filepath.Join(t.TempDir(), "custom-agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    description: Custom agent for sources via empty
`), 0o644))

	// Set up alias for "default"
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{Path: agentFile}))
	require.NoError(t, cfg.Save())

	// ResolveSources with empty string should also use the "default" alias
	sources, err := ResolveSources("", nil)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the filename without extension
	source, ok := sources["custom-agent"]
	require.True(t, ok, "expected source key 'custom-agent', got keys: %v", sources)

	// Verify it reads the custom content
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(data), "Custom agent for sources via empty")
}

func TestResolveAlias_WithYoloOption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias with yolo option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("yolo-agent", &userconfig.Alias{
		Path: "myorg/coder",
		Yolo: true,
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options
	alias := ResolveAlias("yolo-agent")
	require.NotNil(t, alias)
	assert.True(t, alias.Yolo)
	assert.Empty(t, alias.Model)
}

func TestResolveAlias_WithModelOption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias with model option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("model-agent", &userconfig.Alias{
		Path:  "myorg/coder",
		Model: "openai/gpt-4o-mini",
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options
	alias := ResolveAlias("model-agent")
	require.NotNil(t, alias)
	assert.False(t, alias.Yolo)
	assert.Equal(t, "openai/gpt-4o-mini", alias.Model)
}

func TestResolveAlias_WithBothOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias with both options
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("turbo", &userconfig.Alias{
		Path:  "myorg/coder",
		Yolo:  true,
		Model: "anthropic/claude-sonnet-4-0",
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options
	alias := ResolveAlias("turbo")
	require.NotNil(t, alias)
	assert.True(t, alias.Yolo)
	assert.Equal(t, "anthropic/claude-sonnet-4-0", alias.Model)
}

func TestResolveAlias_WithSandboxOption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("safe-coder", &userconfig.Alias{
		Path:    "myorg/coder",
		Sandbox: true,
	}))
	require.NoError(t, cfg.Save())

	alias := ResolveAlias("safe-coder")
	require.NotNil(t, alias)
	assert.True(t, alias.Sandbox)
	assert.False(t, alias.Yolo)
}

func TestResolveAlias_NoOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias without options
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("plain", &userconfig.Alias{
		Path: "myorg/coder",
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options - should return nil since no options set
	alias := ResolveAlias("plain")
	assert.Nil(t, alias)
}

func TestResolveAlias_NotAnAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Resolve non-existent alias
	alias := ResolveAlias("./some-file.yaml")
	assert.Nil(t, alias)
}

func TestResolveAlias_EmptyUsesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up default alias with yolo option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{
		Path: "myorg/coder",
		Yolo: true,
	}))
	require.NoError(t, cfg.Save())

	// Empty string should resolve to default alias
	alias := ResolveAlias("")
	require.NotNil(t, alias)
	assert.True(t, alias.Yolo)
}

func TestResolveAlias_WithHideToolResultsOption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias with hide_tool_results option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("hidden-tools", &userconfig.Alias{
		Path:            "myorg/coder",
		HideToolResults: true,
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options
	alias := ResolveAlias("hidden-tools")
	require.NotNil(t, alias)
	assert.True(t, alias.HideToolResults)
	assert.False(t, alias.Yolo)
	assert.Empty(t, alias.Model)
}

func TestResolveAlias_WithAllOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Set up alias with all options
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("full", &userconfig.Alias{
		Path:            "myorg/coder",
		Yolo:            true,
		Model:           "anthropic/claude-sonnet-4-0",
		HideToolResults: true,
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options
	alias := ResolveAlias("full")
	require.NotNil(t, alias)
	assert.True(t, alias.Yolo)
	assert.Equal(t, "anthropic/claude-sonnet-4-0", alias.Model)
	assert.True(t, alias.HideToolResults)
}

func TestResolveSources_URLEncodedKey(t *testing.T) {
	t.Parallel()

	testURL := "https://example.com/agent.yaml?agentTag=v1.0.0-dev&origin=cli"

	sources, err := ResolveSources(testURL, nil)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the URL-encoded version of the URL
	expectedKey := url.QueryEscape(testURL)
	source, ok := sources[expectedKey]
	require.True(t, ok, "expected source key '%s'", expectedKey)

	// The source Name() should still return the original URL for fetching
	assert.Equal(t, testURL, source.Name())
}

func TestResolve_URLReference(t *testing.T) {
	t.Parallel()

	source, err := Resolve("https://example.com/agent.yaml", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/agent.yaml", source.Name())
	assert.Empty(t, source.ParentDir())
}

func TestResolveSources_URLReference(t *testing.T) {
	t.Parallel()

	testURL := "https://example.com/agent.yaml"
	sources, err := ResolveSources(testURL, nil)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the URL-encoded version
	expectedKey := url.QueryEscape(testURL)
	source, ok := sources[expectedKey]
	require.True(t, ok)
	assert.Equal(t, testURL, source.Name())
}
