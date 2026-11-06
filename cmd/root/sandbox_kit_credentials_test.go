package root

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

func TestSandboxKitDoesNotForwardGitHubCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX backend stub")
	}
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	t.Setenv("SANDBOX_VM_ID", "")
	t.Setenv("EXTRA_KEY", "extra-secret")
	workspace := t.TempDir()
	agentFile := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`providers:
  custom:
    provider: typesafe
    token_key: EXTRA_KEY
models:
  copilot:
    provider: github-copilot
    model: gpt-4.1
  custom:
    provider: custom
    model: test
agents:
  root:
    model: copilot
  other:
    model: custom
`), 0o600))
	fakeDir := t.TempDir()
	calls := filepath.Join(fakeDir, "calls")
	writeMockCLI(t, filepath.Join(fakeDir, "sbx"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1" in
  version|create|exec) ;;
  ls) printf '{"sandboxes":[]}\n' ;;
  *) exit 99 ;;
esac
`, calls))
	t.Setenv("PATH", fakeDir)

	for _, tokenName := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Run(tokenName, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("GH_TOKEN", "")
			t.Setenv(tokenName, "host-github-secret")
			require.NoError(t, os.WriteFile(calls, nil, 0o600))
			cmd := newRunCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			rc := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}
			require.NoError(t, runInSandbox(t.Context(), cmd, []string{agentFile}, rc, true, true,
				sandbox.Options{Workload: "docker/sbx-kit-docker-agent:edge"}))
			data, err := os.ReadFile(calls)
			require.NoError(t, err)
			var execCall string
			for line := range strings.SplitSeq(string(data), "\n") {
				if strings.HasPrefix(line, "exec ") {
					execCall = line
				}
			}
			require.NotEmpty(t, execCall)
			assert.Contains(t, execCall, "-e EXTRA_KEY")
			assert.NotContains(t, execCall, "GITHUB_TOKEN")
			assert.NotContains(t, execCall, "GH_TOKEN")
			assert.NotContains(t, execCall, "host-github-secret")
		})
	}
}
