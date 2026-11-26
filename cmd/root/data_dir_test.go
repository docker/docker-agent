package root

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/testutil"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestResolveDataDir_Precedence(t *testing.T) {
	t.Setenv(envDataDir, "")
	dir, err := resolveDataDir("")
	require.NoError(t, err)
	assert.Empty(t, dir)

	t.Setenv(envDataDir, "/from-env")
	dir, err = resolveDataDir("")
	require.NoError(t, err)
	assert.Equal(t, "/from-env", dir)
	dir, err = resolveDataDir("/from-flag")
	require.NoError(t, err)
	assert.Equal(t, "/from-flag", dir)
}

func TestDataDir_RootCommand(t *testing.T) {
	for _, tt := range []struct {
		name   string
		env    bool
		flag   bool
		preset bool
		nested bool
		tilde  bool
	}{
		{name: "default"},
		{name: "environment", env: true},
		{name: "flag", flag: true},
		{name: "flag overrides environment", env: true, flag: true},
		{name: "nested environment", env: true, nested: true},
		{name: "preserves programmatic override", preset: true},
		{name: "quoted tilde environment", env: true, tilde: true},
		{name: "quoted tilde flag", flag: true, tilde: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			testutil.SetHome(t, home)
			paths.ResetRootsForTesting()
			t.Setenv(envConfigDir, t.TempDir())
			t.Setenv(cagentEnvConfigDir, "")
			t.Setenv(envDataDir, "")
			logger := slog.Default()
			t.Cleanup(func() {
				slog.SetDefault(logger)
				paths.SetDataDir("")
				paths.SetConfigDir("")
			})

			_, expected, _ := testutil.DefaultDirs(runtime.GOOS, home)
			if tt.preset {
				expected = t.TempDir()
				paths.SetDataDir(expected)
			}

			root := NewRootCmd()
			root.SetContext(t.Context())
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)

			if tt.env {
				expected = t.TempDir()
				value := expected
				if tt.tilde {
					expected = filepath.Join(home, "data$literal")
					value = "~/data$literal"
				}
				t.Setenv(envDataDir, value)
			}

			probe := &cobra.Command{
				Use: "test-probe",
				RunE: func(*cobra.Command, []string) error {
					assert.Equal(t, expected, paths.GetDataDir())
					assert.Equal(t, filepath.Join(expected, "session.db"), sessionDBPath(""))
					h, err := history.New("")
					require.NoError(t, err)
					if tt.tilde {
						require.NoError(t, h.Load())
						assert.Equal(t, []string{"legacy prompt"}, h.Messages)
						assert.NoFileExists(t, filepath.Join(expected, "history.json"))
					}
					require.NoError(t, h.Add("test prompt"))
					assert.FileExists(t, filepath.Join(expected, "history"))
					return nil
				},
			}
			runConfig := config.RuntimeConfig{
				EnvProviderForTests: environment.NewMapEnvProvider(nil),
			}
			addGatewayFlags(probe, &runConfig, func() (*userconfig.Config, error) {
				return &userconfig.Config{}, nil
			})

			args := []string{"test-probe"}
			if tt.nested {
				parent := &cobra.Command{Use: "test-parent"}
				parent.AddCommand(probe)
				root.AddCommand(parent)
				args = append([]string{"test-parent"}, args...)
			} else {
				root.AddCommand(probe)
			}
			if tt.flag {
				expected = t.TempDir()
				value := expected
				if tt.tilde {
					expected = filepath.Join(home, "data$literal")
					value = "~/data$literal"
				}
				args = append(args, "--data-dir", value)
			}
			if tt.tilde {
				require.NoError(t, os.MkdirAll(expected, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(expected, "history.json"), []byte(`{"messages":["legacy prompt"]}`), 0o600))
			}
			root.SetArgs(args)
			require.NoError(t, root.Execute())
		})
	}
}

// A problem settling the state directories is reported, never fatal: even
// `version` must work with a read-only or unreadable home.
func TestRootCommand_StateDirProblemsAreNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permission checks as a non-root user")
	}

	run := func(t *testing.T, home string) string {
		t.Helper()
		testutil.SetHome(t, home)
		t.Setenv(envConfigDir, "")
		t.Setenv(cagentEnvConfigDir, "")
		t.Setenv(envDataDir, "")
		paths.SetRoot("")
		paths.ResetRootsForTesting()
		logger := slog.Default()
		t.Cleanup(func() {
			slog.SetDefault(logger)
			paths.ResetRootsForTesting()
		})

		var stderr strings.Builder
		root := NewRootCmd()
		root.SetContext(t.Context())
		root.SetOut(io.Discard)
		root.SetErr(&stderr)
		root.SetArgs([]string{"version"})
		require.NoError(t, root.Execute())
		return stderr.String()
	}

	t.Run("read-only home", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, os.Chmod(home, 0o500))
		t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

		assert.NotContains(t, run(t, home), "Warning:")
	})

	t.Run("unreadable legacy root", func(t *testing.T) {
		home := t.TempDir()
		legacy := filepath.Join(home, ".cagent")
		require.NoError(t, os.MkdirAll(filepath.Join(legacy, "sessions"), 0o700))
		require.NoError(t, os.Chmod(legacy, 0))
		t.Cleanup(func() { _ = os.Chmod(legacy, 0o700) })

		assert.Contains(t, run(t, home), "Warning: cannot read "+legacy)
		assert.Equal(t, legacy, paths.GetDataDir())
	})
}
