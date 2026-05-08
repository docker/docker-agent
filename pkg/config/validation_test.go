package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_InvalidPath(t *testing.T) {
	tmp := t.TempDir()
	tmpRoot := openRoot(t, tmp)

	validConfig := `version: 1
agents:
  root:
    model: "openai/gpt-4"
`

	err := tmpRoot.WriteFile("valid.yaml", []byte(validConfig), 0o644)
	require.NoError(t, err)

	cfg, err := Load(t.Context(), NewFileSource(filepath.Join(tmp, "valid.yaml"), nil))
	require.NoError(t, err)
	require.NotNil(t, cfg)

	_, err = Load(t.Context(), NewFileSource(filepath.Join(tmp, "../../../etc/passwd"), nil)) //nolint: gocritic // testing invalid path
	require.Error(t, err)
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{
			name: "memory toolset missing path",
			path: "missing_memory_path_v2.yaml",
		},
		{
			name: "path in non memory toolset",
			path: "invalid_path_v2.yaml",
		},
		{
			name: "post_edit in non filesystem toolset",
			path: "invalid_post_edit_v2.yaml",
		},
		{
			name: "lsp toolset missing command",
			path: "invalid_lsp_missing_command.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(t.Context(), NewFileSource(filepath.Join("testdata", tt.path), nil))
			require.Error(t, err)
		})
	}
}

func TestLoadConfig_UnsupportedVersion(t *testing.T) {
	t.Parallel()

	cfg := `version: "99"
agents:
  root:
    model: openai/gpt-4
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfg), nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported config version: 99")
	assert.Contains(t, err.Error(), "valid versions")
	// Check that at least some known versions are listed
	assert.Contains(t, err.Error(), "1")
	assert.Contains(t, err.Error(), "2")
}

func TestValidSkillsConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{
			name: "skills with all filesystem tools",
			path: "skills_valid_all_tools.yaml",
		},
		{
			name: "skills with explicit read_file tool",
			path: "skills_valid_explicit_tools.yaml",
		},
		{
			name: "skills disabled",
			path: "skills_disabled.yaml",
		},
		{
			name: "skills with remote sources",
			path: "skills_with_remote.yaml",
		},
		{
			name: "skills enabled without filesystem toolset is fine",
			path: "skills_missing_filesystem.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(t.Context(), NewFileSource(filepath.Join("testdata", tt.path), nil))
			require.NoError(t, err)
			require.NotNil(t, cfg)
		})
	}
}

func TestInvalidSkillsSources(t *testing.T) {
	t.Parallel()

	cfgStr := `version: "5"
agents:
  root:
    model: openai/gpt-4o
    skills:
      - invalid_source
    toolsets:
      - type: filesystem
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr), nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown skills source")
}
