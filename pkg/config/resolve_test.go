package config

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/reference"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestOciRefToFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ociRef   string
		expected string
	}{
		{
			name:     "simple reference",
			ociRef:   "myagent",
			expected: "myagent.yaml",
		},
		{
			name:     "reference with registry and tag",
			ociRef:   "docker.io/myorg/agent:v1",
			expected: "docker.io_myorg_agent_v1.yaml",
		},
		{
			name:     "localhost with port",
			ociRef:   "localhost:5000/test",
			expected: "localhost_5000_test.yaml",
		},
		{
			name:     "reference with digest",
			ociRef:   "myregistry.io/org/app@sha256:abc123",
			expected: "myregistry.io_org_app_sha256_abc123.yaml",
		},
		{
			name:     "already has .yaml extension",
			ociRef:   "myagent.yaml",
			expected: "myagent.yaml",
		},
		{
			name:     "complex path",
			ociRef:   "registry.example.com:443/project/subproject/agent:latest",
			expected: "registry.example.com_443_project_subproject_agent_latest.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := reference.OciRefToFilename(tt.ociRef)

			assert.Equal(t, tt.expected, result)
		})
	}
}

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

	resolved, err := resolve("")

	require.NoError(t, err)
	assert.Equal(t, "default", resolved)
}

func TestResolveAgentFile_DefaultIsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

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

	// Set up alias with yolo option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("yolo-agent", &userconfig.Alias{
		Path: "agentcatalog/coder",
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

	// Set up alias with model option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("model-agent", &userconfig.Alias{
		Path:  "agentcatalog/coder",
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

	// Set up alias with both options
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("turbo", &userconfig.Alias{
		Path:  "agentcatalog/coder",
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

func TestResolveAlias_NoOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Set up alias without options
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("plain", &userconfig.Alias{
		Path: "agentcatalog/coder",
	}))
	require.NoError(t, cfg.Save())

	// Resolve alias options - should return nil since no options set
	alias := ResolveAlias("plain")
	assert.Nil(t, alias)
}

func TestResolveAlias_NotAnAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Resolve non-existent alias
	alias := ResolveAlias("./some-file.yaml")
	assert.Nil(t, alias)
}

func TestResolveAlias_EmptyUsesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Set up default alias with yolo option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("default", &userconfig.Alias{
		Path: "agentcatalog/coder",
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

	// Set up alias with hide_tool_results option
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("hidden-tools", &userconfig.Alias{
		Path:            "agentcatalog/coder",
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

	// Set up alias with all options
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("full", &userconfig.Alias{
		Path:            "agentcatalog/coder",
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

func TestIsExternalReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "OCI reference with namespace",
			input:    "agentcatalog/pirate",
			expected: true,
		},
		{
			name:     "OCI reference with registry",
			input:    "docker.io/myorg/myagent:v1",
			expected: true,
		},
		{
			name:     "HTTPS URL",
			input:    "https://example.com/agent.yaml",
			expected: true,
		},
		{
			name:     "HTTP URL",
			input:    "http://example.com/agent.yaml",
			expected: true,
		},
		{
			name:     "simple agent name is not external",
			input:    "my_agent",
			expected: false,
		},
		{
			name:     "agent name with hyphen is not external",
			input:    "my-local-agent",
			expected: false,
		},
		{
			name:     "empty string is not external",
			input:    "",
			expected: false,
		},
		{
			name:     "named OCI reference is external",
			input:    "reviewer:agentcatalog/review-pr",
			expected: true,
		},
		{
			name:     "named URL reference is external",
			input:    "myagent:https://example.com/agent.yaml",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := IsExternalReference(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseExternalAgentRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		expectedName string
		expectedRef  string
	}{
		{
			name:         "simple OCI reference derives base name",
			input:        "agentcatalog/pirate",
			expectedName: "pirate",
			expectedRef:  "agentcatalog/pirate",
		},
		{
			name:         "OCI reference with tag derives base name without tag",
			input:        "docker.io/myorg/myagent:v1",
			expectedName: "myagent",
			expectedRef:  "docker.io/myorg/myagent:v1",
		},
		{
			name:         "OCI reference with digest derives base name",
			input:        "docker.io/myorg/myagent@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			expectedName: "myagent",
			expectedRef:  "docker.io/myorg/myagent@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:         "explicit name prefix",
			input:        "reviewer:agentcatalog/review-pr",
			expectedName: "reviewer",
			expectedRef:  "agentcatalog/review-pr",
		},
		{
			name:         "explicit name with tagged OCI ref",
			input:        "myreviewer:docker.io/myorg/review-pr:v2",
			expectedName: "myreviewer",
			expectedRef:  "docker.io/myorg/review-pr:v2",
		},
		{
			name:         "URL reference derives filename",
			input:        "https://example.com/agent.yaml",
			expectedName: "agent",
			expectedRef:  "https://example.com/agent.yaml",
		},
		{
			name:         "named URL reference",
			input:        "myagent:https://example.com/agent.yaml",
			expectedName: "myagent",
			expectedRef:  "https://example.com/agent.yaml",
		},
		{
			name:         "simple name without slash is not split",
			input:        "my_agent",
			expectedName: "my_agent",
			expectedRef:  "my_agent",
		},
		{
			name:         "OCI ref with registry port is not confused with name prefix",
			input:        "localhost:5000/test/agent",
			expectedName: "agent",
			expectedRef:  "localhost:5000/test/agent",
		},
		{
			name:         "deeply nested OCI path",
			input:        "registry.example.com/org/sub/agent:latest",
			expectedName: "agent",
			expectedRef:  "registry.example.com/org/sub/agent:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, ref := ParseExternalAgentRef(tt.input)
			assert.Equal(t, tt.expectedName, name)
			assert.Equal(t, tt.expectedRef, ref)
		})
	}
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
