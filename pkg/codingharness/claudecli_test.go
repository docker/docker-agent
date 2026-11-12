package codingharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

// fakeClaudeCLI scripts the probe's exec seams and records every invocation,
// so the tests never touch the developer's real `claude` installation.
type fakeClaudeCLI struct {
	installed  bool
	version    string
	versionErr error
	authOut    string
	authErr    error

	commands [][]string
}

func (f *fakeClaudeCLI) probe() ClaudeCLIProbe {
	return ClaudeCLIProbe{
		LookPath: func(file string) (string, error) {
			if !f.installed {
				return "", errors.New("executable file not found in $PATH")
			}
			return "/usr/local/bin/" + file, nil
		},
		RunOutput: func(_ context.Context, name string, args ...string) ([]byte, error) {
			f.commands = append(f.commands, append([]string{name}, args...))
			switch {
			case len(args) == 1 && args[0] == "--version":
				return []byte(f.version), f.versionErr
			default:
				return []byte(f.authOut), f.authErr
			}
		},
	}
}

const fakeAuthStatusJSON = `{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "email": "dev@example.com",
  "orgId": "org-1234",
  "orgName": "Example Org",
  "subscriptionType": "pro"
}`

func TestClaudeCLIProbe_NotInstalled(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: false}
	status := fake.probe().Probe(t.Context())

	assert.Equal(t, ClaudeStateNotInstalled, status.State)
	assert.False(t, status.Installed())
	assert.False(t, status.Authenticated())
	assert.Contains(t, status.Detail, "not found in PATH")
	assert.Empty(t, fake.commands, "nothing must be executed when the binary is missing")
}

func TestClaudeCLIProbe_Authenticated(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: true, version: "2.1.210 (Claude Code)\n", authOut: fakeAuthStatusJSON}
	status := fake.probe().Probe(t.Context())

	assert.Equal(t, ClaudeStateAuthenticated, status.State)
	assert.True(t, status.Installed())
	assert.True(t, status.Authenticated())
	assert.Equal(t, "2.1.210 (Claude Code)", status.Version)
	assert.Equal(t, "claude.ai", status.AuthMethod)
	assert.Equal(t, "firstParty", status.APIProvider)
	assert.Equal(t, "pro", status.SubscriptionType)
	assert.Equal(t, "auth: claude.ai, api: firstParty, subscription: pro", status.AuthSummary())

	require.Equal(t, [][]string{
		{"claude", "--version"},
		{"claude", "auth", "status", "--json"},
	}, fake.commands, "the probe must call the CLI directly, without a shell")
}

// The status must never retain identity fields: marshalling it back must not
// leak anything the auth-status document carried beyond the safe fields.
func TestClaudeCLIProbe_DropsIdentityFields(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: true, authOut: fakeAuthStatusJSON}
	status := fake.probe().Probe(t.Context())

	marshalled, err := json.Marshal(status)
	require.NoError(t, err)
	for _, sensitive := range []string{"email", "dev@example.com", "org", "Example"} {
		assert.NotContains(t, string(marshalled), sensitive)
	}
}

func TestClaudeCLIProbe_Unauthenticated(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: true, version: "2.1.210 (Claude Code)", authOut: `{"loggedIn": false}`}
	status := fake.probe().Probe(t.Context())

	assert.Equal(t, ClaudeStateUnauthenticated, status.State)
	assert.True(t, status.Installed())
	assert.False(t, status.Authenticated())
	assert.Equal(t, "2.1.210 (Claude Code)", status.Version)
}

// A logged-out CLI may exit non-zero while still printing the status JSON:
// the parsed document wins over the exit code.
func TestClaudeCLIProbe_LoggedOutWithNonZeroExit(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: true, authOut: `{"loggedIn": false}`, authErr: errors.New("exit status 1")}
	status := fake.probe().Probe(t.Context())

	assert.Equal(t, ClaudeStateUnauthenticated, status.State)
}

func TestClaudeCLIProbe_AuthCommandFails(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: true, version: "2.1.210 (Claude Code)", authErr: errors.New("exit status 1")}
	status := fake.probe().Probe(t.Context())

	assert.Equal(t, ClaudeStateAuthCheckFailed, status.State)
	assert.True(t, status.Installed())
	assert.False(t, status.Authenticated())
	assert.Contains(t, status.Detail, "exit status 1")
	assert.Equal(t, "2.1.210 (Claude Code)", status.Version, "the version survives a failed auth check")
}

func TestClaudeCLIProbe_InvalidStatusJSON(t *testing.T) {
	t.Parallel()

	for name, out := range map[string]string{
		"not json":       "please run claude auth login",
		"missing key":    `{"status": "ok"}`,
		"null document":  "null",
		"empty output":   "",
		"wrong type":     `{"loggedIn": "yes"}`,
		"array document": `[true]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeClaudeCLI{installed: true, authOut: out}
			status := fake.probe().Probe(t.Context())

			assert.Equal(t, ClaudeStateAuthCheckFailed, status.State)
			assert.Contains(t, status.Detail, "unexpected output")
			if out != "" {
				assert.NotContains(t, status.Detail, out, "raw CLI output must never be surfaced")
			}
		})
	}
}

func TestClaudeCLIProbe_VersionFailureDoesNotDecideState(t *testing.T) {
	t.Parallel()

	fake := &fakeClaudeCLI{installed: true, versionErr: errors.New("boom"), authOut: fakeAuthStatusJSON}
	status := fake.probe().Probe(t.Context())

	assert.Equal(t, ClaudeStateAuthenticated, status.State)
	assert.Empty(t, status.Version)
}

// The default seams run a real executable found via PATH, directly and
// without a shell. A fake `claude` script stands in for the CLI.
func TestClaudeCLIProbe_DefaultSeamsUseRealExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude executable is a shell script")
	}

	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then echo "9.9.9 (Claude Code)"; exit 0; fi
echo '%s'
`, `{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "max", "email": "dev@example.com"}`)
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700))
	t.Setenv("PATH", binDir)

	status := ProbeClaudeCLI(t.Context())

	assert.Equal(t, ClaudeStateAuthenticated, status.State)
	assert.Equal(t, "9.9.9 (Claude Code)", status.Version)
	assert.Equal(t, "max", status.SubscriptionType)
}

func TestClaudeCLIProbe_MissingBinaryWithDefaultSeams(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	status := ProbeClaudeCLI(t.Context())

	assert.Equal(t, ClaudeStateNotInstalled, status.State)
}

// useClaudeCompleteCLI puts a fake `claude` first on PATH: it records its
// arguments and stdin in dir, prints output and exits with code.
func useClaudeCompleteCLI(t *testing.T, output string, code int) (dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude executable is a shell script")
	}

	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out"), []byte(output), 0o600))
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat > %q\ncat %q\nexit %d\n",
		filepath.Join(dir, "args"), filepath.Join(dir, "stdin"), filepath.Join(dir, "out"), code)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestFactoryOnlyClaudeCodeCompletes(t *testing.T) {
	t.Parallel()

	p, err := Factory(&latest.HarnessConfig{Type: TypeClaudeCode})
	require.NoError(t, err)
	assert.Implements(t, (*harness.Completer)(nil), p)

	p, err = Factory(&latest.HarnessConfig{Type: TypeCodex})
	require.NoError(t, err)
	assert.NotImplements(t, (*harness.Completer)(nil), p)
}

func TestClaudeCodeComplete(t *testing.T) {
	dir := useClaudeCompleteCLI(t, `{"type":"result","subtype":"success","is_error":false,"result":"Fix the login bug"}`, 0)
	p, err := Factory(&latest.HarnessConfig{Type: TypeClaudeCode, Model: "opus", Effort: "max"})
	require.NoError(t, err)

	answer, err := p.(harness.Completer).Complete(t.Context(), "Be brief.", "--help me title this")
	require.NoError(t, err)
	assert.Equal(t, "Fix the login bug", answer)

	args, err := os.ReadFile(filepath.Join(dir, "args"))
	require.NoError(t, err)
	assert.Equal(t, strings.Join([]string{
		"--print", "--model", "haiku", "--system-prompt", "Be brief.",
		"--tools", "", "--strict-mcp-config", "--no-session-persistence",
		"--output-format", "json",
	}, "\n")+"\n", string(args), "a tool-free, unsaved prompt on the fast model, without the agent's flags")
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
	require.NoError(t, err)
	assert.Equal(t, "--help me title this", string(stdin), "the prompt goes through stdin, never argv")
}

// With verbose mode on in the user's config, Claude Code 2.1.310 prints every
// message of the run instead of the result alone.
func TestClaudeCodeCompleteVerboseOutput(t *testing.T) {
	for name, output := range map[string]string{
		"result last": `[
  {"type":"system","subtype":"init","tools":[],"mcp_servers":[],"model":"claude-haiku-5-5"},
  {"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fix the login bug"}]}},
  {"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}},
  {"type":"result","subtype":"success","is_error":false,"result":"Fix the login bug"}
]`,
		"notice after the result": `[
  {"type":"system","subtype":"init","tools":[],"mcp_servers":[],"model":"claude-haiku-5-5"},
  {"type":"result","subtype":"success","is_error":false,"result":"Fix the login bug"},
  {"type":"system","subtype":"informational","content":"notice"}
]`,
	} {
		t.Run(name, func(t *testing.T) {
			useClaudeCompleteCLI(t, output, 0)
			p, err := Factory(&latest.HarnessConfig{Type: TypeClaudeCode})
			require.NoError(t, err)

			answer, err := p.(harness.Completer).Complete(t.Context(), "system", "prompt")
			require.NoError(t, err)
			assert.Equal(t, "Fix the login bug", answer)
		})
	}
}

func TestClaudeCodeCompleteErrorsOmitOutput(t *testing.T) {
	for name, tt := range map[string]struct {
		output string
		code   int
		want   string
	}{
		"reported error": {output: `{"type":"result","is_error":true,"result":"private-output-marker"}`, want: "reported an error"},
		"not json":       {output: "private-output-marker", want: "unexpected output"},
		"not a result":   {output: `{"type":"private-output-marker"}`, want: "unexpected output"},
		"failed command": {output: "private-output-marker", code: 1, want: "exit status 1"},
		"verbose reported error": {
			output: `[{"type":"system","subtype":"init"},{"type":"result","is_error":true,"result":"private-output-marker"}]`,
			want:   "reported an error",
		},
		"verbose without result": {output: `[{"type":"assistant","message":"private-output-marker"}]`, want: "unexpected output"},
		"empty array":            {output: `[]`, want: "unexpected output"},
	} {
		t.Run(name, func(t *testing.T) {
			useClaudeCompleteCLI(t, tt.output, tt.code)
			p, err := Factory(&latest.HarnessConfig{Type: TypeClaudeCode})
			require.NoError(t, err)

			_, err = p.(harness.Completer).Complete(t.Context(), "system", "prompt")
			require.ErrorContains(t, err, tt.want)
			assert.NotContains(t, err.Error(), "private-output-marker")
		})
	}
}

func TestClaudeCLIStatus_AuthSummaryOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	assert.Empty(t, ClaudeCLIStatus{}.AuthSummary())
	assert.Equal(t, "subscription: pro", ClaudeCLIStatus{SubscriptionType: "pro"}.AuthSummary())
}
