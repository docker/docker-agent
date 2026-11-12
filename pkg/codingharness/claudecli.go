package codingharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
)

// Claude Code CLI probe states, from worst to best.
const (
	ClaudeStateNotInstalled    = "not-installed"
	ClaudeStateAuthCheckFailed = "auth-check-failed"
	ClaudeStateUnauthenticated = "unauthenticated"
	ClaudeStateAuthenticated   = "authenticated"
)

// ClaudeLoginCommand is the official interactive Claude Code login for a
// Claude (claude.ai) subscription. docker-agent never runs it implicitly:
// remediation messages print it, and the setup wizard only executes it after
// an explicit confirmation.
const ClaudeLoginCommand = "claude auth login --claudeai"

// ClaudeInstallDocsURL is where the Claude Code CLI install instructions live.
const ClaudeInstallDocsURL = "https://docs.anthropic.com/en/docs/claude-code"

const (
	claudeBinary = "claude"

	// claudeProbeTimeout bounds each CLI invocation of the probe so a hung
	// `claude` process cannot stall doctor or setup.
	claudeProbeTimeout = 15 * time.Second

	// claudeCompleteModel answers one-shot prompts: the small, fast model
	// Claude Code uses for its own background tasks.
	claudeCompleteModel = "haiku"

	// claudeCompleteWaitDelay bounds the wait for a canceled one-shot prompt
	// whose output pipe is still held open, e.g. by a child process.
	claudeCompleteWaitDelay = time.Second
)

// claudeLoginArgs is ClaudeLoginCommand as argv, kept next to the constant so
// the printed command and the executed one cannot drift apart.
var claudeLoginArgs = []string{"auth", "login", "--claudeai"}

// ClaudeCLIStatus reports the state of the locally installed Claude Code CLI.
// Only non-identifying fields of `claude auth status` are retained: the
// email, organization, and token material the command also reports are never
// parsed, stored, or printed.
type ClaudeCLIStatus struct {
	State            string `json:"state"`
	Version          string `json:"version,omitempty"`
	AuthMethod       string `json:"auth_method,omitempty"`
	APIProvider      string `json:"api_provider,omitempty"`
	SubscriptionType string `json:"subscription_type,omitempty"`
	// Detail explains the not-installed and auth-check-failed states. It only
	// ever carries exec-level error strings (exit status, context timeout),
	// never command output.
	Detail string `json:"detail,omitempty"`
}

// Installed reports whether the `claude` binary was found in PATH.
func (s ClaudeCLIStatus) Installed() bool {
	return s.State != "" && s.State != ClaudeStateNotInstalled
}

// Authenticated reports whether the CLI is logged in.
func (s ClaudeCLIStatus) Authenticated() bool {
	return s.State == ClaudeStateAuthenticated
}

// AuthSummary renders the safe auth metadata as a short human-readable
// fragment, e.g. "auth: claude.ai, api: firstParty, subscription: pro".
func (s ClaudeCLIStatus) AuthSummary() string {
	var parts []string
	if s.AuthMethod != "" {
		parts = append(parts, "auth: "+s.AuthMethod)
	}
	if s.APIProvider != "" {
		parts = append(parts, "api: "+s.APIProvider)
	}
	if s.SubscriptionType != "" {
		parts = append(parts, "subscription: "+s.SubscriptionType)
	}
	return strings.Join(parts, ", ")
}

// claudeAuthStatus mirrors only the non-identifying fields of
// `claude auth status --json`. The document also carries identity fields
// (email, orgId, orgName) that docker-agent must never retain; limiting the
// struct to safe fields makes that guarantee structural.
type claudeAuthStatus struct {
	// LoggedIn is a pointer so a JSON document without the loggedIn key is
	// distinguishable from loggedIn=false and rejected as unexpected output.
	LoggedIn         *bool  `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	SubscriptionType string `json:"subscriptionType"`
}

// ClaudeCLIProbe checks the installation and login state of the Claude Code
// CLI. The zero value probes the real `claude` binary from PATH; tests inject
// the seams.
type ClaudeCLIProbe struct {
	// LookPath defaults to exec.LookPath.
	LookPath func(file string) (string, error)
	// RunOutput runs a command and returns its stdout. It defaults to a
	// direct exec.CommandContext invocation (never a shell) bounded by
	// Timeout.
	RunOutput func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Timeout bounds each CLI invocation; defaults to claudeProbeTimeout.
	Timeout time.Duration
}

// ProbeClaudeCLI probes the Claude Code CLI with the default seams. It never
// starts a login or a browser; it only observes.
func ProbeClaudeCLI(ctx context.Context) ClaudeCLIStatus {
	return ClaudeCLIProbe{}.Probe(ctx)
}

// Probe distinguishes four states: the binary is not installed, the auth
// check could not be completed, the CLI is installed but not logged in, and
// the CLI is logged in.
func (p ClaudeCLIProbe) Probe(ctx context.Context) ClaudeCLIStatus {
	lookPath := p.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	run := p.RunOutput
	if run == nil {
		run = p.runOutput
	}

	if _, err := lookPath(claudeBinary); err != nil {
		return ClaudeCLIStatus{
			State:  ClaudeStateNotInstalled,
			Detail: "the `claude` CLI was not found in PATH",
		}
	}

	status := ClaudeCLIStatus{}
	// A failed --version is not fatal on its own: the auth check below
	// decides the state, and a broken binary fails there too.
	if out, err := run(ctx, claudeBinary, "--version"); err == nil {
		status.Version = firstLine(out)
	}

	out, runErr := run(ctx, claudeBinary, "auth", "status", "--json")

	// `claude auth status` may exit non-zero when logged out, so trust the
	// JSON whenever the output parses and report a failed check otherwise.
	var auth claudeAuthStatus
	if err := json.Unmarshal(out, &auth); err != nil || auth.LoggedIn == nil {
		status.State = ClaudeStateAuthCheckFailed
		if runErr != nil {
			status.Detail = "`claude auth status` failed: " + runErr.Error()
		} else {
			status.Detail = "`claude auth status --json` returned unexpected output"
		}
		return status
	}

	if !*auth.LoggedIn {
		status.State = ClaudeStateUnauthenticated
		return status
	}

	status.State = ClaudeStateAuthenticated
	status.AuthMethod = auth.AuthMethod
	status.APIProvider = auth.APIProvider
	status.SubscriptionType = auth.SubscriptionType
	return status
}

func (p ClaudeCLIProbe) runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = claudeProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Direct invocation: name and args reach the binary as-is, no shell.
	return exec.CommandContext(ctx, name, args...).Output()
}

func firstLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// claudeCodeProvider is the Claude Code harness driver. It also implements
// harness.Completer, so harness agents get session titles, and
// harness.Eliciter, so Claude Code's questions reach the user.
type claudeCodeProvider struct {
	provider

	elicit tools.ElicitationHandler
}

// Complete answers prompt with the claude CLI's fast model, without tools, MCP
// servers or a saved session. Like the probe, its errors never carry output.
func (claudeCodeProvider) Complete(ctx context.Context, systemPrompt, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, claudeBinary, claudeCompleteArgs(systemPrompt)...)
	// Through stdin, a long or dash-led prompt is never parsed as arguments.
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = claudeCompleteWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("`claude --print` failed: %w", err)
	}

	result, ok := claudeFinalResult(out)
	if !ok {
		return "", errors.New("`claude --print` returned unexpected output")
	}
	if result.IsError {
		return "", errors.New("`claude --print` reported an error")
	}
	return result.Result, nil
}

// claudeResult is the result message of `claude --print --output-format json`.
type claudeResult struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// claudeFinalResult returns the run's last result message. When verbose mode is
// on in the user's config, the CLI prints every message as an array instead,
// and system notices can follow the result.
func claudeFinalResult(out []byte) (claudeResult, bool) {
	var messages []json.RawMessage
	if json.Unmarshal(out, &messages) != nil {
		messages = []json.RawMessage{out}
	}
	for _, message := range slices.Backward(messages) {
		var result claudeResult
		if json.Unmarshal(message, &result) == nil && result.Type == "result" {
			return result, true
		}
	}
	return claudeResult{}, false
}

func claudeCompleteArgs(systemPrompt string) []string {
	return []string{
		"--print",
		"--model", claudeCompleteModel,
		"--system-prompt", systemPrompt,
		// An empty list disables every built-in tool.
		"--tools", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--output-format", "json",
	}
}

// RunClaudeLogin runs the official interactive Claude Code login
// (ClaudeLoginCommand) attached to this process's terminal, so the login
// lands in the exact OS user and environment docker-agent runs as. Callers
// must only invoke it after an explicit user confirmation: the command is
// interactive and opens a browser.
func RunClaudeLogin(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, claudeBinary, claudeLoginArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
