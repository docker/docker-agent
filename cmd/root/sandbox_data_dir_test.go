package root

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

func TestSandboxStateDirs_DataDirEnvironment(t *testing.T) {
	wd, err := sandbox.CanonicalPath(t.TempDir())
	require.NoError(t, err)
	outside, err := sandbox.CanonicalPath(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", wd)
	t.Setenv("USERPROFILE", wd)

	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		want    []string
		wantErr string
	}{
		{name: "unset"},
		{
			name: "inside workspace", env: filepath.Join(wd, "state"),
			want: []string{"--data-dir", filepath.Join(wd, "state")},
		},
		{
			name: "workspace root", env: wd,
			want: []string{"--data-dir", wd},
		},
		{
			name: "outside workspace", env: outside,
			wantErr: "inside the sandbox's writable workspace",
		},
		{
			name: "flag overrides outside environment", env: outside,
			args: []string{"--data-dir", filepath.Join(wd, "flag-state")},
			want: []string{"--data-dir", filepath.Join(wd, "flag-state")},
		},
		{
			name: "outside flag overrides inside environment", env: wd,
			args: []string{"--data-dir", outside}, wantErr: "inside the sandbox's writable workspace",
		},
		{
			name: "empty flag still rejected", env: wd,
			args: []string{"--data-dir="}, wantErr: "must not be empty",
		},
		{name: "blank environment rejected", env: " ", wantErr: "must not be empty"},
		{
			name: "quoted tilde environment", env: "~/state$literal",
			want: []string{"--data-dir", filepath.Join(wd, "state$literal")},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envDataDir, tt.env)
			cmd := &cobra.Command{Use: "run"}
			cmd.Flags().String("data-dir", "", "")
			cmd.Flags().String("cache-dir", "", "")
			require.NoError(t, cmd.ParseFlags(tt.args))

			got, err := sandboxStateDirs(cmd, wd)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			guestArgs := dockerAgentArgs(cmd, []string{"default"}, "", nil, got...)
			if len(tt.want) > 0 {
				assert.Contains(t, guestArgs, tt.want[1])
			} else {
				assert.NotContains(t, guestArgs, "--data-dir")
			}
		})
	}

	t.Run("escaping symlink rejected", func(t *testing.T) {
		escape := filepath.Join(wd, "escape")
		if err := os.Symlink(outside, escape); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Setenv(envDataDir, filepath.Join(escape, "state"))
		cmd := &cobra.Command{Use: "run"}
		cmd.Flags().String("data-dir", "", "")
		got, err := sandboxStateDirs(cmd, wd)
		require.ErrorContains(t, err, "inside the sandbox's writable workspace")
		assert.Nil(t, got)
	})
}

func TestSandboxStateDirs_ImplicitRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX backend stub")
	}
	wd, err := sandbox.CanonicalPath(t.TempDir())
	require.NoError(t, err)
	t.Chdir(wd)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SANDBOX_VM_ID", "")
	t.Setenv(envModelsGateway, "")
	t.Setenv(cagentEnvModelsGateway, "")
	t.Setenv("DOCKER_AGENT_HIDE_TELEMETRY_BANNER", "1")
	configDir := t.TempDir()
	t.Setenv(envConfigDir, configDir)
	t.Setenv(cagentEnvConfigDir, "")
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("settings: {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(wd, "docker-agent.yaml"), []byte(`runtime:
  sandbox: true
agents:
  root:
    model: openai/gpt-4o
    description: test
`), 0o600))

	fakeDir := t.TempDir()
	calls := filepath.Join(fakeDir, "calls")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "sandbox version") ;;
  "sandbox ls") printf '{"sandboxes":[]}\n' ;;
  "sandbox create"|"sandbox policy"|"sandbox exec") ;;
  *) exit 99 ;;
esac
`, calls)
	writeMockCLI(t, filepath.Join(fakeDir, "docker"), script)
	t.Setenv("PATH", fakeDir)
	logger := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		paths.SetDataDir("")
		paths.SetConfigDir("")
	})

	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		wantErr string
	}{
		{name: "environment forwarded", env: filepath.Join(wd, "state")},
		{name: "outside environment rejected", env: t.TempDir(), wantErr: "inside the sandbox's writable workspace"},
		{name: "empty flag rejected", env: filepath.Join(wd, "state"), args: []string{"--data-dir="}, wantErr: "must not be empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envDataDir, tt.env)
			require.NoError(t, os.WriteFile(calls, nil, 0o600))
			root := NewRootCmd()
			visitAll(root, func(cmd *cobra.Command) { cmd.SetContext(t.Context()) })
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(tt.args)
			err := root.Execute()
			data, readErr := os.ReadFile(calls)
			require.NoError(t, readErr)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.NotContains(t, string(data), "sandbox exec")
				return
			}
			require.NoError(t, err)
			assert.Contains(t, string(data), "sandbox exec")
			assert.Contains(t, string(data), "--data-dir "+tt.env)
		})
	}
}
