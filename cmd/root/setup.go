package root

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/codingharness"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/input"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// errSetupCancelled is returned when the user aborts the wizard (EOF or an
// explicit quit) rather than a step failing.
var errSetupCancelled = errors.New("setup cancelled")

// errNoUsableModel is the concise error returned when the setup offer is
// declined or cancelled: the full failure was already printed just above the
// offer, so returning the original error would print it twice.
var errNoUsableModel = invocationError("no usable model is configured; run `{{docker_agent_command}} setup` or see `{{docker_agent_command}} doctor`")

// errClaudeHarnessConfigured ends an offered setup that took the Claude Code
// harness path: the wizard configured an agent file for the external CLI,
// not a credential the failed invocation could pick up, so the original run
// never happened and must not exit as a success. The message stays concise:
// the wizard already printed the agent file (or its content) and the exact
// command to run.
var errClaudeHarnessConfigured = invocationError("this run was not retried: setup configured a Claude Code harness agent; start it with the `{{docker_agent_command}} run <file>` command shown above")

// invocationError is a sentinel error whose message names the runnable
// command for the current invocation mode. The {{docker_agent_command}}
// placeholder (shared with pkg/creator/instructions.txt) is resolved at
// Error() time so the mode is read when the message is rendered.
type invocationError string

func (e invocationError) Error() string {
	return strings.ReplaceAll(string(e), "{{docker_agent_command}}", invocation.DockerAgent())
}

// setupResult reports what the wizard configured, so the caller that offered
// setup after a failed run can retry with the new credential in place.
type setupResult struct {
	// EnvVar and Value are set when the cloud path stored an API key.
	EnvVar string
	Value  string
	// Model is set when the local path selected or pulled a DMR model.
	Model string
	// ProviderName and Provider are set when the custom path registered an
	// OpenAI-compatible provider in the user config.
	ProviderName string
	Provider     *latest.ProviderConfig
	// ClaudeHarness marks the Claude Code harness path, which configures an
	// external CLI instead of a model provider. AgentFile is the local agent
	// configuration it wrote; it is empty when the user declined to overwrite
	// an existing file and the config was printed instead.
	ClaudeHarness bool
	AgentFile     string
}

// setupWizard drives the interactive model setup. The function fields are
// seams: production wiring talks to the terminal, the secret stores, and
// Docker Model Runner, while tests inject scripted answers and fakes.
//
// in is buffered once at construction: a fresh bufio.Reader per prompt would
// drop the read-ahead it buffered beyond the first line.
type setupWizard struct {
	in  *bufio.Reader
	out io.Writer

	readSecret     func(prompt string) (string, error)
	stores         []environment.SecretStore
	dmrLister      config.DMRModelLister
	pullModel      func(ctx context.Context, model string) error
	chatgptLogin   func(ctx context.Context, out io.Writer) (*chatgpt.LoginResult, error)
	saveProvider   func(name string, provider latest.ProviderConfig) error
	listModels     func(ctx context.Context, baseURL, token string) []string
	probeClaudeCLI func(ctx context.Context) codingharness.ClaudeCLIStatus
	claudeLogin    func(ctx context.Context) error

	// agentFileDir is where generated agent files are written: the current
	// directory in production, a temp dir in tests.
	agentFileDir string
}

func newSetupCmd() *cobra.Command {
	command := invocation.DockerAgent()
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactively set up a model (built-in provider, local, custom endpoint, or Claude Code)",
		Long: fmt.Sprintf(`Set up a model for docker agent, interactively.

Four paths:
  - Built-in cloud provider: pick a provider docker agent already knows
    (Anthropic, OpenAI, Google, Groq, Hugging Face, ...) and connect it.
    Credentials vary by provider: most paste an API key or token, stored in
    the docker agent env file (~/.config/cagent/.env), while chatgpt signs
    in with your ChatGPT account in the browser instead.
  - Local model: check Docker Model Runner and pull a model. No API key needed.
  - Custom OpenAI-compatible endpoint: for endpoints that are not built in
    (vLLM, LiteLLM, a corporate gateway, ...). Register the endpoint with
    its base URL, API format, and API key variable; the provider is saved
    to your user configuration and its models become usable everywhere via
    --model <name>/<model>.
  - Claude Code harness: use your Claude subscription through the official
    'claude' CLI. Checks that the CLI is installed and logged in (offering
    'claude auth login --claudeai'), then writes a ready-to-run agent file.
    No API key needed; docker agent never reads or copies the CLI's
    credentials.

Ends with the exact command to start chatting. Secret values are never
printed. Check the result anytime with '%s doctor'.`, command),
		Example:      `  docker-agent setup`,
		GroupID:      "core",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) (commandErr error) {
			ctx := cmd.Context()
			telemetry.TrackCommand(ctx, "setup", args)
			defer func() { // do not inline this defer so that commandErr is not resolved early
				telemetry.TrackCommandError(ctx, "setup", args, commandErr)
			}()

			if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
				return fmt.Errorf("%s setup is interactive and needs a terminal\n"+
					"Without one, set a provider API key directly (e.g. export ANTHROPIC_API_KEY=<value>)\n"+
					"or pull a local model with `docker model pull ai/qwen3`.\n"+
					"See %s for every secret source", command, environment.SecretsDocsURL)
			}

			wizard := newTerminalSetupWizard(cmd.InOrStdin(), cmd.OutOrStdout())
			_, err := wizard.run(ctx)
			if errors.Is(err, errSetupCancelled) {
				fmt.Fprintln(cmd.OutOrStdout(), "Setup cancelled.")
				return nil
			}
			return err
		},
	}
}

// newTerminalSetupWizard wires a wizard to the real terminal, secret stores,
// and Docker Model Runner.
func newTerminalSetupWizard(in io.Reader, out io.Writer) *setupWizard {
	return &setupWizard{
		in:  bufio.NewReader(in),
		out: out,
		readSecret: func(prompt string) (string, error) {
			fmt.Fprint(out, prompt)
			// Read directly from the terminal fd: the key must not echo.
			value, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(out)
			if err != nil {
				return "", fmt.Errorf("reading the credential: %w", err)
			}
			return string(value), nil
		},
		stores:       environment.SecretStores(),
		dmrLister:    dmr.ListModels,
		pullModel:    dmr.Pull,
		chatgptLogin: chatgpt.Login,
		saveProvider: func(name string, provider latest.ProviderConfig) error {
			return userconfig.Update(func(cfg *userconfig.Config) error {
				return cfg.SetProvider(name, provider)
			})
		},
		listModels:     fetchOpenAICompatibleModels,
		probeClaudeCLI: codingharness.ProbeClaudeCLI,
		claudeLogin:    codingharness.RunClaudeLogin,
		agentFileDir:   ".",
	}
}

// run executes the wizard: choose cloud or local, configure it, and print the
// command to start chatting.
func (w *setupWizard) run(ctx context.Context) (*setupResult, error) {
	fmt.Fprintln(w.out, "Let's set up a model for docker agent.")
	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "How do you want to run models?")
	fmt.Fprintln(w.out, "  1. Built-in cloud provider (Anthropic, OpenAI, Groq, Hugging Face, ...)")
	fmt.Fprintln(w.out, "  2. Local model via Docker Model Runner (no API key)")
	fmt.Fprintln(w.out, "  3. Custom OpenAI-compatible endpoint (your own base URL, e.g. vLLM, LiteLLM)")
	fmt.Fprintln(w.out, "  4. Claude Code harness (Claude subscription via the official `claude` CLI)")

	choice, err := w.promptChoice(ctx, 4, 1)
	if err != nil {
		return nil, err
	}

	var result *setupResult
	switch choice {
	case 1:
		result, err = w.setupCloudProvider(ctx)
	case 2:
		result, err = w.setupLocalModel(ctx)
	case 3:
		result, err = w.setupCustomProvider(ctx)
	default:
		result, err = w.setupClaudeHarness(ctx)
	}
	if err != nil {
		return nil, err
	}

	w.printNextSteps(result)
	return result, nil
}

// setupCloudProvider walks the cloud path: pick a provider, paste its key,
// and persist it in a secret store.
func (w *setupWizard) setupCloudProvider(ctx context.Context) (*setupResult, error) {
	providers := config.CloudProviderEnvVars()

	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "Pick a provider:")
	for i, p := range providers {
		credential := p.EnvVars[0]
		if p.Provider == chatgpt.ProviderName {
			credential = "ChatGPT account sign-in, no API key"
		}
		fmt.Fprintf(w.out, "  %2d. %-15s (%s)\n", i+1, p.Provider, credential)
	}

	choice, err := w.promptChoice(ctx, len(providers), 1)
	if err != nil {
		return nil, err
	}
	selected := providers[choice-1]
	envVar := selected.EnvVars[0]

	// The chatgpt provider signs in with a browser flow instead of a pasted
	// API key; the credential is stored by the login itself.
	if selected.Provider == chatgpt.ProviderName {
		fmt.Fprintln(w.out)
		result, err := w.chatgptLogin(ctx, w.out)
		if err != nil {
			return nil, err
		}
		if result.Email != "" {
			fmt.Fprintf(w.out, "Signed in as %s.\n", result.Email)
		} else {
			fmt.Fprintln(w.out, "Signed in with your ChatGPT account.")
		}
		return &setupResult{Model: selected.Provider + "/" + config.DefaultModels[selected.Provider]}, nil
	}

	key, err := w.promptSecret(ctx, fmt.Sprintf("\nPaste your %s credential (%s, input hidden): ", selected.Provider, envVar))
	if err != nil {
		return nil, err
	}

	if err := w.storeSecret(ctx, envVar, key); err != nil {
		return nil, err
	}

	return &setupResult{EnvVar: envVar, Value: key, Model: selected.Provider + "/" + config.DefaultModels[selected.Provider]}, nil
}

// promptSecret asks for the secret until a non-empty value is entered.
func (w *setupWizard) promptSecret(ctx context.Context, prompt string) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, err := w.readSecret(prompt)
		if err != nil {
			return "", err
		}
		if key = strings.TrimSpace(key); key != "" {
			return key, nil
		}
		fmt.Fprintln(w.out, "The key is empty; paste it or press Ctrl+C to cancel.")
	}
}

// storeSecret persists the key in a secret store. When several stores are
// available it asks which one to use, re-asking when a store fails so the
// pasted key is not lost to a storage hiccup.
func (w *setupWizard) storeSecret(ctx context.Context, envVar, key string) error {
	if len(w.stores) == 0 {
		return errors.New("no secret store is available to save the key")
	}
	for {
		store := w.stores[0]
		if len(w.stores) > 1 {
			fmt.Fprintln(w.out)
			fmt.Fprintf(w.out, "Where should %s be stored?\n", envVar)
			for i, s := range w.stores {
				fmt.Fprintf(w.out, "  %d. %s\n", i+1, s.Description())
			}

			choice, err := w.promptChoice(ctx, len(w.stores), 1)
			if err != nil {
				return err
			}
			store = w.stores[choice-1]
		}

		if err := store.Store(ctx, envVar, key); err != nil {
			if len(w.stores) == 1 {
				return fmt.Errorf("storing the key: %w", err)
			}
			fmt.Fprintf(w.out, "Could not store the key: %v\nPick another location, or press Ctrl+C to cancel.\n", err)
			continue
		}

		fmt.Fprintf(w.out, "\nStored %s in the %s.\n", envVar, store.Description())
		return nil
	}
}

// setupLocalModel walks the local path: check Docker Model Runner and make
// sure at least one model is pulled.
func (w *setupWizard) setupLocalModel(ctx context.Context) (*setupResult, error) {
	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "Checking Docker Model Runner...")

	models, err := w.dmrLister(ctx)
	switch {
	case errors.Is(err, dmr.ErrNotInstalled):
		return nil, fmt.Errorf("cannot use a local model: Docker Model Runner is not installed.\n"+
			"Install it (%s), then run `%s setup` again", dmrDocsURL, invocation.DockerAgent())
	case err != nil:
		return nil, fmt.Errorf("cannot use a local model: Docker Model Runner is not reachable: %w\n"+
			"Start it (or install it: %s), then run `%s setup` again", err, dmrDocsURL, invocation.DockerAgent())
	}

	if len(models) > 0 {
		fmt.Fprintf(w.out, "Docker Model Runner is ready with %d model(s) pulled:\n", len(models))
		for _, m := range models {
			fmt.Fprintf(w.out, "  - %s\n", m)
		}
		model, _ := config.PickDMRModel(ctx, config.DefaultModels["dmr"], func(context.Context) ([]string, error) { return models, nil })
		return &setupResult{Model: "dmr/" + model}, nil
	}

	defaultModel := config.DefaultModels["dmr"]
	fmt.Fprintln(w.out, "Docker Model Runner is reachable but no model is pulled yet.")
	fmt.Fprintf(w.out, "Model to pull [%s]: ", defaultModel)

	model, err := w.readLine(ctx)
	if err != nil {
		return nil, err
	}
	if model = strings.TrimSpace(model); model == "" {
		model = defaultModel
	}

	if err := w.pullModel(ctx, model); err != nil {
		return nil, err
	}

	return &setupResult{Model: "dmr/" + model}, nil
}

// customAPITypes maps the wizard's API-format menu entries to the api_type
// values understood by the providers section.
var customAPITypes = []string{"openai_chatcompletions", "openai_responses"}

// setupCustomProvider walks the custom path: name an OpenAI-compatible
// provider, point it at an endpoint, pick the API format, optionally wire an
// API key, and save the provider to the user config so every command can use
// its models via --model <name>/<model>.
func (w *setupWizard) setupCustomProvider(ctx context.Context) (*setupResult, error) {
	name, err := w.promptProviderName(ctx)
	if err != nil {
		return nil, err
	}

	baseURL, err := w.promptBaseURL(ctx)
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "Which API format does the endpoint use?")
	fmt.Fprintln(w.out, "  1. Chat Completions (/chat/completions, most common)")
	fmt.Fprintln(w.out, "  2. Responses (/responses)")
	formatChoice, err := w.promptChoice(ctx, len(customAPITypes), 1)
	if err != nil {
		return nil, err
	}

	envVar, key, err := w.promptCustomProviderKey(ctx, name)
	if err != nil {
		return nil, err
	}

	providerCfg := latest.ProviderConfig{
		BaseURL:  baseURL,
		APIType:  customAPITypes[formatChoice-1],
		TokenKey: envVar,
	}

	model, err := w.promptCustomProviderModel(ctx, baseURL, key)
	if err != nil {
		return nil, err
	}

	if err := w.saveProvider(name, providerCfg); err != nil {
		return nil, fmt.Errorf("saving provider %q to the user config: %w", name, err)
	}
	fmt.Fprintf(w.out, "\nSaved provider %q (%s) to %s.\n", name, baseURL, userconfig.Path())

	result := &setupResult{EnvVar: envVar, Value: key, ProviderName: name, Provider: &providerCfg}
	if model != "" {
		result.Model = name + "/" + model
	}
	return result, nil
}

// promptProviderName asks for the custom provider's name until a valid,
// non-conflicting one is entered. Built-in provider names are rejected so a
// custom definition never silently shadows them.
func (w *setupWizard) promptProviderName(ctx context.Context) (string, error) {
	for {
		fmt.Fprint(w.out, "\nProvider name (e.g. myprovider): ")
		name, err := w.readLine(ctx)
		if err != nil {
			return "", err
		}
		name = strings.TrimSpace(name)
		switch {
		case name == "":
			fmt.Fprintln(w.out, "The name is empty; enter one or press Ctrl+C to cancel.")
		case strings.ContainsAny(name, "/ \t"):
			fmt.Fprintln(w.out, "The name cannot contain '/' or whitespace.")
		case name == "auto" || provider.IsKnownProvider(name):
			fmt.Fprintf(w.out, "%q is a built-in provider name; pick another one.\n", name)
		default:
			return name, nil
		}
	}
}

// promptBaseURL asks for the endpoint's base URL until an absolute http(s)
// URL is entered.
func (w *setupWizard) promptBaseURL(ctx context.Context) (string, error) {
	for {
		fmt.Fprint(w.out, "Base URL of the API endpoint (e.g. https://api.example.com/v1): ")
		baseURL, err := w.readLine(ctx)
		if err != nil {
			return "", err
		}
		baseURL = strings.TrimSpace(baseURL)
		u, parseErr := url.Parse(baseURL)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fmt.Fprintln(w.out, "Enter an absolute http(s) URL, e.g. https://api.example.com/v1.")
			continue
		}
		return baseURL, nil
	}
}

// promptCustomProviderKey asks which environment variable holds the API key
// (empty for an unauthenticated endpoint) and, when one is named, collects
// and stores the key like the cloud path does. The raw key is also returned
// so the wizard can authenticate its model-discovery request.
func (w *setupWizard) promptCustomProviderKey(ctx context.Context, name string) (envVar, key string, err error) {
	for {
		fmt.Fprint(w.out, "Environment variable that holds the API key (empty if none is needed): ")
		envVar, err = w.readLine(ctx)
		if err != nil {
			return "", "", err
		}
		envVar = strings.TrimSpace(envVar)
		if envVar == "" {
			return "", "", nil
		}
		if !isValidEnvVarName(envVar) {
			fmt.Fprintln(w.out, "Enter a valid environment variable name (letters, digits and underscores, e.g. MYPROVIDER_API_KEY), or leave it empty.")
			continue
		}
		break
	}

	key, err = w.promptSecret(ctx, fmt.Sprintf("\nPaste your %s API key (%s, input hidden): ", name, envVar))
	if err != nil {
		return "", "", err
	}
	if err := w.storeSecret(ctx, envVar, key); err != nil {
		return "", "", err
	}
	return envVar, key, nil
}

// promptCustomProviderModel queries the endpoint for its models to validate
// the configuration and suggest a default, then asks which model to use.
// Discovery failures are not fatal: the user can type a model ID manually or
// leave it empty and discover models later with `docker agent models`.
func (w *setupWizard) promptCustomProviderModel(ctx context.Context, baseURL, key string) (string, error) {
	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "Checking the endpoint for available models...")

	models := w.listModels(ctx, baseURL, key)
	var defaultModel string
	if len(models) > 0 {
		fmt.Fprintf(w.out, "The endpoint lists %d model(s):\n", len(models))
		for i, m := range models {
			if i == maxModelsShown {
				fmt.Fprintf(w.out, "  ... and %d more (see `%s models`)\n", len(models)-maxModelsShown, invocation.DockerAgent())
				break
			}
			fmt.Fprintf(w.out, "  - %s\n", m)
		}
		defaultModel = models[0]
	} else {
		fmt.Fprintln(w.out, "Could not list models from the endpoint; you can enter one manually.")
	}

	if defaultModel != "" {
		fmt.Fprintf(w.out, "Model to use [%s]: ", defaultModel)
	} else {
		fmt.Fprint(w.out, "Model to use (leave empty to pick one later): ")
	}
	model, err := w.readLine(ctx)
	if err != nil {
		return "", err
	}
	if model = strings.TrimSpace(model); model == "" {
		model = defaultModel
	}
	return model, nil
}

// maxModelsShown caps the endpoint's discovered-model listing so a provider
// serving hundreds of models does not flood the wizard.
const maxModelsShown = 10

// envVarNameRe matches portable environment variable names.
var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func isValidEnvVarName(name string) bool {
	return envVarNameRe.MatchString(name)
}

// claudeAgentFileName is the agent configuration the Claude Code harness
// path writes into the current directory.
const claudeAgentFileName = "claude-code-agent.yaml"

// claudeEffortLevels are the values the Claude Code CLI accepts for --effort.
var claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

const defaultClaudeEffort = "medium"

// setupClaudeHarness walks the Claude Code harness path: verify the official
// `claude` CLI is installed and logged in (offering the official login,
// never running it without confirmation), pick a model override and effort,
// and write a ready-to-run agent file. No API key and no token handling:
// docker agent only checks the CLI's state and never reads its credentials.
func (w *setupWizard) setupClaudeHarness(ctx context.Context) (*setupResult, error) {
	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "Checking the Claude Code CLI...")

	status := w.probeClaudeCLI(ctx)
	if !status.Installed() {
		return nil, fmt.Errorf("cannot use the Claude Code harness: the `claude` CLI was not found in PATH.\n"+
			"Install Claude Code (%s) and log in with `%s`, then run `%s setup` again",
			codingharness.ClaudeInstallDocsURL, codingharness.ClaudeLoginCommand, invocation.DockerAgent())
	}

	if status.Authenticated() {
		fmt.Fprintf(w.out, "Claude Code%s is installed and logged in (%s).\n", claudeVersionSuffix(status), status.AuthSummary())
	} else if err := w.claudeLoginFlow(ctx, status); err != nil {
		return nil, err
	}

	model, err := w.promptClaudeModel(ctx)
	if err != nil {
		return nil, err
	}
	effort, err := w.promptClaudeEffort(ctx)
	if err != nil {
		return nil, err
	}

	content, err := claudeAgentYAML(model, effort)
	if err != nil {
		return nil, fmt.Errorf("generating the agent configuration: %w", err)
	}

	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "Heads-up: the harness runs the official `claude` CLI non-interactively with")
	fmt.Fprintln(w.out, "its own tools and skips Claude Code's permission prompts")
	fmt.Fprintln(w.out, "(--dangerously-skip-permissions). Use it in a repository you trust, and prefer")
	fmt.Fprintf(w.out, "`%s run --worktree` to isolate its changes in a git worktree.\n", invocation.DockerAgent())
	fmt.Fprintln(w.out, "`--sandbox` does not carry the `claude` CLI or its login into the sandbox;")
	fmt.Fprintln(w.out, "use it only with a sandbox image that ships and authenticates the CLI.")

	path, err := w.writeClaudeAgentFile(ctx, content)
	if err != nil {
		return nil, err
	}
	if path == "" {
		// Declined overwrite: the copyable config and the run command were
		// already printed by writeClaudeAgentFile.
		return &setupResult{ClaudeHarness: true}, nil
	}

	fmt.Fprintf(w.out, "\nWrote %s.\n", path)
	return &setupResult{ClaudeHarness: true, AgentFile: path}, nil
}

// claudeLoginFlow offers the official interactive login for a CLI that is
// installed but not (verifiably) logged in. The login only runs after an
// explicit yes, and the state is re-probed afterwards so a failed or aborted
// sign-in cannot end the wizard in a broken state.
func (w *setupWizard) claudeLoginFlow(ctx context.Context, status codingharness.ClaudeCLIStatus) error {
	if status.State == codingharness.ClaudeStateAuthCheckFailed {
		fmt.Fprintf(w.out, "Claude Code%s is installed but the login check failed: %s.\n", claudeVersionSuffix(status), status.Detail)
	} else {
		fmt.Fprintf(w.out, "Claude Code%s is installed but not logged in.\n", claudeVersionSuffix(status))
	}
	fmt.Fprintln(w.out, "Log in with your Claude subscription using the official command, run as the")
	fmt.Fprintln(w.out, "same OS user and environment as docker agent:")
	fmt.Fprintln(w.out)
	fmt.Fprintf(w.out, "  %s\n", codingharness.ClaudeLoginCommand)
	fmt.Fprintln(w.out)
	fmt.Fprint(w.out, "Run it now? ([y]es/[n]o): ")

	answer, err := w.readLine(ctx)
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("the Claude Code CLI is not logged in.\n"+
			"Run `%s` yourself, then run `%s setup` again", codingharness.ClaudeLoginCommand, invocation.DockerAgent())
	}

	fmt.Fprintln(w.out)
	if err := w.claudeLogin(ctx); err != nil {
		return fmt.Errorf("`%s` failed: %w", codingharness.ClaudeLoginCommand, err)
	}

	status = w.probeClaudeCLI(ctx)
	if !status.Authenticated() {
		return fmt.Errorf("the `claude` CLI still reports it is not logged in after the login attempt.\n"+
			"Check `claude auth status` as the same OS user and environment that run docker agent,\n"+
			"then run `%s setup` again", invocation.DockerAgent())
	}

	fmt.Fprintf(w.out, "Logged in (%s).\n", status.AuthSummary())
	return nil
}

// claudeVersionSuffix renders the probed version for a sentence that already
// starts with "Claude Code": `claude --version` reports e.g.
// "2.1.210 (Claude Code)", so the product-name suffix is dropped to avoid
// printing "Claude Code 2.1.210 (Claude Code)".
func claudeVersionSuffix(status codingharness.ClaudeCLIStatus) string {
	version := strings.TrimSpace(strings.TrimSuffix(status.Version, "(Claude Code)"))
	if version == "" {
		return ""
	}
	return " " + version
}

// promptClaudeModel asks for the optional model override forwarded to the
// CLI; empty keeps Claude Code's own default model.
func (w *setupWizard) promptClaudeModel(ctx context.Context) (string, error) {
	fmt.Fprintln(w.out)
	fmt.Fprint(w.out, "Claude model override, an alias like sonnet/opus/haiku or a full model ID\n(leave empty for the Claude Code default): ")
	model, err := w.readLine(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(model), nil
}

// promptClaudeEffort asks for the effort level forwarded as --effort,
// re-asking until the answer is one of the levels the CLI accepts.
func (w *setupWizard) promptClaudeEffort(ctx context.Context) (string, error) {
	levels := strings.Join(claudeEffortLevels, ", ")
	for {
		fmt.Fprintf(w.out, "Effort level (%s) [%s]: ", levels, defaultClaudeEffort)
		answer, err := w.readLine(ctx)
		if err != nil {
			return "", err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" {
			return defaultClaudeEffort, nil
		}
		if slices.Contains(claudeEffortLevels, answer) {
			return answer, nil
		}
		fmt.Fprintf(w.out, "Enter one of %s, or leave it empty for %s.\n", levels, defaultClaudeEffort)
	}
}

// claudeAgentYAML renders the generated agent configuration: a single root
// agent that delegates to the claude-code harness. It never contains a
// credential; the harness uses the CLI's own login at run time.
func claudeAgentYAML(model, effort string) ([]byte, error) {
	harness := yaml.MapSlice{{Key: "type", Value: codingharness.TypeClaudeCode}}
	if model != "" {
		harness = append(harness, yaml.MapItem{Key: "model", Value: model})
	}
	harness = append(harness, yaml.MapItem{Key: "effort", Value: effort})

	return yaml.Marshal(yaml.MapSlice{
		{Key: "agents", Value: yaml.MapSlice{
			{Key: "root", Value: yaml.MapSlice{
				{Key: "description", Value: "Claude Code running on your Claude subscription"},
				{Key: "harness", Value: harness},
			}},
		}},
	})
}

// writeClaudeAgentFile writes the generated configuration to
// claude-code-agent.yaml in the wizard's agent-file directory. An existing
// file is only replaced after explicit confirmation; on decline the complete
// config is printed instead so nothing is lost. Returns the written path, or
// "" when the user declined to overwrite.
func (w *setupWizard) writeClaudeAgentFile(ctx context.Context, content []byte) (string, error) {
	path := filepath.Join(w.agentFileDir, claudeAgentFileName)
	if _, err := os.Lstat(path); err == nil {
		fmt.Fprintf(w.out, "\n%s already exists. Overwrite it? ([y]es/[n]o): ", path)
		answer, err := w.readLine(ctx)
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintf(w.out, "\nKeeping %s. Save this configuration to a file of your choice:\n\n%s\n", path, content)
			fmt.Fprintf(w.out, "Then start it with `%s run <file>`.\n", invocation.DockerAgent())
			return "", nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking %s: %w", path, err)
	}

	if err := atomicWriteFile(path, content); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// atomicWriteFile writes through a temp file in the same directory plus a
// rename, so an interrupted write never leaves a truncated config behind.
func atomicWriteFile(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	// 0o644 like a hand-written config: the file carries no secret.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// printNextSteps ends the wizard with ready-to-copy commands.
func (w *setupWizard) printNextSteps(result *setupResult) {
	fmt.Fprintln(w.out)
	command := invocation.DockerAgent()

	// The harness runs through a generated agent file, not a --model flag:
	// the harness model belongs to the external CLI, not to a docker agent
	// model provider.
	if result.ClaudeHarness {
		if result.AgentFile != "" {
			fmt.Fprintln(w.out, "You're all set. Start the Claude Code agent with:")
			fmt.Fprintln(w.out)
			fmt.Fprintf(w.out, "  %s run %s\n", command, result.AgentFile)
			fmt.Fprintln(w.out)
			fmt.Fprintf(w.out, "Check the harness anytime with `%s doctor %s`.\n", command, result.AgentFile)
		} else {
			fmt.Fprintf(w.out, "Check your setup anytime with `%s doctor <file>`.\n", command)
		}
		return
	}

	// Auto-selection never picks a custom provider, so its next steps must
	// carry the explicit --model reference (or how to find one) instead of
	// the bare `docker agent run`.
	if result.ProviderName != "" {
		if result.Model != "" {
			fmt.Fprintln(w.out, "You're all set. Start chatting with:")
			fmt.Fprintln(w.out)
			fmt.Fprintf(w.out, "  %s run --model %s\n", command, result.Model)
		} else {
			fmt.Fprintf(w.out, "Provider %q is set up. List its models with:\n", result.ProviderName)
			fmt.Fprintln(w.out)
			fmt.Fprintf(w.out, "  %s models --provider %s\n", command, result.ProviderName)
			fmt.Fprintln(w.out)
			fmt.Fprintln(w.out, "Then start chatting with:")
			fmt.Fprintln(w.out)
			fmt.Fprintf(w.out, "  %s run --model %s/<model>\n", command, result.ProviderName)
		}
		fmt.Fprintln(w.out)
		fmt.Fprintf(w.out, "Check your setup anytime with `%s doctor`.\n", command)
		return
	}

	fmt.Fprintln(w.out, "You're all set. Start chatting with:")
	fmt.Fprintln(w.out)
	fmt.Fprintf(w.out, "  %s run\n", command)
	fmt.Fprintln(w.out)
	if result.Model != "" {
		fmt.Fprintf(w.out, "Or pin the model explicitly: %s run --model %s\n", command, result.Model)
	}
	fmt.Fprintf(w.out, "Check your setup anytime with `%s doctor`.\n", command)
}

// promptChoice reads a 1-based menu choice, re-asking on invalid input. An
// empty answer selects def; EOF cancels the wizard.
//
//nolint:unparam // def is 1 for every current menu; the parameter documents the default-choice contract
func (w *setupWizard) promptChoice(ctx context.Context, n, def int) (int, error) {
	for {
		fmt.Fprintf(w.out, "Choice [%d]: ", def)
		answer, err := w.readLine(ctx)
		if err != nil {
			return 0, err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return def, nil
		}
		if choice, err := strconv.Atoi(answer); err == nil && choice >= 1 && choice <= n {
			return choice, nil
		}
		fmt.Fprintf(w.out, "Enter a number between 1 and %d.\n", n)
	}
}

// readLine reads one line of user input, mapping EOF (Ctrl+D, closed stdin)
// and context cancellation (Ctrl+C) to a cancellation.
func (w *setupWizard) readLine(ctx context.Context) (string, error) {
	line, err := input.ReadLine(ctx, w.in)
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return "", errSetupCancelled
	}
	if err != nil {
		return "", err
	}
	return line, nil
}

// noSetupOfferEnvVars suppress the automatic setup offer for scripted
// environments driving a real terminal (mirrors the tour's NO_TOUR escape).
var noSetupOfferEnvVars = []string{"DOCKER_AGENT_NO_SETUP", "CAGENT_NO_SETUP"}

func setupOfferDisabledByEnv(getenv func(string) string) bool {
	for _, name := range noSetupOfferEnvVars {
		if getenv(name) == "1" {
			return true
		}
	}
	return false
}

// errorIndicatesNoUsableModel reports whether err means "no usable model or
// missing model credentials", the failures the setup wizard fixes. Errors
// that already name their own exact remediation (a failed or declined pull of
// a specific model) are excluded: re-offering a generic wizard on top of them
// would drown the fix they carry.
func errorIndicatesNoUsableModel(err error) bool {
	if _, ok := errors.AsType[*config.AutoModelFallbackError](err); ok {
		return true
	}
	if errors.Is(err, dmr.ErrNotInstalled) {
		return true
	}
	if reqErr, ok := errors.AsType[*environment.RequiredEnvError](err); ok {
		return reqErr.MissingModelCredentials
	}
	// Matches errors that self-classify, e.g. the unexported first_available
	// variant in pkg/config.
	var modelCreds interface{ MissingModelCredentials() bool }
	if errors.As(err, &modelCreds) {
		return modelCreds.MissingModelCredentials()
	}
	return false
}

// shouldOfferSetup reports whether a failed run should offer the setup
// wizard: interactive terminal on both ends, not exec mode, not suppressed
// via environment, and a failure the wizard can actually fix.
func shouldOfferSetup(runErr error, execMode bool, getenv func(string) string) bool {
	if runErr == nil || execMode || setupOfferDisabledByEnv(getenv) {
		return false
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return false
	}
	return errorIndicatesNoUsableModel(runErr)
}

// offerSetupOnNoModel completes an interactive run that failed for lack of a
// usable model: it surfaces the failure, offers the setup wizard (decline-able),
// and hands a successful setup to completeOfferedSetup for the retry. In
// every other case the original error is returned unchanged.
func (f *runExecFlags) offerSetupOnNoModel(ctx context.Context, cmd *cobra.Command, out *cli.Printer, args []string, useTUI bool, runErr error) error {
	if !shouldOfferSetup(runErr, f.exec, os.Getenv) {
		return runErr
	}

	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "%v\n\n", runErr)
	fmt.Fprint(errOut, "Run the interactive setup now to configure a model? ([y]es/[n]o): ")

	answer, err := input.ReadLine(ctx, cmd.InOrStdin())
	if err != nil {
		fmt.Fprintln(errOut)
		return errNoUsableModel
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return errNoUsableModel
	}

	fmt.Fprintln(cmd.OutOrStdout())
	wizard := newTerminalSetupWizard(cmd.InOrStdin(), cmd.OutOrStdout())
	result, err := wizard.run(ctx)
	if errors.Is(err, errSetupCancelled) {
		return errNoUsableModel
	}
	if err != nil {
		return err
	}

	return f.completeOfferedSetup(ctx, result, cmd.OutOrStdout(), func() error {
		return f.runOrExec(ctx, out, args, useTUI)
	})
}

// completeOfferedSetup finishes a setup that was offered after a failed run:
// it bridges what the wizard configured into the retry's environment and run
// config, then retries the original invocation once. The Claude Code harness
// path is the exception: it configured an external CLI agent file the failed
// invocation cannot use, so nothing is retried and the sentinel keeps the
// exit status non-zero.
func (f *runExecFlags) completeOfferedSetup(ctx context.Context, result *setupResult, out io.Writer, retry func() error) error {
	if result.ClaudeHarness {
		return errClaudeHarnessConfigured
	}

	// The run's env provider chain was built before the wizard stored the key,
	// so bridge it into the process environment for the retry: the config env
	// file is not live when it did not exist at chain construction.
	if result.EnvVar != "" {
		if err := os.Setenv(result.EnvVar, result.Value); err != nil {
			slog.WarnContext(ctx, "Failed to export the stored key for the retry", "env_var", result.EnvVar, "error", err)
		}
	}

	// A provider registered by the wizard was saved after the run config was
	// seeded from the user config, so bridge it in for the retry too. Auto
	// model selection never picks a custom provider, so pin the model chosen
	// in the wizard unless a default model is already configured.
	if result.ProviderName != "" && result.Provider != nil {
		if f.runConfig.Providers == nil {
			f.runConfig.Providers = map[string]latest.ProviderConfig{}
		}
		f.runConfig.Providers[result.ProviderName] = *result.Provider
		if result.Model != "" && f.runConfig.DefaultModel == nil {
			f.runConfig.DefaultModel = parseModelShorthand(result.Model)
		}
	}

	fmt.Fprintln(out, "Retrying with the new configuration...")
	return retry()
}
