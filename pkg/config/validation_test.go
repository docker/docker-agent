package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestLoadConfig_InvalidPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	tmpRoot := openRoot(t, tmp)

	validConfig := `version: 1
agents:
  root:
    model: "openai/gpt-4"
`

	err := tmpRoot.WriteFile("valid.yaml", []byte(validConfig), 0o644)
	require.NoError(t, err)

	cfg, err := Load(t.Context(), NewFileSource(filepath.Join(tmp, "valid.yaml")))
	require.NoError(t, err)
	require.NotNil(t, cfg)

	_, err = Load(t.Context(), NewFileSource(filepath.Join(tmp, "../../../etc/passwd"))) //nolint: gocritic // testing invalid path
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

			_, err := Load(t.Context(), NewFileSource(filepath.Join("testdata", tt.path)))
			require.Error(t, err)
		})
	}
}

func TestCompactionThresholdValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		threshold float64
		wantErr   bool
	}{
		{name: "valid low", threshold: 0.1},
		{name: "valid default", threshold: 0.9},
		{name: "valid max", threshold: 1},
		{name: "zero rejected", threshold: 0, wantErr: true},
		{name: "negative rejected", threshold: -0.5, wantErr: true},
		{name: "above one rejected", threshold: 1.5, wantErr: true},
	}

	for _, tt := range tests {
		t.Run("model "+tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := latest.Config{Models: map[string]latest.ModelConfig{
				"primary": {Provider: "openai", Model: "gpt-4o", CompactionThreshold: new(tt.threshold)},
			}}
			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "models.primary: compaction_threshold must be greater than 0 and at most 1")
			} else {
				assert.NoError(t, err)
			}
		})

		t.Run("agent "+tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := latest.Config{Agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4o", CompactionThreshold: new(tt.threshold)},
			}}
			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "agents.root: compaction_threshold must be greater than 0 and at most 1")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidateAddPromptFilesDepth keeps the Go validation in step with the
// JSON schema's "minimum": 0 — YAML loading only goes through the former.
func TestValidateAddPromptFilesDepth(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		depth   int
		wantErr bool
	}{
		{name: "disabled", depth: 0},
		{name: "positive", depth: 3},
		{name: "negative rejected", depth: -1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := latest.Config{Agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4o", AddPromptFilesDepth: tt.depth},
			}}
			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "agents.root: add_prompt_files_depth must be >= 0, got -1")
			} else {
				assert.NoError(t, err)
			}
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
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfg)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported config version: 99")
	assert.Contains(t, err.Error(), "valid versions")
	// Check that at least some known versions are listed
	assert.Contains(t, err.Error(), "1")
	assert.Contains(t, err.Error(), "2")
}

func TestLoadConfig_RequiresAtLeastOneAgent(t *testing.T) {
	t.Parallel()

	cfg := `version: "12"
models:
  fast:
    provider: openai
    model: gpt-4o-mini
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfg)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one agent must be configured")
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

			cfg, err := Load(t.Context(), NewFileSource(filepath.Join("testdata", tt.path)))
			require.NoError(t, err)
			require.NotNil(t, cfg)
		})
	}
}

func TestSkillsConfigRejectsEmptyEntry(t *testing.T) {
	t.Parallel()

	// Empty entries in the skills list should be rejected.
	cfgStr := `version: "5"
agents:
  root:
    model: openai/gpt-4o
    skills:
      - local
      - ""
    toolsets:
      - type: filesystem
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty skills entry")
}

func TestInlineSkillsValid(t *testing.T) {
	t.Parallel()

	cfgStr := `version: "` + latest.Version + `"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - local
      - name: changelog
        description: Write a changelog entry.
        instructions: Do the thing.
      - name: triage
        description: Triage a bug.
        context: fork
        instructions: Triage it.
`
	cfg, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
	require.NoError(t, err)
	agent, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, agent.Skills.Inline, 2)
	assert.Equal(t, "changelog", agent.Skills.Inline[0].Name)
	assert.Equal(t, "fork", agent.Skills.Inline[1].Context)
}

func TestInlineSkillsValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		skills  string
		wantErr string
	}{
		{
			name:    "missing description",
			skills:  "      - name: foo\n        instructions: do it\n",
			wantErr: "missing a description",
		},
		{
			name:    "missing instructions",
			skills:  "      - name: foo\n        description: a foo\n",
			wantErr: "missing instructions",
		},
		{
			name:    "invalid context",
			skills:  "      - name: foo\n        description: a foo\n        instructions: do it\n        context: nope\n",
			wantErr: "invalid context",
		},
		{
			name:    "duplicate inline skill",
			skills:  "      - name: foo\n        description: a foo\n        instructions: do it\n      - name: foo\n        description: another foo\n        instructions: do it again\n",
			wantErr: "duplicate inline skill",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfgStr := "version: \"" + latest.Version + "\"\nagents:\n  root:\n    model: openai/gpt-4o\n    instruction: test\n    skills:\n" + tt.skills
			_, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestForkSkillToolsets(t *testing.T) {
	t.Parallel()

	t.Run("valid fork skill with toolset reference", func(t *testing.T) {
		t.Parallel()
		cfgStr := `version: "` + latest.Version + `"
toolsets:
  web:
    type: fetch
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: research
        description: Research a topic.
        context: fork
        toolsets:
          - web
        instructions: Do research.
`
		cfg, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
		require.NoError(t, err)
		agent, ok := cfg.Agents.Lookup("root")
		require.True(t, ok)
		require.Len(t, agent.Skills.Inline, 1)
		assert.Equal(t, []string{"web"}, agent.Skills.Inline[0].Toolsets)
	})

	t.Run("valid fork skill with allowed_tools", func(t *testing.T) {
		t.Parallel()
		cfgStr := `version: "` + latest.Version + `"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: audit
        description: Audit the repo.
        context: fork
        allowed_tools:
          - read_file
        instructions: Inspect only.
`
		cfg, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
		require.NoError(t, err)
		agent, ok := cfg.Agents.Lookup("root")
		require.True(t, ok)
		require.Len(t, agent.Skills.Inline, 1)
		assert.Equal(t, []string{"read_file"}, agent.Skills.Inline[0].AllowedTools)
	})

	t.Run("toolsets on non-fork skill is rejected", func(t *testing.T) {
		t.Parallel()
		cfgStr := `version: "` + latest.Version + `"
toolsets:
  web:
    type: fetch
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: research
        description: Research a topic.
        toolsets:
          - web
        instructions: Do research.
`
		_, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a fork skill")
	})

	t.Run("allowed_tools on non-fork skill is rejected", func(t *testing.T) {
		t.Parallel()
		cfgStr := `version: "` + latest.Version + `"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: audit
        description: Audit the repo.
        allowed_tools:
          - read_file
        instructions: Inspect only.
`
		_, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a fork skill")
	})

	t.Run("unknown toolset reference is rejected", func(t *testing.T) {
		t.Parallel()
		cfgStr := `version: "` + latest.Version + `"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: research
        description: Research a topic.
        context: fork
        toolsets:
          - nonexistent
        instructions: Do research.
`
		_, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-existent toolset")
	})
}

func TestSkillsNameFilter(t *testing.T) {
	t.Parallel()

	// A string that is not "local" and not a URL is interpreted as a skill
	// name to include. This must load successfully — the filter simply keeps
	// only matching skills at runtime.
	cfgStr := `version: "7"
agents:
  root:
    model: openai/gpt-4o
    skills:
      - git
      - docker
    toolsets:
      - type: filesystem
`
	cfg, err := Load(t.Context(), NewBytesSource("test", []byte(cfgStr)))
	require.NoError(t, err)
	agent, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.True(t, agent.Skills.Enabled())
	require.True(t, agent.Skills.HasLocal())
	assert.Equal(t, []string{"git", "docker"}, agent.Skills.Include)
}
