package root

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestCloudAgentArgs(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	for _, ref := range []string{"default", "coder", "docker/agent:latest", "https://example.com/agent.yaml"} {
		cmd := newRunCmd()
		require.NoError(t, cmd.ParseFlags([]string{"--cloud", "--exec", "--flavor", "a", "--flavor", "b", "--model", "openai/gpt-5.6"}))
		got, err := cloudAgentArgs(cmd, []string{ref, "--sandbox", "hello"})
		require.NoError(t, err)
		assert.Equal(t, []string{"--exec=true", "--flavor", "a", "--flavor", "b", "--model", "openai/gpt-5.6", "--yolo", "--sandbox=false", "--", ref, "--sandbox", "hello"}, got)
		assert.NotContains(t, got, "--cloud")
		assert.NotContains(t, got, "--config-dir")
	}
	_, err := cloudAgentArgs(newRunCmd(), []string{"./agent.yaml"})
	require.ErrorContains(t, err, "cannot read local agent")

	for _, flag := range []string{"--attach", "--prompt-file", "--working-dir", "--env-from-file", "--models-gateway"} {
		cmd := newRunCmd()
		require.NoError(t, cmd.ParseFlags([]string{flag, "host-path"}))
		_, err := cloudAgentArgs(cmd, []string{"default"})
		require.ErrorContains(t, err, flag+" is not supported")
	}
}

func TestCloudAliasSafety(t *testing.T) {
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("aliases:\n  safe:\n    path: docker/agent:latest\n    safety: strict\n"), 0o600))
	got, err := cloudAgentArgs(newRunCmd(), []string{"safe", "--yolo"})
	require.NoError(t, err)
	assert.Equal(t, []string{"--safety", "strict", "--sandbox=false", "--", "docker/agent:latest", "--yolo"}, got)
}

func TestCloudLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX argv capture")
	}
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	t.Setenv("OPENAI_API_KEY", "host-secret-must-not-be-forwarded")
	fakeDir := t.TempDir()
	log := filepath.Join(fakeDir, "calls")
	t.Setenv("PATH", fakeDir)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
[ "$1" = --cloud ] || exit 99
case "$2" in
  run) printf 'sbx_test123\n' ;;
  exec) printf 'agent-output\n'; exit "${TEST_EXEC_EXIT:-0}" ;;
  stop) exit "${TEST_STOP_EXIT:-0}" ;;
  *) exit 99 ;;
esac
`, log)
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "sbx"), []byte(script), 0o700))
	for _, tc := range []struct {
		name               string
		execExit, stopExit string
		wantExit           int
		wantError          bool
	}{
		{"success", "0", "0", 0, false},
		{"exec failure still stops", "7", "0", 7, true},
		{"stop failure", "0", "9", 0, true},
		{"preserve agent failure", "7", "9", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_EXEC_EXIT", tc.execExit)
			t.Setenv("TEST_STOP_EXIT", tc.stopExit)
			require.NoError(t, os.WriteFile(log, nil, 0o600))
			cmd := newRunCmd()
			require.NoError(t, cmd.ParseFlags([]string{"--model", "openai/gpt-5.6"}))
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := runInCloudSandbox(t.Context(), cmd, []string{"default"}, sandbox.NewBackend(true, true), sandbox.Options{Cloud: true, TTL: time.Hour})
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.wantExit != 0 {
				var status cli.StatusError
				require.ErrorAs(t, err, &status)
				assert.Equal(t, tc.wantExit, status.StatusCode)
			}
			assert.Equal(t, "agent-output\n", stdout.String())
			assert.Contains(t, stderr.String(), "sbx_test123")
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			require.Len(t, lines, 3)
			assert.Regexp(t, `^--cloud run --detached --new --name docker-agent-[a-f0-9]{32} --ttl 1h0m0s --on-timeout stop docker-agent$`, lines[0])
			assert.Contains(t, lines[1], "--cloud exec -i ")
			assert.NotContains(t, lines[1], " -t ")
			assert.NotContains(t, lines[1], " -w ")
			assert.NotContains(t, lines[1], "OPENAI_API_KEY")
			assert.NotContains(t, lines[1], "config-dir")
			assert.Equal(t, "--cloud stop sbx_test123", lines[2])
		})
	}
}

func TestSandboxOptionValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flags   []string
		enabled bool
		opts    sandbox.Options
		want    string
	}{
		{[]string{"--kit", "docker/kit"}, false, sandbox.Options{}, "requires --sandbox"},
		{[]string{"--sandbox-ttl", "1h"}, true, sandbox.Options{}, "requires --cloud"},
		{nil, true, sandbox.Options{Cloud: true, TTL: -1}, "must be greater"},
		{nil, true, sandbox.Options{Cloud: true, TTL: 25 * time.Hour}, "at most 24h"},
		{nil, true, sandbox.Options{KitArgs: []string{"foo=bar"}}, "requires --sandbox-kit or --kit"},
	} {
		cmd := newRunCmd()
		require.NoError(t, cmd.ParseFlags(tc.flags))
		require.ErrorContains(t, validateSandboxOptions(cmd, tc.enabled, tc.opts), tc.want)
	}
}

func TestSandboxOuterFlagsAreNotForwarded(t *testing.T) {
	t.Parallel()
	cmd := newRunCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--sandbox", "--cloud", "--sandbox-kit", "./workload", "--kit", "./mixin", "--kit-arg", "list=a,b", "--sandbox-ttl", "2h"}))
	got := dockerAgentArgs(cmd, []string{"default"}, "", nil)
	assert.Equal(t, []string{"--yolo", "--sandbox=false", "--", "default"}, got)
}

func TestCloudAliasSafetyAndFlagValues(t *testing.T) {
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("aliases:\n  safe:\n    path: docker/agent:latest\n    safety: strict\n"), 0o600))
	for _, value := range []string{"--", "--yolo"} {
		cmd := newRunCmd()
		require.NoError(t, cmd.ParseFlags([]string{"--yolo=false", "--app-name=" + value}))
		got, err := cloudAgentArgs(cmd, []string{"safe"})
		require.NoError(t, err)
		assert.Equal(t, []string{"--app-name", value, "--yolo=false", "--safety", "strict", "--sandbox=false", "--", "docker/agent:latest"}, got)
	}
}

func TestSandboxSliceFlagRoundTrip(t *testing.T) {
	t.Parallel()
	cmd := newRunCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--env-from-file", `"/tmp/a,b.env"`, "--flavor", "a,b", "--flavor", "c"}))
	argv := dockerAgentArgs(cmd, []string{"default"}, "", nil)
	inner := newRunCmd()
	require.NoError(t, inner.ParseFlags(argv))
	files, err := inner.Flags().GetStringSlice("env-from-file")
	require.NoError(t, err)
	assert.Equal(t, []string{"/tmp/a,b.env"}, files)
	flavors, err := inner.Flags().GetStringArray("flavor")
	require.NoError(t, err)
	assert.Equal(t, []string{"a,b", "c"}, flavors)
}

func TestSandboxAliasSafetyDoesNotOverrideResumedSession(t *testing.T) {
	t.Parallel()
	cmd := newRunCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--session", "existing", "--yolo=false"}))
	got := dockerAgentArgs(cmd, []string{"default"}, "", &userconfig.Alias{Safety: "autonomous"})
	assert.NotContains(t, got, "--safety")
	assert.NotContains(t, got, "--yolo")
	assert.Contains(t, got, "--yolo=false")
}

func TestCloudRejectsInvalidUserConfigBeforeProvisioning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX backend stub")
	}
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`aliases:
  coder:
    path: docker/reviewed-agent:latest
    safety: strict
  broken:
    path: default
    safety: typo
`), 0o600))
	fakeDir := t.TempDir()
	calls := filepath.Join(fakeDir, "calls")
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "sbx"), fmt.Appendf(nil, "#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n", calls), 0o700))
	t.Setenv("PATH", fakeDir)
	t.Setenv("SANDBOX_VM_ID", "")
	cmd := newRunCmd()
	cmd.SetArgs([]string{"--cloud", "--exec", "coder"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "loading user config")
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "run")
	assert.NotContains(t, string(data), "exec")
}

func TestCloudBuiltinsRequireExplicitModel(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	// Provider sentinels from sbx are not evidence of cloud credentials.
	t.Setenv("OPENAI_API_KEY", "proxy-managed")
	t.Setenv("ANTHROPIC_API_KEY", "proxy-managed")
	_, err := cloudAgentArgs(newRunCmd(), []string{"default"})
	require.ErrorContains(t, err, "require --model")
	cmd := newRunCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--model", "openai/gpt-5.6"}))
	args, err := cloudAgentArgs(cmd, []string{"default"})
	require.NoError(t, err)
	assert.Contains(t, args, "openai/gpt-5.6")
	assert.NotContains(t, args, "anthropic")
}

func TestSandboxRejectsEmptyLaunchSources(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"sandbox-kit", "template", "kit"} {
		cmd := newRunCmd()
		require.NoError(t, cmd.ParseFlags([]string{"--" + flag + "="}))
		opts := sandbox.Options{}
		if flag == "kit" {
			opts.Kits = []string{""}
		}
		require.ErrorContains(t, validateSandboxOptions(cmd, true, opts), "--"+flag+" must not be empty")
	}
}

func TestSandboxStateDirOverrides(t *testing.T) {
	t.Parallel()
	wd, err := sandbox.CanonicalPath(t.TempDir())
	require.NoError(t, err)
	cmd := newRunCmd()
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().String("cache-dir", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"--data-dir", filepath.Join(wd, "state"), "--cache-dir", filepath.Join(wd, "cache"), "--session", "-1"}))
	extra, err := sandboxStateDirs(cmd, wd)
	require.NoError(t, err)
	got := dockerAgentArgs(cmd, []string{"default"}, "", nil, extra...)
	assert.Contains(t, got, filepath.Join(wd, "state"))
	assert.Contains(t, got, filepath.Join(wd, "cache"))
	assert.Contains(t, got, "--session")
	require.NoError(t, cmd.Flags().Set("data-dir", t.TempDir()))
	_, err = sandboxStateDirs(cmd, wd)
	require.ErrorContains(t, err, "inside the sandbox's writable workspace")
}

func TestSandboxCommandCancellationStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var status cli.StatusError
	require.ErrorAs(t, sandboxCommandError(ctx, errors.New("killed")), &status)
	assert.Equal(t, 130, status.StatusCode)
}

func TestSandboxResolvedSelectionSurvivesAliasChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX backend stub")
	}
	configDir := t.TempDir()
	paths.SetConfigDir(configDir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	t.Setenv("SANDBOX_VM_ID", "")
	workspace, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	firstPath, secondPath := filepath.Join(first, "agent.yaml"), filepath.Join(second, "agent.yaml")
	body := func(key, host string) []byte {
		return fmt.Appendf(nil, `providers:
  custom:
    provider: typesafe
    token_key: %s
models:
  root:
    provider: custom
    model: test
runtime:
  network_allowlist: [%s]
agents:
  root:
    model: root
`, key, host)
	}
	require.NoError(t, os.WriteFile(firstPath, body("SNAPSHOT_FIRST_KEY", "first.example.com"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, body("SNAPSHOT_SECOND_KEY", "second.example.com"), 0o600))
	t.Setenv("SNAPSHOT_FIRST_KEY", "first-value")
	t.Setenv("SNAPSHOT_SECOND_KEY", "second-value")
	configPath := filepath.Join(configDir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, "aliases:\n  selected:\n    path: %s\n    safety: strict\n", firstPath), 0o600))
	fakeDir := t.TempDir()
	calls := filepath.Join(fakeDir, "calls")
	newConfig := "aliases:\n  selected:\n    path: " + secondPath + "\n    safety: autonomous\n"
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "sandbox version") ;;
  "sandbox ls")
    printf '%%s' %q > %q
    printf '{"sandboxes":[]}\n' ;;
  "sandbox create"|"sandbox policy"|"sandbox exec") ;;
  *) exit 99 ;;
esac
`, calls, newConfig, configPath)
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"), []byte(script), 0o700))
	t.Setenv("PATH", fakeDir)
	cmd := newRunCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	rc := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}
	require.NoError(t, runInSandbox(t.Context(), cmd, []string{"selected"}, rc, false, true, sandbox.Options{}))
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	canonical, err := sandbox.CanonicalFilePath(firstPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), canonical)
	assert.Contains(t, string(data), "--safety strict")
	assert.Contains(t, string(data), "SNAPSHOT_FIRST_KEY")
	assert.NotContains(t, string(data), "SNAPSHOT_SECOND_KEY")
	assert.Contains(t, string(data), "first.example.com")
	assert.NotContains(t, string(data), "second.example.com")
	assert.NotContains(t, string(data), secondPath)
}
