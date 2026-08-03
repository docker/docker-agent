package root

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/cli/cli-plugins/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/codingharness"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
)

func TestSetupWizard_CommandsMatchInvocation(t *testing.T) {
	tests := []struct {
		name        string
		originalCLI string
		command     string
		unexpected  string
	}{
		{name: "standalone", command: "docker-agent", unexpected: "docker agent run"},
		{name: "plugin", originalCLI: "docker", command: "docker agent", unexpected: "docker-agent run"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(metadata.ReexecEnvvar, test.originalCLI)

			var out bytes.Buffer
			wizard := &setupWizard{out: &out}
			wizard.printNextSteps(&setupResult{ClaudeHarness: true, AgentFile: "claude-code-agent.yaml"})

			assert.Contains(t, out.String(), test.command+" run claude-code-agent.yaml")
			assert.Contains(t, out.String(), test.command+" doctor claude-code-agent.yaml")
			assert.NotContains(t, out.String(), test.unexpected)
			assert.Contains(t, errClaudeHarnessConfigured.Error(), test.command+" run <file>")
		})
	}
}

// fakeSecretStore records stored secrets in memory and can be made to fail.
type fakeSecretStore struct {
	name   string
	stored map[string]string
	err    error
}

func (s *fakeSecretStore) Name() string        { return s.name }
func (s *fakeSecretStore) Description() string { return s.name + " (fake)" }

func (s *fakeSecretStore) Store(_ context.Context, name, value string) error {
	if s.err != nil {
		return s.err
	}
	if s.stored == nil {
		s.stored = map[string]string{}
	}
	s.stored[name] = value
	return nil
}

// newTestWizard builds a wizard fed by scripted answers, returning the output
// buffer. Secrets are answered by keys (one per prompt); DMR state comes from
// dmrModels/dmrErr. The custom-provider seams default to "no models listed"
// and an in-memory provider store; tests override the fields as needed.
func newTestWizard(answers string, keys []string, stores []environment.SecretStore, dmrModels []string, dmrErr error) (*setupWizard, *bytes.Buffer, *[]string) {
	var out bytes.Buffer
	var pulled []string
	secretCalls := 0

	wizard := &setupWizard{
		in:  bufio.NewReader(strings.NewReader(answers)),
		out: &out,
		readSecret: func(prompt string) (string, error) {
			// Echo the prompt like the terminal readSecret so tests can assert on it.
			fmt.Fprintln(&out, prompt)
			if secretCalls >= len(keys) {
				return "", errors.New("no scripted key left")
			}
			key := keys[secretCalls]
			secretCalls++
			return key, nil
		},
		stores:    stores,
		dmrLister: func(context.Context) ([]string, error) { return dmrModels, dmrErr },
		pullModel: func(_ context.Context, model string) error {
			pulled = append(pulled, model)
			return nil
		},
		saveProvider: func(string, latest.ProviderConfig) error { return nil },
		listModels:   func(context.Context, string, string) []string { return nil },
		probeClaudeCLI: func(context.Context) codingharness.ClaudeCLIStatus {
			return codingharness.ClaudeCLIStatus{State: codingharness.ClaudeStateNotInstalled, Detail: "not scripted"}
		},
		claudeLogin: func(context.Context) error { return errors.New("claude login is not scripted") },
	}
	return wizard, &out, &pulled
}

func TestSetupWizard_CloudPathStoresKey(t *testing.T) {
	t.Parallel()

	store := &fakeSecretStore{name: "config-env-file"}
	// cloud -> provider 1 (anthropic); a single store is used without a prompt
	wizard, out, _ := newTestWizard("1\n1\n", []string{"sk-cloud-key"}, []environment.SecretStore{store}, nil, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "sk-cloud-key", store.stored["ANTHROPIC_API_KEY"])
	assert.Equal(t, "ANTHROPIC_API_KEY", result.EnvVar)
	assert.Equal(t, "sk-cloud-key", result.Value)
	assert.Equal(t, "anthropic/claude-sonnet-4-6", result.Model)

	output := out.String()
	assert.NotContains(t, output, "Where should", "a single store must not prompt for a location")
	assert.Contains(t, output, "anthropic credential", "the built-in prompt asks for a credential")
	assert.NotContains(t, output, "anthropic API key", "not every built-in credential is an API key (docker/docker-agent#3805)")
	assert.Contains(t, output, "Stored ANTHROPIC_API_KEY in the config-env-file (fake).")
	assert.Contains(t, output, invocation.DockerAgent()+" run")
	assert.Contains(t, output, "--model anthropic/claude-sonnet-4-6")
	assert.Contains(t, output, invocation.DockerAgent()+" doctor")
	assert.NotContains(t, output, "sk-cloud-key", "secret values must never be printed")
}

func TestSetupWizard_DefaultsSelectCloudAndFirstEntries(t *testing.T) {
	t.Parallel()

	store := &fakeSecretStore{name: "config-env-file"}
	// Empty answers take every default: cloud, first provider.
	wizard, _, _ := newTestWizard("\n\n", []string{"sk-key"}, []environment.SecretStore{store}, nil, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	first := config.CloudProviderEnvVars()[0]
	assert.Equal(t, first.EnvVars[0], result.EnvVar)
	assert.Equal(t, "sk-key", store.stored[first.EnvVars[0]])
}

func TestSetupWizard_CloudPathRetriesFailedStore(t *testing.T) {
	t.Parallel()

	broken := &fakeSecretStore{name: "broken-store", err: errors.New("store is unavailable")}
	working := &fakeSecretStore{name: "config-env-file"}
	// cloud -> provider 1 -> store 1 (fails) -> store 2 (succeeds)
	wizard, out, _ := newTestWizard("1\n1\n1\n2\n", []string{"sk-key"},
		[]environment.SecretStore{broken, working}, nil, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Could not store the key: store is unavailable")
	assert.Equal(t, "sk-key", working.stored[result.EnvVar])
}

func TestSetupWizard_CloudPathSingleStoreFailureIsFatal(t *testing.T) {
	t.Parallel()

	broken := &fakeSecretStore{name: "config-env-file", err: errors.New("disk full")}
	wizard, _, _ := newTestWizard("1\n1\n", []string{"sk-key"}, []environment.SecretStore{broken}, nil, nil)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storing the key")
	assert.Contains(t, err.Error(), "disk full")
}

func TestSetupWizard_CloudPathNoStoresIsFatal(t *testing.T) {
	t.Parallel()

	wizard, _, _ := newTestWizard("1\n1\n", []string{"sk-key"}, nil, nil, nil)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no secret store is available")
}

func TestSetupWizard_CloudPathReasksOnEmptyKey(t *testing.T) {
	t.Parallel()

	store := &fakeSecretStore{name: "config-env-file"}
	wizard, out, _ := newTestWizard("1\n1\n", []string{"  ", "sk-key"}, []environment.SecretStore{store}, nil, nil)

	_, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Contains(t, out.String(), "The key is empty")
	assert.Equal(t, "sk-key", store.stored["ANTHROPIC_API_KEY"])
}

func TestSetupWizard_ChatGPTPathRunsBrowserSignIn(t *testing.T) {
	t.Parallel()

	providers := config.CloudProviderEnvVars()
	idx := slices.IndexFunc(providers, func(p config.ProviderEnvVars) bool { return p.Provider == "chatgpt" })
	require.GreaterOrEqual(t, idx, 0, "chatgpt must be offered by the wizard")

	store := &fakeSecretStore{name: "config-env-file"}
	// cloud -> chatgpt: no key prompt, no store involved.
	wizard, out, _ := newTestWizard(fmt.Sprintf("1\n%d\n", idx+1), nil, []environment.SecretStore{store}, nil, nil)
	loginCalled := false
	wizard.chatgptLogin = func(_ context.Context, _ io.Writer) (*chatgpt.LoginResult, error) {
		loginCalled = true
		return &chatgpt.LoginResult{Email: "user@example.com", Plan: "plus"}, nil
	}

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.True(t, loginCalled)
	assert.Empty(t, result.EnvVar, "the sign-in stores the credential itself; nothing to export")
	assert.Equal(t, "chatgpt/"+config.DefaultModels["chatgpt"], result.Model)
	assert.Empty(t, store.stored, "no secret store is involved")

	output := out.String()
	assert.Contains(t, output, "ChatGPT account sign-in")
	assert.Contains(t, output, "Signed in as user@example.com.")
	assert.Contains(t, output, "--model chatgpt/"+config.DefaultModels["chatgpt"])
}

func TestSetupWizard_CloudPathOffersGitHubCopilot(t *testing.T) {
	t.Parallel()

	providers := config.CloudProviderEnvVars()
	idx := slices.IndexFunc(providers, func(p config.ProviderEnvVars) bool { return p.Provider == "github-copilot" })
	require.GreaterOrEqual(t, idx, 0, "github-copilot must be offered by the wizard")

	store := &fakeSecretStore{name: "config-env-file"}
	wizard, out, _ := newTestWizard(fmt.Sprintf("1\n%d\n", idx+1), []string{"ghp-token"}, []environment.SecretStore{store}, nil, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "GITHUB_TOKEN", result.EnvVar)
	assert.Equal(t, "ghp-token", store.stored["GITHUB_TOKEN"])
	assert.Equal(t, "github-copilot/"+config.DefaultModels["github-copilot"], result.Model)
	assert.Contains(t, out.String(), "github-copilot")
	assert.Contains(t, out.String(), "GITHUB_TOKEN")
}

func TestSetupWizard_LocalPathWithPulledModels(t *testing.T) {
	t.Parallel()

	wizard, out, pulled := newTestWizard("2\n", nil, nil, []string{"ai/smollm2"}, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Empty(t, *pulled, "no pull needed when a model is already available")
	assert.Equal(t, "dmr/ai/smollm2", result.Model)
	assert.Contains(t, out.String(), "- ai/smollm2")
	assert.Contains(t, out.String(), invocation.DockerAgent()+" run")
}

func TestSetupWizard_LocalPathPullsDefaultModel(t *testing.T) {
	t.Parallel()

	// local -> accept the default model
	wizard, out, pulled := newTestWizard("2\n\n", nil, nil, nil, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	require.Equal(t, []string{"ai/qwen3:latest"}, *pulled)
	assert.Equal(t, "dmr/ai/qwen3:latest", result.Model)
	assert.Contains(t, out.String(), "no model is pulled yet")
}

func TestSetupWizard_LocalPathPullsCustomModel(t *testing.T) {
	t.Parallel()

	wizard, _, pulled := newTestWizard("2\nai/llama3.2\n", nil, nil, nil, nil)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	require.Equal(t, []string{"ai/llama3.2"}, *pulled)
	assert.Equal(t, "dmr/ai/llama3.2", result.Model)
}

func TestSetupWizard_LocalPathDMRNotInstalled(t *testing.T) {
	t.Parallel()

	wizard, _, _ := newTestWizard("2\n", nil, nil, nil, dmr.ErrNotInstalled)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not installed")
	assert.Contains(t, err.Error(), dmrDocsURL)
}

func TestSetupWizard_LocalPathDMRUnreachable(t *testing.T) {
	t.Parallel()

	wizard, _, _ := newTestWizard("2\n", nil, nil, nil, errors.New("connection refused"))

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reachable")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestSetupWizard_InvalidChoiceReasks(t *testing.T) {
	t.Parallel()

	wizard, out, pulled := newTestWizard("9\nx\n2\n", nil, nil, []string{"ai/smollm2"}, nil)

	_, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Enter a number between 1 and 4.")
	assert.Empty(t, *pulled)
}

// The top-level menu must make the built-in vs custom distinction obvious:
// Groq and Hugging Face are built into option 1, and option 3 is only for a
// custom endpoint reached through its base URL (docker/docker-agent#3805).
func TestSetupWizard_MenuDistinguishesBuiltInFromCustomEndpoint(t *testing.T) {
	t.Parallel()

	wizard, out, _ := newTestWizard("2\n", nil, nil, []string{"ai/smollm2"}, nil)

	_, err := wizard.run(t.Context())
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "1. Built-in cloud provider (Anthropic, OpenAI, Groq, Hugging Face, ...)")
	assert.Contains(t, output, "3. Custom OpenAI-compatible endpoint (your own base URL, e.g. vLLM, LiteLLM)")
	assert.NotContains(t, output, "needs an API key", "not every built-in provider uses a plain API key")

	// The menu names Groq and Hugging Face as built-in; keep that honest.
	providers := config.CloudProviderEnvVars()
	for _, name := range []string{"groq", "huggingface"} {
		contains := slices.ContainsFunc(providers, func(p config.ProviderEnvVars) bool { return p.Provider == name })
		assert.True(t, contains, "%s must be a built-in cloud provider", name)
	}
}

func TestSetupWizard_EOFCancels(t *testing.T) {
	t.Parallel()

	wizard, _, _ := newTestWizard("", nil, nil, nil, nil)

	_, err := wizard.run(t.Context())
	require.ErrorIs(t, err, errSetupCancelled)
}

// customProviderRecorder overrides the wizard's saveProvider seam and records
// what the custom path persisted.
type customProviderRecorder struct {
	name     string
	provider latest.ProviderConfig
	err      error
}

func (r *customProviderRecorder) save(name string, provider latest.ProviderConfig) error {
	if r.err != nil {
		return r.err
	}
	r.name = name
	r.provider = provider
	return nil
}

func TestSetupWizard_CustomPathSavesProviderAndKey(t *testing.T) {
	t.Parallel()

	store := &fakeSecretStore{name: "config-env-file"}
	recorder := &customProviderRecorder{}
	// custom -> name -> base URL -> format 2 (responses) -> env var -> model (default)
	wizard, out, _ := newTestWizard("3\nmyprovider\nhttps://llm.corp.example.com/v1\n2\nMYPROVIDER_API_KEY\n\n",
		[]string{"sk-custom-key"}, []environment.SecretStore{store}, nil, nil)
	wizard.saveProvider = recorder.save
	wizard.listModels = func(_ context.Context, baseURL, token string) []string {
		assert.Equal(t, "https://llm.corp.example.com/v1", baseURL)
		assert.Equal(t, "sk-custom-key", token)
		return []string{"corp-model-a", "corp-model-b"}
	}

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "myprovider", recorder.name)
	assert.Equal(t, latest.ProviderConfig{
		BaseURL:  "https://llm.corp.example.com/v1",
		APIType:  "openai_responses",
		TokenKey: "MYPROVIDER_API_KEY",
	}, recorder.provider)

	assert.Equal(t, "sk-custom-key", store.stored["MYPROVIDER_API_KEY"])
	assert.Equal(t, "MYPROVIDER_API_KEY", result.EnvVar)
	assert.Equal(t, "myprovider", result.ProviderName)
	require.NotNil(t, result.Provider)
	assert.Equal(t, "myprovider/corp-model-a", result.Model, "empty answer picks the first discovered model")

	output := out.String()
	assert.Contains(t, output, "corp-model-a")
	assert.Contains(t, output, invocation.DockerAgent()+" run --model myprovider/corp-model-a")
	assert.NotContains(t, output, "sk-custom-key", "secret values must never be printed")
}

func TestSetupWizard_CustomPathWithoutKeyOrModels(t *testing.T) {
	t.Parallel()

	recorder := &customProviderRecorder{}
	// custom -> name -> base URL -> format default -> no env var -> no model
	wizard, out, _ := newTestWizard("3\nlocal-vllm\nhttp://localhost:8000/v1\n\n\n\n", nil, nil, nil, nil)
	wizard.saveProvider = recorder.save

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "local-vllm", recorder.name)
	assert.Equal(t, latest.ProviderConfig{
		BaseURL: "http://localhost:8000/v1",
		APIType: "openai_chatcompletions",
	}, recorder.provider)

	assert.Empty(t, result.EnvVar)
	assert.Empty(t, result.Model)
	assert.Equal(t, "local-vllm", result.ProviderName)

	output := out.String()
	assert.Contains(t, output, "Could not list models from the endpoint")
	assert.Contains(t, output, invocation.DockerAgent()+" models --provider local-vllm")
	assert.Contains(t, output, invocation.DockerAgent()+" run --model local-vllm/<model>")
}

func TestSetupWizard_CustomPathValidatesNameAndURL(t *testing.T) {
	t.Parallel()

	recorder := &customProviderRecorder{}
	// Rejected names: empty, slash, built-in (openai), reserved (auto); then a
	// bad URL before a valid one.
	wizard, out, _ := newTestWizard("3\n\nbad/name\nopenai\nauto\nmyprovider\nnot-a-url\nhttps://ok.example.com/v1\n1\n\ncorp-model\n", nil, nil, nil, nil)
	wizard.saveProvider = recorder.save

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "The name is empty")
	assert.Contains(t, output, "cannot contain '/'")
	assert.Contains(t, output, `"openai" is a built-in provider name`)
	assert.Contains(t, output, `"auto" is a built-in provider name`)
	assert.Contains(t, output, "Enter an absolute http(s) URL")

	assert.Equal(t, "myprovider", recorder.name)
	assert.Equal(t, "https://ok.example.com/v1", recorder.provider.BaseURL)
	assert.Equal(t, "myprovider/corp-model", result.Model)
}

func TestSetupWizard_CustomPathReasksOnInvalidEnvVarName(t *testing.T) {
	t.Parallel()

	store := &fakeSecretStore{name: "config-env-file"}
	recorder := &customProviderRecorder{}
	wizard, out, _ := newTestWizard("3\nmyprovider\nhttps://ok.example.com/v1\n1\nMY BAD VAR\nMY_KEY\nm1\n",
		[]string{"sk-key"}, []environment.SecretStore{store}, nil, nil)
	wizard.saveProvider = recorder.save

	_, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Enter a valid environment variable name")
	assert.Equal(t, "MY_KEY", recorder.provider.TokenKey)
	assert.Equal(t, "sk-key", store.stored["MY_KEY"])
}

func TestSetupWizard_CustomPathSurfacesSaveFailure(t *testing.T) {
	t.Parallel()

	recorder := &customProviderRecorder{err: errors.New("disk full")}
	wizard, _, _ := newTestWizard("3\nmyprovider\nhttps://ok.example.com/v1\n1\n\nm1\n", nil, nil, nil, nil)
	wizard.saveProvider = recorder.save

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `saving provider "myprovider" to the user config`)
	assert.Contains(t, err.Error(), "disk full")
}

// claudeProbeScript scripts successive probe results for the Claude harness
// path and records how often the probe and the login ran.
type claudeProbeScript struct {
	statuses   []codingharness.ClaudeCLIStatus
	probeCalls int
	loginCalls int
	loginErr   error
}

func (s *claudeProbeScript) probe(context.Context) codingharness.ClaudeCLIStatus {
	status := s.statuses[min(s.probeCalls, len(s.statuses)-1)]
	s.probeCalls++
	return status
}

func (s *claudeProbeScript) login(context.Context) error {
	s.loginCalls++
	return s.loginErr
}

var claudeAuthenticatedStatus = codingharness.ClaudeCLIStatus{
	State:            codingharness.ClaudeStateAuthenticated,
	Version:          "2.1.210 (Claude Code)",
	AuthMethod:       "claude.ai",
	APIProvider:      "firstParty",
	SubscriptionType: "pro",
}

// newClaudeHarnessWizard wires a wizard for the Claude harness path: scripted
// probe results and a temp dir for the generated agent file.
func newClaudeHarnessWizard(t *testing.T, answers string, script *claudeProbeScript) (*setupWizard, *bytes.Buffer, string) {
	t.Helper()

	wizard, out, _ := newTestWizard(answers, nil, nil, nil, nil)
	wizard.probeClaudeCLI = script.probe
	wizard.claudeLogin = script.login
	dir := t.TempDir()
	wizard.agentFileDir = dir
	return wizard, out, dir
}

func TestSetupWizard_ClaudeHarnessAlreadyLoggedIn(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{claudeAuthenticatedStatus}}
	// harness -> no model override -> default effort
	wizard, out, dir := newClaudeHarnessWizard(t, "4\n\n\n", script)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Zero(t, script.loginCalls, "an authenticated CLI must not trigger a login")
	assert.True(t, result.ClaudeHarness)
	assert.Equal(t, filepath.Join(dir, "claude-code-agent.yaml"), result.AgentFile)

	content, readErr := os.ReadFile(result.AgentFile)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "type: claude-code")
	assert.Contains(t, string(content), "effort: medium")
	assert.NotContains(t, string(content), "model:", "an empty override keeps the Claude Code default")

	output := out.String()
	assert.Contains(t, output, "Claude Code 2.1.210 is installed and logged in (auth: claude.ai, api: firstParty, subscription: pro).")
	assert.NotContains(t, output, "(Claude Code)", "the version must not repeat the product name")
	assert.Contains(t, output, "--dangerously-skip-permissions")
	assert.Contains(t, output, "--worktree")
	assert.Contains(t, output, "`--sandbox` does not carry the `claude` CLI or its login", "sandbox mode must not be presented as harness isolation")
	assert.Contains(t, output, invocation.DockerAgent()+" run "+result.AgentFile)
	assert.Contains(t, output, invocation.DockerAgent()+" doctor "+result.AgentFile)
	assert.NotContains(t, output, "--model", "the harness model is not a docker agent provider model")
}

func TestSetupWizard_ClaudeHarnessNotInstalled(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{{
		State:  codingharness.ClaudeStateNotInstalled,
		Detail: "the `claude` CLI was not found in PATH",
	}}}
	wizard, _, _ := newClaudeHarnessWizard(t, "4\n", script)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in PATH")
	assert.Contains(t, err.Error(), codingharness.ClaudeInstallDocsURL)
	assert.Contains(t, err.Error(), "claude auth login --claudeai")
	assert.Zero(t, script.loginCalls)
}

func TestSetupWizard_ClaudeHarnessLoginDeclined(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{{
		State:   codingharness.ClaudeStateUnauthenticated,
		Version: "2.1.210 (Claude Code)",
	}}}
	// harness -> decline the login offer
	wizard, out, _ := newClaudeHarnessWizard(t, "4\nn\n", script)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not logged in")
	assert.Contains(t, err.Error(), "claude auth login --claudeai")
	assert.Zero(t, script.loginCalls, "declining must not run the login")

	output := out.String()
	assert.Contains(t, output, "Claude Code 2.1.210 is installed but not logged in.")
	assert.Contains(t, output, "same OS user and environment")
}

// An empty answer to the login offer must not run the login either: only an
// explicit yes does.
func TestSetupWizard_ClaudeHarnessLoginNeedsExplicitYes(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{{
		State: codingharness.ClaudeStateUnauthenticated,
	}}}
	wizard, _, _ := newClaudeHarnessWizard(t, "4\n\n", script)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Zero(t, script.loginCalls)
}

func TestSetupWizard_ClaudeHarnessLoginRunsAfterConfirmation(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{
		{State: codingharness.ClaudeStateUnauthenticated, Version: "2.1.210 (Claude Code)"},
		claudeAuthenticatedStatus,
	}}
	// harness -> confirm login -> model override -> xhigh effort
	wizard, out, _ := newClaudeHarnessWizard(t, "4\ny\nsonnet\nxhigh\n", script)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, script.loginCalls)
	assert.Equal(t, 2, script.probeCalls, "the state must be re-probed after the login")

	content, readErr := os.ReadFile(result.AgentFile)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "model: sonnet")
	assert.Contains(t, string(content), "effort: xhigh")

	assert.Contains(t, out.String(), "Logged in (auth: claude.ai, api: firstParty, subscription: pro)")
}

func TestSetupWizard_ClaudeHarnessStillUnauthenticatedAfterLogin(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{{
		State: codingharness.ClaudeStateUnauthenticated,
	}}}
	wizard, _, _ := newClaudeHarnessWizard(t, "4\ny\n", script)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, script.loginCalls)
	assert.Contains(t, err.Error(), "still reports it is not logged in")
	assert.Contains(t, err.Error(), "claude auth status")
}

func TestSetupWizard_ClaudeHarnessLoginFailure(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{
		statuses: []codingharness.ClaudeCLIStatus{{State: codingharness.ClaudeStateAuthCheckFailed, Detail: "`claude auth status` failed: exit status 1"}},
		loginErr: errors.New("exit status 130"),
	}
	wizard, out, _ := newClaudeHarnessWizard(t, "4\ny\n", script)

	_, err := wizard.run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "`claude auth login --claudeai` failed")
	assert.Contains(t, err.Error(), "exit status 130")
	assert.Contains(t, out.String(), "login check failed: `claude auth status` failed: exit status 1")
}

func TestSetupWizard_ClaudeHarnessInvalidEffortReasks(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{claudeAuthenticatedStatus}}
	// harness -> no model -> invalid effort -> xhigh
	wizard, out, _ := newClaudeHarnessWizard(t, "4\n\nturbo\nxhigh\n", script)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Enter one of low, medium, high, xhigh, max")
	content, readErr := os.ReadFile(result.AgentFile)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "effort: xhigh")
}

func TestSetupWizard_ClaudeHarnessGeneratedFileIsValidConfig(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{claudeAuthenticatedStatus}}
	wizard, _, _ := newClaudeHarnessWizard(t, "4\nclaude-sonnet-4-5\nmax\n", script)

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	cfg, err := config.Load(t.Context(), config.NewFileSource(result.AgentFile))
	require.NoError(t, err, "the generated file must load and validate")
	require.Len(t, cfg.Agents, 1)
	agent := cfg.Agents[0]
	require.NotNil(t, agent.Harness)
	assert.Equal(t, "claude-code", agent.Harness.Type)
	assert.Equal(t, "claude-sonnet-4-5", agent.Harness.Model)
	assert.Equal(t, "max", agent.Harness.Effort)
}

func TestSetupWizard_ClaudeHarnessDoesNotOverwriteWithoutConfirmation(t *testing.T) {
	t.Parallel()

	// Only an explicit yes overwrites: "n" and an empty answer both decline.
	tests := []struct {
		name   string
		answer string
	}{
		{name: "explicit no", answer: "n"},
		{name: "empty answer", answer: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{claudeAuthenticatedStatus}}
			// harness -> no model -> default effort -> decline the overwrite
			wizard, out, dir := newClaudeHarnessWizard(t, "4\n\n\n"+test.answer+"\n", script)
			existing := filepath.Join(dir, "claude-code-agent.yaml")
			require.NoError(t, os.WriteFile(existing, []byte("# precious\n"), 0o600))

			result, err := wizard.run(t.Context())
			require.NoError(t, err)

			assert.True(t, result.ClaudeHarness)
			assert.Empty(t, result.AgentFile)

			content, readErr := os.ReadFile(existing)
			require.NoError(t, readErr)
			assert.Equal(t, "# precious\n", string(content), "the existing file must be left untouched")

			// The declined path prints the complete copyable config and the command.
			output := out.String()
			assert.Contains(t, output, "already exists")
			assert.Contains(t, output, "type: claude-code")
			assert.Contains(t, output, "effort: medium")
			assert.Contains(t, output, invocation.DockerAgent()+" run")
		})
	}
}

func TestSetupWizard_ClaudeHarnessOverwriteAfterConfirmation(t *testing.T) {
	t.Parallel()

	script := &claudeProbeScript{statuses: []codingharness.ClaudeCLIStatus{claudeAuthenticatedStatus}}
	// harness -> no model -> default effort -> confirm the overwrite
	wizard, _, dir := newClaudeHarnessWizard(t, "4\n\n\ny\n", script)
	existing := filepath.Join(dir, "claude-code-agent.yaml")
	require.NoError(t, os.WriteFile(existing, []byte("# old\n"), 0o600))

	result, err := wizard.run(t.Context())
	require.NoError(t, err)

	assert.Equal(t, existing, result.AgentFile)
	content, readErr := os.ReadFile(existing)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "type: claude-code")
}

// The Claude Code harness path configures an external CLI agent file the
// failed invocation cannot use: completing the offer must not retry the old
// configuration and must not exit zero, because the original run never
// happened.
func TestCompleteOfferedSetup_ClaudeHarnessFailsWithoutRetry(t *testing.T) {
	t.Parallel()

	for _, result := range []*setupResult{
		{ClaudeHarness: true, AgentFile: "claude-code-agent.yaml"},
		{ClaudeHarness: true}, // declined overwrite: the config was printed, not written
	} {
		f := &runExecFlags{}
		var out bytes.Buffer
		retried := false

		err := f.completeOfferedSetup(t.Context(), result, &out, func() error {
			retried = true
			return nil
		})

		require.ErrorIs(t, err, errClaudeHarnessConfigured)
		assert.False(t, retried, "the old configuration must not be retried")
		assert.Contains(t, err.Error(), invocation.DockerAgent()+" run")
		assert.Empty(t, out.String(), "no retry banner when nothing is retried")
	}
}

// The cloud, local, and custom paths still bridge the wizard's outcome into
// the failed run and retry it exactly once.
func TestCompleteOfferedSetup_ModelPathsRetry(t *testing.T) {
	// Not parallel: the cloud case exports the stored key into the process
	// environment (restored via t.Setenv).
	tests := []struct {
		name   string
		result *setupResult
		check  func(t *testing.T, f *runExecFlags)
	}{
		{
			name:   "cloud key is exported for the retry",
			result: &setupResult{EnvVar: "SETUP_RETRY_TEST_KEY", Value: "sk-retry", Model: "anthropic/claude-sonnet-4-6"},
			check: func(t *testing.T, _ *runExecFlags) {
				t.Helper()
				assert.Equal(t, "sk-retry", os.Getenv("SETUP_RETRY_TEST_KEY"))
			},
		},
		{
			name:   "local model needs no bridging",
			result: &setupResult{Model: "dmr/ai/qwen3:latest"},
			check:  func(*testing.T, *runExecFlags) {},
		},
		{
			name: "custom provider is bridged into the run config",
			result: &setupResult{
				ProviderName: "myprovider",
				Provider:     &latest.ProviderConfig{BaseURL: "https://llm.example.com/v1", APIType: "openai_chatcompletions"},
				Model:        "myprovider/corp-model",
			},
			check: func(t *testing.T, f *runExecFlags) {
				t.Helper()
				assert.Contains(t, f.runConfig.Providers, "myprovider")
				require.NotNil(t, f.runConfig.DefaultModel)
				assert.Equal(t, "myprovider", f.runConfig.DefaultModel.Provider)
				assert.Equal(t, "corp-model", f.runConfig.DefaultModel.Model)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.result.EnvVar != "" {
				t.Setenv(test.result.EnvVar, "") // restore the variable after the test
			}

			f := &runExecFlags{}
			var out bytes.Buffer
			retries := 0

			err := f.completeOfferedSetup(t.Context(), test.result, &out, func() error {
				retries++
				return nil
			})

			require.NoError(t, err)
			assert.Equal(t, 1, retries)
			assert.Contains(t, out.String(), "Retrying with the new configuration...")
			test.check(t, f)
		})
	}
}

func TestErrorIndicatesNoUsableModel(t *testing.T) {
	t.Parallel()

	assert.True(t, errorIndicatesNoUsableModel(&config.AutoModelFallbackError{}))
	assert.True(t, errorIndicatesNoUsableModel(fmt.Errorf("loading team: %w", dmr.ErrNotInstalled)))
	assert.True(t, errorIndicatesNoUsableModel(&environment.RequiredEnvError{
		Missing: []string{"OPENAI_API_KEY"}, MissingModelCredentials: true,
	}))

	// Missing tool secrets are not fixable by the model setup wizard.
	assert.False(t, errorIndicatesNoUsableModel(&environment.RequiredEnvError{
		Missing: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
	}))
	assert.False(t, errorIndicatesNoUsableModel(errors.New("boom")))
	assert.False(t, errorIndicatesNoUsableModel(nil))
	// Pull errors already carry their own exact remediation.
	assert.False(t, errorIndicatesNoUsableModel(&dmr.PullFailedError{Model: "ai/qwen3"}))
}

func TestErrorIndicatesNoUsableModel_FirstAvailableVariant(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {FirstAvailable: []string{"openai/gpt-5"}},
		},
	}
	err := config.ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider())
	require.Error(t, err)

	assert.True(t, errorIndicatesNoUsableModel(err))
}

func TestSetupOfferDisabledByEnv(t *testing.T) {
	t.Parallel()

	assert.False(t, setupOfferDisabledByEnv(func(string) string { return "" }))
	for _, name := range []string{"DOCKER_AGENT_NO_SETUP", "CAGENT_NO_SETUP"} {
		env := func(key string) string {
			if key == name {
				return "1"
			}
			return ""
		}
		assert.True(t, setupOfferDisabledByEnv(env), name)
	}
}

func TestShouldOfferSetup(t *testing.T) {
	t.Parallel()

	noEnv := func(string) string { return "" }
	modelErr := &config.AutoModelFallbackError{}

	assert.False(t, shouldOfferSetup(nil, false, noEnv))
	assert.False(t, shouldOfferSetup(modelErr, true, noEnv), "exec mode never prompts")
	assert.False(t, shouldOfferSetup(modelErr, false, func(string) string { return "1" }))
	// The remaining conditions require a real terminal on stdin and stdout,
	// which a test process does not have: the guard must say no here.
	assert.False(t, shouldOfferSetup(modelErr, false, noEnv))
}

func TestSetupCommand_RequiresTerminal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cmd := newSetupCmd()
	cmd.SilenceErrors = true
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a terminal")
	assert.Contains(t, err.Error(), environment.SecretsDocsURL)
}
