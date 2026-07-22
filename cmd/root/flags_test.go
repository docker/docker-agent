package root

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
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
			t.Parallel()

			// Inject env via a map provider rather than t.Setenv so the
			// test stays parallel-safe.
			loadUserConfig := func() (*userconfig.Config, error) {
				if tt.userConfig != nil {
					return tt.userConfig, nil
				}
				return &userconfig.Config{}, nil
			}

			cmd := &cobra.Command{
				RunE: func(*cobra.Command, []string) error {
					return nil
				},
			}
			env := map[string]string{}
			if tt.env != "" {
				env[envModelsGateway] = tt.env
			}
			runConfig := config.RuntimeConfig{
				EnvProviderForTests: environment.NewMapEnvProvider(env),
			}
			addGatewayFlags(cmd, &runConfig, loadUserConfig)

			cmd.SetArgs(tt.args)
			err := cmd.Execute()

			require.NoError(t, err)
			assert.Equal(t, tt.expected, runConfig.ModelsGateway)
		})
	}
}

func TestGatewayFlags_CallsAncestorPersistentPreRunE(t *testing.T) {
	t.Parallel()

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
	addGatewayFlags(leaf, &runConfig, userconfig.Load)

	middle.AddCommand(leaf)
	root.AddCommand(middle)

	root.SetArgs([]string{"middle", "leaf"})
	require.NoError(t, root.Execute())
	assert.True(t, called, "root PersistentPreRunE should have been called through the intermediate parent")
}

func TestGatewayFlags_RunsParentBeforeMaterialisingEnvProvider(t *testing.T) {
	t.Parallel()

	// Regression test: the persistent pre-run for the gateway flags
	// used to call runConfig.EnvProvider() before invoking the parent
	// PersistentPreRunE that overrides --config-dir / --cache-dir /
	// --data-dir. Because EnvProvider() caches its result, the parent
	// override never landed in the cached chain — in particular the
	// in-sandbox SandboxTokenProvider was constructed with the wrong
	// path (~/.config/cagent inside the sandbox image instead of the
	// host config dir bind-mounted at the same location), causing the
	// inner agent to fail with "sorry, you first need to sign in
	// Docker Desktop to use the Docker AI Gateway" even when the
	// host-side token writer was working.
	parentRanFirst := false
	envProviderConsulted := false

	root := &cobra.Command{Use: "root"}
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		assert.False(t, envProviderConsulted,
			"parent PersistentPreRunE must run before the gateway pre-run materialises the env provider")
		parentRanFirst = true
		return nil
	}

	leaf := &cobra.Command{
		Use:  "leaf",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	runConfig := config.RuntimeConfig{
		EnvProviderForTests: &recordingProvider{onGet: func() { envProviderConsulted = true }},
	}
	addGatewayFlags(leaf, &runConfig, userconfig.Load)
	root.AddCommand(leaf)

	root.SetArgs([]string{"leaf"})
	require.NoError(t, root.Execute())
	assert.True(t, parentRanFirst, "parent PersistentPreRunE never ran")
}

// recordingProvider invokes onGet on every Get call so tests can
// observe when the env provider chain is first consulted.
type recordingProvider struct {
	onGet func()
}

func (p *recordingProvider) Get(_ context.Context, _ string) (string, bool) {
	p.onGet()
	return "", false
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
			t.Parallel()

			loadUserConfig := func() (*userconfig.Config, error) {
				if tt.userConfig != nil {
					return tt.userConfig, nil
				}
				return &userconfig.Config{}, nil
			}

			cmd := &cobra.Command{
				RunE: func(*cobra.Command, []string) error {
					return nil
				},
			}
			env := map[string]string{}
			if tt.env != "" {
				env[envDefaultModel] = tt.env
			}
			runConfig := config.RuntimeConfig{
				EnvProviderForTests: environment.NewMapEnvProvider(env),
			}
			addGatewayFlags(cmd, &runConfig, loadUserConfig)

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

// User-level custom providers from the user config must seed the runtime
// config so custom `--model myprovider/mymodel` references resolve in every
// command (issue #3557).
func TestGatewayFlags_SeedsUserConfigProviders(t *testing.T) {
	t.Parallel()

	userCfg := &userconfig.Config{
		Providers: map[string]latest.ProviderConfig{
			"myprovider": {BaseURL: "https://llm.corp.example.com/v1", TokenKey: "MY_KEY"},
		},
	}

	cmd := &cobra.Command{
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	runConfig := config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(nil),
	}
	addGatewayFlags(cmd, &runConfig, func() (*userconfig.Config, error) { return userCfg, nil })

	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	require.Contains(t, runConfig.Providers, "myprovider")
	assert.Equal(t, "https://llm.corp.example.com/v1", runConfig.Providers["myprovider"].BaseURL)
}

// A missing or malformed --env-from-file must abort the run instead of being
// logged and skipped: the flag is the documented way to supply credentials
// (issue #3442).
func TestEnvFromFileErrorsAbortPreRun(t *testing.T) {
	t.Parallel()

	loadUserConfig := func() (*userconfig.Config, error) {
		return &userconfig.Config{}, nil
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		cmd := &cobra.Command{
			SilenceErrors: true,
			SilenceUsage:  true,
			RunE:          func(*cobra.Command, []string) error { return nil },
		}
		runConfig := config.RuntimeConfig{
			Config: config.Config{EnvFiles: []string{filepath.Join(t.TempDir(), "missing.env")}},
		}
		addGatewayFlags(cmd, &runConfig, loadUserConfig)

		cmd.SetArgs(nil)
		err := cmd.Execute()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "--env-from-file")
		assert.Contains(t, err.Error(), "missing.env")
	})

	t.Run("malformed file", func(t *testing.T) {
		t.Parallel()

		bad := filepath.Join(t.TempDir(), "bad.env")
		require.NoError(t, os.WriteFile(bad, []byte("NOT_A_PAIR\n"), 0o600))

		cmd := &cobra.Command{
			SilenceErrors: true,
			SilenceUsage:  true,
			RunE:          func(*cobra.Command, []string) error { return nil },
		}
		runConfig := config.RuntimeConfig{
			Config: config.Config{EnvFiles: []string{bad}},
		}
		addGatewayFlags(cmd, &runConfig, loadUserConfig)

		cmd.SetArgs(nil)
		err := cmd.Execute()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "--env-from-file")
		assert.Contains(t, err.Error(), "bad.env")
	})
}

// TestSetupWorkingDirectory: whitespace-only --working-dir is a
// misconfiguration and must fail loudly, not silently disable containment.
func TestSetupWorkingDirectory(t *testing.T) {
	t.Parallel()

	t.Run("empty is a no-op", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, setupWorkingDirectory(""))
	})

	t.Run("whitespace-only is rejected", func(t *testing.T) {
		t.Parallel()
		err := setupWorkingDirectory("   ")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--working-dir")

		err = setupWorkingDirectory("\t\n")
		require.Error(t, err)
	})
}
