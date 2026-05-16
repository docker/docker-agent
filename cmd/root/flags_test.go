package root

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestGatewayLogic(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		args       []string
		userConfig *userconfig.Config
		expected   string
	}{
		{
			name:     "env",
			env:      "https://models.example.com",
			expected: "https://models.example.com",
		},
		{
			name:     "cli",
			args:     []string{"--models-gateway", "https://cli-models.example.com"},
			expected: "https://cli-models.example.com",
		},
		{
			name:     "cli_overrides_env",
			env:      "https://env-models.example.com",
			args:     []string{"--models-gateway", "https://cli-models.example.com"},
			expected: "https://cli-models.example.com",
		},
		{
			name:       "user_config",
			userConfig: &userconfig.Config{ModelsGateway: "https://userconfig-models.example.com"},
			expected:   "https://userconfig-models.example.com",
		},
		{
			name:       "env_overrides_user_config",
			env:        "https://env-models.example.com",
			userConfig: &userconfig.Config{ModelsGateway: "https://userconfig-models.example.com"},
			expected:   "https://env-models.example.com",
		},
		{
			name:       "cli_overrides_user_config",
			args:       []string{"--models-gateway", "https://cli-models.example.com"},
			userConfig: &userconfig.Config{ModelsGateway: "https://userconfig-models.example.com"},
			expected:   "https://cli-models.example.com",
		},
		{
			name:       "cli_overrides_env_and_user_config",
			env:        "https://env-models.example.com",
			args:       []string{"--models-gateway", "https://cli-models.example.com"},
			userConfig: &userconfig.Config{ModelsGateway: "https://userconfig-models.example.com"},
			expected:   "https://cli-models.example.com",
		},
		{
			name:       "user_config_with_trailing_slash",
			userConfig: &userconfig.Config{ModelsGateway: "https://userconfig-models.example.com/"},
			expected:   "https://userconfig-models.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CAGENT_MODELS_GATEWAY", tt.env) // Make sure the user doesn't have an old env set
			t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", tt.env)

			// Mock user config loader
			original := loadUserConfig
			loadUserConfig = func() (*userconfig.Config, error) {
				if tt.userConfig != nil {
					return tt.userConfig, nil
				}
				return &userconfig.Config{}, nil
			}
			t.Cleanup(func() { loadUserConfig = original })

			cmd := &cobra.Command{
				RunE: func(*cobra.Command, []string) error {
					return nil
				},
			}
			runConfig := config.RuntimeConfig{}
			addGatewayFlags(cmd, &runConfig)

			cmd.SetArgs(tt.args)
			err := cmd.Execute()

			require.NoError(t, err)
			assert.Equal(t, tt.expected, runConfig.ModelsGateway)
		})
	}
}

func TestGatewayFlags_CallsAncestorPersistentPreRunE(t *testing.T) {
	// Regression test: addGatewayFlags overrides PersistentPreRunE on the command
	// it is applied to. When that command is nested under an intermediate parent
	// (e.g. root → serve → api), the old code only checked cmd.Parent(), so a
	// grandparent's PersistentPreRunE was silently skipped.
	called := false

	root := &cobra.Command{Use: "root"}
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		called = true
		return nil
	}

	middle := &cobra.Command{Use: "middle"}

	leaf := &cobra.Command{
		Use:  "leaf",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	runConfig := config.RuntimeConfig{}
	addGatewayFlags(leaf, &runConfig)

	middle.AddCommand(leaf)
	root.AddCommand(middle)

	root.SetArgs([]string{"middle", "leaf"})
	require.NoError(t, root.Execute())
	assert.True(t, called, "root PersistentPreRunE should have been called through the intermediate parent")
}

func TestCanonize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "trailing_slash",
			input:    "https://example.com/",
			expected: "https://example.com",
		},
		{
			name:     "leading_and_trailing_whitespace",
			input:    " https://example.com ",
			expected: "https://example.com",
		},
		{
			name:     "trailing_slash_and_whitespace",
			input:    " https://example.com/ ",
			expected: "https://example.com",
		},
		{
			name:     "no_trailing_slash",
			input:    "https://example.com",
			expected: "https://example.com",
		},
		{
			name:     "path_with_trailing_slash",
			input:    "https://example.com/path/",
			expected: "https://example.com/path",
		},
		{
			name:     "empty_string",
			input:    "",
			expected: "",
		},
		{
			name:     "only_whitespace",
			input:    "   ",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := canonize(tt.input)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDefaultModelLogic(t *testing.T) {
	tests := []struct {
		name             string
		env              string
		userConfig       *userconfig.Config
		expectedProvider string
		expectedModel    string
	}{
		{
			name:             "env",
			env:              "openai/gpt-4o",
			expectedProvider: "openai",
			expectedModel:    "gpt-4o",
		},
		{
			name: "user_config",
			userConfig: &userconfig.Config{
				DefaultModel: &latest.FlexibleModelConfig{
					ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
				},
			},
			expectedProvider: "google",
			expectedModel:    "gemini-2.5-flash",
		},
		{
			name: "env_overrides_user_config",
			env:  "openai/gpt-4o",
			userConfig: &userconfig.Config{
				DefaultModel: &latest.FlexibleModelConfig{
					ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
				},
			},
			expectedProvider: "openai",
			expectedModel:    "gpt-4o",
		},
		{
			name:             "empty_when_not_set",
			expectedProvider: "",
			expectedModel:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DOCKER_AGENT_DEFAULT_MODEL", tt.env)

			// Mock user config loader
			original := loadUserConfig
			loadUserConfig = func() (*userconfig.Config, error) {
				if tt.userConfig != nil {
					return tt.userConfig, nil
				}
				return &userconfig.Config{}, nil
			}
			t.Cleanup(func() { loadUserConfig = original })

			cmd := &cobra.Command{
				RunE: func(*cobra.Command, []string) error {
					return nil
				},
			}
			runConfig := config.RuntimeConfig{}
			addGatewayFlags(cmd, &runConfig)

			cmd.SetArgs(nil)
			err := cmd.Execute()

			require.NoError(t, err)
			if tt.expectedProvider == "" && tt.expectedModel == "" {
				assert.Nil(t, runConfig.DefaultModel)
			} else {
				require.NotNil(t, runConfig.DefaultModel)
				assert.Equal(t, tt.expectedProvider, runConfig.DefaultModel.Provider)
				assert.Equal(t, tt.expectedModel, runConfig.DefaultModel.Model)
			}
		})
	}
}
