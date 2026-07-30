package latest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCompactionThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   *float64
		wantErr string
	}{
		{name: "nil is valid", value: nil},
		{name: "just above zero", value: new(0.0001)},
		{name: "mid range", value: new(0.5)},
		{name: "exactly one", value: new(1.0)},
		{name: "exactly zero", value: new(0.0), wantErr: "compaction_threshold must be greater than 0 and at most 1, got 0"},
		{name: "negative", value: new(-0.5), wantErr: "compaction_threshold must be greater than 0 and at most 1, got -0.5"},
		{name: "just above one", value: new(1.000001), wantErr: "compaction_threshold must be greater than 0 and at most 1"},
		{name: "well above one", value: new(1.5), wantErr: "compaction_threshold must be greater than 0 and at most 1, got 1.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCompactionThreshold(tt.value)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestModelConfigValidateFirstAvailable(t *testing.T) {
	t.Parallel()

	candidates := []string{"openai/gpt-4o"}

	tests := []struct {
		name    string
		model   ModelConfig
		wantErr string
	}{
		{name: "nil selector", model: ModelConfig{Provider: "openai", Model: "gpt-4o"}},
		{name: "single candidate", model: ModelConfig{FirstAvailable: candidates}},
		{name: "multiple candidates", model: ModelConfig{FirstAvailable: []string{"anthropic/claude-sonnet-4-5", "dmr/local"}}},
		{name: "empty selector", model: ModelConfig{FirstAvailable: []string{}}, wantErr: "first_available must contain at least one candidate"},
		{name: "blank candidate", model: ModelConfig{FirstAvailable: []string{"openai/gpt-4o", "  "}}, wantErr: "first_available[1] must not be empty"},
		{name: "with provider", model: ModelConfig{FirstAvailable: candidates, Provider: "openai"}, wantErr: "first_available cannot be combined with provider/model"},
		{name: "with model", model: ModelConfig{FirstAvailable: candidates, Model: "gpt-4o"}, wantErr: "first_available cannot be combined with provider/model"},
		{name: "with temperature", model: ModelConfig{FirstAvailable: candidates, Temperature: new(0.5)}, wantErr: "first_available cannot be combined with temperature"},
		{name: "with max_tokens", model: ModelConfig{FirstAvailable: candidates, MaxTokens: new(int64(100))}, wantErr: "first_available cannot be combined with max_tokens"},
		{name: "with top_p", model: ModelConfig{FirstAvailable: candidates, TopP: new(0.9)}, wantErr: "first_available cannot be combined with top_p"},
		{name: "with frequency_penalty", model: ModelConfig{FirstAvailable: candidates, FrequencyPenalty: new(0.1)}, wantErr: "first_available cannot be combined with frequency_penalty"},
		{name: "with presence_penalty", model: ModelConfig{FirstAvailable: candidates, PresencePenalty: new(0.1)}, wantErr: "first_available cannot be combined with presence_penalty"},
		{name: "with base_url", model: ModelConfig{FirstAvailable: candidates, BaseURL: "https://example.invalid/v1"}, wantErr: "first_available cannot be combined with base_url"},
		{name: "with token_key", model: ModelConfig{FirstAvailable: candidates, TokenKey: "MY_KEY"}, wantErr: "first_available cannot be combined with token_key"},
		{name: "with bypass_models_gateway", model: ModelConfig{FirstAvailable: candidates, BypassModelsGateway: true}, wantErr: "first_available cannot be combined with bypass_models_gateway"},
		{name: "with provider_opts", model: ModelConfig{FirstAvailable: candidates, ProviderOpts: map[string]any{"context_size": 4096}}, wantErr: "first_available cannot be combined with provider_opts"},
		{name: "with track_usage", model: ModelConfig{FirstAvailable: candidates, TrackUsage: new(true)}, wantErr: "first_available cannot be combined with track_usage"},
		{name: "with thinking_budget", model: ModelConfig{FirstAvailable: candidates, ThinkingBudget: &ThinkingBudget{Effort: "high"}}, wantErr: "first_available cannot be combined with thinking_budget"},
		{name: "with task_budget", model: ModelConfig{FirstAvailable: candidates, TaskBudget: &TaskBudget{Type: "tokens", Total: 1000}}, wantErr: "first_available cannot be combined with task_budget"},
		{name: "with auth", model: ModelConfig{FirstAvailable: candidates, Auth: &AuthConfig{Type: AuthTypeWorkloadIdentityFederation}}, wantErr: "first_available cannot be combined with auth"},
		{name: "with routing", model: ModelConfig{FirstAvailable: candidates, Routing: []RoutingRule{{Model: "openai/gpt-4o-mini", Examples: []string{"hi"}}}}, wantErr: "first_available cannot be combined with routing"},
		{name: "with title_model", model: ModelConfig{FirstAvailable: candidates, TitleModel: "small"}, wantErr: "first_available cannot be combined with title_model"},
		{name: "with compaction_model", model: ModelConfig{FirstAvailable: candidates, CompactionModel: "small"}, wantErr: "first_available cannot be combined with compaction_model"},
		{name: "with compaction_threshold", model: ModelConfig{FirstAvailable: candidates, CompactionThreshold: new(0.5)}, wantErr: "first_available cannot be combined with compaction_threshold"},
		{name: "with cost", model: ModelConfig{FirstAvailable: candidates, Cost: &CostConfig{Input: 1}}, wantErr: "first_available cannot be combined with cost"},
		{name: "with output_capabilities", model: ModelConfig{FirstAvailable: candidates, OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}}, wantErr: "first_available cannot be combined with output_capabilities"},
		{name: "with output_capabilities false", model: ModelConfig{FirstAvailable: candidates, OutputCapabilities: &OutputCapabilitiesConfig{Image: new(false)}}, wantErr: "first_available cannot be combined with output_capabilities"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.model.validateFirstAvailable()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAgentConfigValidateFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fallback *FallbackConfig
		wantErr  string
	}{
		{name: "nil fallback", fallback: nil},
		{name: "defaults", fallback: &FallbackConfig{Models: []string{"backup"}}},
		{name: "explicit no retries", fallback: &FallbackConfig{Retries: -1}},
		{name: "positive retries and cooldown", fallback: &FallbackConfig{Retries: 3, Cooldown: Duration{Duration: time.Minute}}},
		{name: "retries below -1", fallback: &FallbackConfig{Retries: -2}, wantErr: "fallback.retries must be >= -1"},
		{name: "negative cooldown", fallback: &FallbackConfig{Cooldown: Duration{Duration: -time.Second}}, wantErr: "fallback.cooldown must be non-negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := AgentConfig{Name: "root", Fallback: tt.fallback}
			err := agent.validateFallback()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAgentConfigValidateHarness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		harness          *HarnessConfig
		compactionModel  string
		structuredOutput *StructuredOutput
		wantErr          string
	}{
		{name: "nil harness", harness: nil},
		{name: "nil harness with compaction model", harness: nil, compactionModel: "fast"},
		{name: "claude-code", harness: &HarnessConfig{Type: "claude-code"}},
		{name: "codex", harness: &HarnessConfig{Type: "codex"}},
		{name: "pi", harness: &HarnessConfig{Type: "pi"}},
		{name: "opencode", harness: &HarnessConfig{Type: "opencode"}},
		{name: "missing type", harness: &HarnessConfig{}, wantErr: "harness.type is required"},
		{name: "unsupported type", harness: &HarnessConfig{Type: "cursor"}, wantErr: `unsupported harness.type "cursor"`},
		{name: "effort low", harness: &HarnessConfig{Type: "claude-code", Effort: "low"}},
		{name: "effort medium", harness: &HarnessConfig{Type: "claude-code", Effort: "medium"}},
		{name: "effort high", harness: &HarnessConfig{Type: "claude-code", Effort: "high"}},
		{name: "effort xhigh", harness: &HarnessConfig{Type: "claude-code", Effort: "xhigh"}},
		{name: "effort max", harness: &HarnessConfig{Type: "claude-code", Effort: "max"}},
		{name: "effort on wrong type", harness: &HarnessConfig{Type: "codex", Effort: "high"}, wantErr: "harness.effort can only be used with harness.type 'claude-code'"},
		{name: "invalid effort", harness: &HarnessConfig{Type: "claude-code", Effort: "extreme"}, wantErr: "harness.effort must be one of: low, medium, high, xhigh, max"},
		{name: "agent on opencode", harness: &HarnessConfig{Type: "opencode", Agent: "build"}},
		{name: "agent on wrong type", harness: &HarnessConfig{Type: "claude-code", Agent: "build"}, wantErr: "harness.agent can only be used with harness.type 'opencode'"},
		{name: "thinking on opencode", harness: &HarnessConfig{Type: "opencode", Thinking: true}},
		{name: "thinking on wrong type", harness: &HarnessConfig{Type: "codex", Thinking: true}, wantErr: "harness.thinking can only be used with harness.type 'opencode'"},
		{name: "compaction model on harness", harness: &HarnessConfig{Type: "claude-code"}, compactionModel: "fast", wantErr: "compaction_model cannot be used with a harness"},
		{name: "nil harness with tool-mode structured output", harness: nil, structuredOutput: &StructuredOutput{Mode: StructuredOutputModeTool}},
		{name: "native structured output on harness", harness: &HarnessConfig{Type: "claude-code"}, structuredOutput: &StructuredOutput{Mode: StructuredOutputModeNative}},
		{name: "default-mode structured output on harness", harness: &HarnessConfig{Type: "claude-code"}, structuredOutput: &StructuredOutput{Name: "out"}},
		{name: "tool-mode structured output on harness", harness: &HarnessConfig{Type: "claude-code"}, structuredOutput: &StructuredOutput{Mode: StructuredOutputModeTool}, wantErr: "structured_output.mode 'tool' cannot be used with a harness"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := AgentConfig{Name: "root", Harness: tt.harness, CompactionModel: tt.compactionModel, StructuredOutput: tt.structuredOutput}
			err := agent.validateHarness()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestToolsetValidateAttributeTypeMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		toolset Toolset
		wantErr string
	}{
		{name: "shell map on non-script", toolset: Toolset{Type: "shell", Shell: map[string]ScriptShellToolConfig{"greet": {}}}, wantErr: "shell can only be used with type 'script'"},
		{name: "path on non-memory/tasks", toolset: Toolset{Type: "shell", Path: "/tmp/db"}, wantErr: "path can only be used with type 'memory' or 'tasks'"},
		{name: "post_edit on non-filesystem", toolset: Toolset{Type: "shell", PostEdit: []PostEditConfig{{}}}, wantErr: "post_edit can only be used with type 'filesystem'"},
		{name: "ignore_vcs on non-filesystem", toolset: Toolset{Type: "shell", IgnoreVCS: new(true)}, wantErr: "ignore_vcs can only be used with type 'filesystem'"},
		{name: "allow_list on non-filesystem", toolset: Toolset{Type: "shell", AllowList: []string{"."}}, wantErr: "allow_list can only be used with type 'filesystem'"},
		{name: "deny_list on non-filesystem", toolset: Toolset{Type: "shell", DenyList: []string{"/etc"}}, wantErr: "deny_list can only be used with type 'filesystem'"},
		{name: "blank allow_list entry", toolset: Toolset{Type: "filesystem", AllowList: []string{"  "}}, wantErr: "allow_list[0] must not be empty"},
		{name: "blank deny_list entry", toolset: Toolset{Type: "filesystem", DenyList: []string{"/etc", ""}}, wantErr: "deny_list[1] must not be empty"},
		{name: "env on wrong type", toolset: Toolset{Type: "filesystem", Env: map[string]string{"A": "b"}}, wantErr: "env can only be used with type 'shell', 'background_jobs', 'script', 'mcp' or 'lsp'"},
		{name: "file_types on non-lsp", toolset: Toolset{Type: "shell", FileTypes: []string{"go"}}, wantErr: "file_types can only be used with type 'lsp'"},
		{name: "allowed_servers on non-mcp_catalog", toolset: Toolset{Type: "shell", AllowedServers: []string{"github"}}, wantErr: "allowed_servers can only be used with type 'mcp_catalog'"},
		{name: "blocked_servers on non-mcp_catalog", toolset: Toolset{Type: "shell", BlockedServers: []string{"github"}}, wantErr: "blocked_servers can only be used with type 'mcp_catalog'"},
		{name: "blank allowed_servers entry", toolset: Toolset{Type: "mcp_catalog", AllowedServers: []string{""}}, wantErr: "allowed_servers[0] must not be empty"},
		{name: "blank blocked_servers entry", toolset: Toolset{Type: "mcp_catalog", BlockedServers: []string{"github", " "}}, wantErr: "blocked_servers[1] must not be empty"},
		{name: "allowed_domains on non-fetch", toolset: Toolset{Type: "shell", AllowedDomains: []string{"example.com"}}, wantErr: "allowed_domains can only be used with type 'fetch'"},
		{name: "blocked_domains on non-fetch", toolset: Toolset{Type: "shell", BlockedDomains: []string{"example.com"}}, wantErr: "blocked_domains can only be used with type 'fetch'"},
		{name: "allow_private_ips on wrong type", toolset: Toolset{Type: "shell", AllowPrivateIPs: new(true)}, wantErr: "allow_private_ips can only be used with type 'fetch', 'api', 'openapi', 'a2a' or remote MCP toolsets"},
		{name: "sudo_askpass on non-shell", toolset: Toolset{Type: "fetch", SudoAskpass: new(true)}, wantErr: "sudo_askpass can only be used with type 'shell'"},
		{name: "recall on non-background_jobs", toolset: Toolset{Type: "shell", Recall: new(true)}, wantErr: "recall can only be used with type 'background_jobs'"},
		{name: "safer on non-shell", toolset: Toolset{Type: "fetch", Safer: new(true)}, wantErr: "safer can only be used with type 'shell'"},
		{name: "allowed and blocked domains", toolset: Toolset{Type: "fetch", AllowedDomains: []string{"a.example.com"}, BlockedDomains: []string{"b.example.com"}}, wantErr: "allowed_domains and blocked_domains are mutually exclusive"},
		{name: "invalid allowed_domains pattern", toolset: Toolset{Type: "fetch", AllowedDomains: []string{"foo.*"}}, wantErr: `allowed_domains[0] "foo.*" is invalid`},
		{name: "invalid blocked_domains pattern", toolset: Toolset{Type: "fetch", BlockedDomains: []string{"10.0.0.0/33"}}, wantErr: `blocked_domains[0] "10.0.0.0/33" is invalid: not a valid CIDR`},
		{name: "models on non-model_picker", toolset: Toolset{Type: "shell", Models: []string{"openai/gpt-4o"}}, wantErr: "models can only be used with type 'model_picker'"},
		{name: "shared on non-todo", toolset: Toolset{Type: "shell", Shared: true}, wantErr: "shared can only be used with type 'todo'"},
		{name: "version on wrong type", toolset: Toolset{Type: "shell", Version: "owner/repo"}, wantErr: "version can only be used with type 'mcp' or 'lsp'"},
		{name: "command on wrong type", toolset: Toolset{Type: "shell", Command: "run"}, wantErr: "command can only be used with type 'mcp' or 'lsp'"},
		{name: "args on wrong type", toolset: Toolset{Type: "shell", Args: []string{"-v"}}, wantErr: "args can only be used with type 'mcp' or 'lsp'"},
		{name: "ref on wrong type", toolset: Toolset{Type: "shell", Ref: "docker/duckduckgo"}, wantErr: "ref can only be used with type 'mcp' or 'rag'"},
		{name: "remote url on non-mcp", toolset: Toolset{Type: "shell", Remote: Remote{URL: "https://mcp.example.com"}}, wantErr: "remote can only be used with type 'mcp'"},
		{name: "remote transport on non-mcp", toolset: Toolset{Type: "a2a", URL: "https://agent.example.com", Remote: Remote{TransportType: "sse"}}, wantErr: "remote can only be used with type 'mcp'"},
		{name: "remote oauth on non-mcp", toolset: Toolset{Type: "shell", Remote: Remote{OAuth: &RemoteOAuthConfig{ClientID: "c"}}}, wantErr: "remote can only be used with type 'mcp'"},
		{name: "remote headers on wrong type", toolset: Toolset{Type: "shell", Remote: Remote{Headers: map[string]string{"A": "b"}}}, wantErr: "remote headers can only be used with type 'mcp' or 'a2a'"},
		{name: "headers on wrong type", toolset: Toolset{Type: "shell", Headers: map[string]string{"A": "b"}}, wantErr: "headers can only be used with type 'openapi', 'a2a' or 'fetch'"},
		{name: "config on non-mcp", toolset: Toolset{Type: "shell", Config: map[string]any{"k": "v"}}, wantErr: "config can only be used with type 'mcp'"},
		{name: "url on wrong type", toolset: Toolset{Type: "shell", URL: "https://example.com"}, wantErr: "url can only be used with type 'a2a', 'openapi' or 'open_url'"},
		{name: "name on wrong type", toolset: Toolset{Type: "shell", Name: "custom"}, wantErr: "name can only be used with type 'mcp', 'a2a', 'rag', or 'open_url'"},
		{name: "rag_config on non-rag", toolset: Toolset{Type: "shell", RAGConfig: &RAGConfig{}}, wantErr: "rag_config can only be used with type 'rag'"},
		{name: "working_dir on wrong type", toolset: Toolset{Type: "shell", WorkingDir: "/tmp"}, wantErr: "working_dir can only be used with type 'mcp' or 'lsp'"},
		{name: "working_dir on remote mcp", toolset: Toolset{Type: "mcp", WorkingDir: "/tmp", Remote: Remote{URL: "https://mcp.example.com"}}, wantErr: "working_dir is not valid for remote MCP toolsets"},
		{name: "lifecycle on wrong type", toolset: Toolset{Type: "shell", Lifecycle: &LifecycleConfig{}}, wantErr: "lifecycle can only be used with type 'mcp' or 'lsp'"},
		{name: "invalid lifecycle", toolset: Toolset{Type: "mcp", Command: "server", Lifecycle: &LifecycleConfig{Profile: "bogus"}}, wantErr: `lifecycle.profile "bogus" is not supported`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, tt.toolset.validate(), tt.wantErr)
		})
	}
}

func TestToolsetValidateTypeRequirements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		toolset Toolset
		wantErr string
	}{
		{name: "mcp without source", toolset: Toolset{Type: "mcp"}, wantErr: "either command, remote or ref must be set"},
		{name: "mcp command and remote", toolset: Toolset{Type: "mcp", Command: "server", Remote: Remote{URL: "https://mcp.example.com"}}, wantErr: "but only one of those"},
		{name: "mcp command and ref", toolset: Toolset{Type: "mcp", Command: "server", Ref: "docker/duckduckgo"}, wantErr: "but only one of those"},
		{name: "mcp remote and ref", toolset: Toolset{Type: "mcp", Remote: Remote{URL: "https://mcp.example.com"}, Ref: "docker/duckduckgo"}, wantErr: "but only one of those"},
		{name: "local mcp with allow_private_ips", toolset: Toolset{Type: "mcp", Command: "server", AllowPrivateIPs: new(true)}, wantErr: "allow_private_ips can only be used with type 'fetch', 'api', 'openapi', 'a2a' or remote MCP toolsets"},
		{name: "a2a without url", toolset: Toolset{Type: "a2a"}, wantErr: "a2a toolset requires a url to be set"},
		{name: "lsp without command", toolset: Toolset{Type: "lsp"}, wantErr: "lsp toolset requires a command to be set"},
		{name: "openapi without url", toolset: Toolset{Type: "openapi"}, wantErr: "openapi toolset requires a url to be set"},
		{name: "open_url without url", toolset: Toolset{Type: "open_url"}, wantErr: "open_url toolset requires a url to be set"},
		{name: "model_picker without models", toolset: Toolset{Type: "model_picker"}, wantErr: "model_picker toolset requires at least one model in the 'models' list"},
		{name: "rag without ref or config", toolset: Toolset{Type: "rag"}, wantErr: "rag toolset requires either ref or rag_config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, tt.toolset.validate(), tt.wantErr)
		})
	}
}

func TestToolsetValidateOAuth(t *testing.T) {
	t.Parallel()

	remote := func(oauth *RemoteOAuthConfig) Toolset {
		return Toolset{Type: "mcp", Remote: Remote{URL: "https://mcp.example.com", OAuth: oauth}}
	}

	tests := []struct {
		name    string
		toolset Toolset
		wantErr string
	}{
		{name: "minimal oauth", toolset: remote(&RemoteOAuthConfig{ClientID: "client"})},
		{name: "oauth without remote url", toolset: Toolset{Type: "mcp", Command: "server", Remote: Remote{OAuth: &RemoteOAuthConfig{ClientID: "client"}}}, wantErr: "oauth requires remote url to be set"},
		{name: "missing clientId falls back to dynamic registration", toolset: remote(&RemoteOAuthConfig{})},
		{name: "clientSecret without clientId", toolset: remote(&RemoteOAuthConfig{ClientSecret: "secret"}), wantErr: "oauth clientSecret requires clientId to be set"},
		{name: "callback settings without clientId", toolset: remote(&RemoteOAuthConfig{CallbackPort: 8765, CallbackRedirectURL: "https://redirect.example.com/cb"})},
		{name: "callback port unset", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackPort: 0})},
		{name: "callback port lower bound", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackPort: 1})},
		{name: "callback port upper bound", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackPort: 65535})},
		{name: "callback port negative", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackPort: -1}), wantErr: "oauth callbackPort must be between 1 and 65535"},
		{name: "callback port too large", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackPort: 65536}), wantErr: "oauth callbackPort must be between 1 and 65535"},
		{name: "https redirect url", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackRedirectURL: "https://redirect.example.com/cb"})},
		{name: "loopback redirect url with placeholder", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackRedirectURL: "http://127.0.0.1:${callbackPort}/callback"})},
		{name: "http redirect to non-loopback", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackRedirectURL: "http://redirect.example.com/cb"}), wantErr: "oauth callbackRedirectURL must use https for non-loopback hosts"},
		{name: "relative redirect url", toolset: remote(&RemoteOAuthConfig{ClientID: "client", CallbackRedirectURL: "/callback"}), wantErr: "oauth callbackRedirectURL must be an absolute URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.toolset.validate()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestToolsetValidateValidToolsets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		toolset Toolset
	}{
		{name: "shell", toolset: Toolset{Type: "shell", Env: map[string]string{"A": "b"}, SudoAskpass: new(true), Safer: new(true)}},
		{name: "background_jobs", toolset: Toolset{Type: "background_jobs", Env: map[string]string{"A": "b"}, Recall: new(true)}},
		{name: "memory with path", toolset: Toolset{Type: "memory", Path: "/tmp/memory.db"}},
		{name: "memory without path", toolset: Toolset{Type: "memory"}},
		{name: "tasks", toolset: Toolset{Type: "tasks", Path: "./tasks.json"}},
		{name: "todo shared", toolset: Toolset{Type: "todo", Shared: true}},
		{name: "script", toolset: Toolset{Type: "script", Shell: map[string]ScriptShellToolConfig{"greet": {}}, Env: map[string]string{"A": "b"}}},
		{name: "filesystem", toolset: Toolset{Type: "filesystem", PostEdit: []PostEditConfig{{}}, IgnoreVCS: new(false), AllowList: []string{".", "~/src"}, DenyList: []string{"/etc"}}},
		{name: "fetch with allowed domains", toolset: Toolset{Type: "fetch", AllowedDomains: []string{"example.com", "*.example.org", ".sub.example.net", "10.0.0.0/8"}, Headers: map[string]string{"Accept": "text/html"}, AllowPrivateIPs: new(true)}},
		{name: "fetch with blocked domains", toolset: Toolset{Type: "fetch", BlockedDomains: []string{"internal.example.com"}}},
		{name: "api with allow_private_ips", toolset: Toolset{Type: "api", AllowPrivateIPs: new(true)}},
		{name: "mcp_catalog", toolset: Toolset{Type: "mcp_catalog", AllowedServers: []string{"github"}, BlockedServers: []string{"slack"}}},
		{name: "mcp local command", toolset: Toolset{Type: "mcp", Command: "server", Args: []string{"--stdio"}, Env: map[string]string{"A": "b"}, Version: "owner/repo@v1", WorkingDir: ".", Config: map[string]any{"k": "v"}, Lifecycle: &LifecycleConfig{Profile: LifecycleProfileResilient}}},
		{name: "mcp local allow_private_ips disabled", toolset: Toolset{Type: "mcp", Command: "server", AllowPrivateIPs: new(false)}},
		{name: "mcp remote", toolset: Toolset{Type: "mcp", Remote: Remote{URL: "https://mcp.example.com", TransportType: "sse", Headers: map[string]string{"Authorization": "Bearer x"}}, AllowPrivateIPs: new(true)}},
		{name: "mcp ref", toolset: Toolset{Type: "mcp", Ref: "docker/duckduckgo", Name: "search", AllowPrivateIPs: new(true)}},
		{name: "a2a", toolset: Toolset{Type: "a2a", URL: "https://agent.example.com", Name: "helper", Headers: map[string]string{"A": "b"}, Remote: Remote{Headers: map[string]string{"C": "d"}}}},
		{name: "lsp", toolset: Toolset{Type: "lsp", Command: "gopls", Args: []string{"serve"}, FileTypes: []string{"go"}, Version: "golang/tools", WorkingDir: ".", Env: map[string]string{"A": "b"}, Lifecycle: &LifecycleConfig{Profile: LifecycleProfileStrict}}},
		{name: "openapi", toolset: Toolset{Type: "openapi", URL: "https://api.example.com/spec.yaml", Headers: map[string]string{"A": "b"}}},
		{name: "open_url", toolset: Toolset{Type: "open_url", URL: "https://example.com", Name: "docs"}},
		{name: "model_picker", toolset: Toolset{Type: "model_picker", Models: []string{"openai/gpt-4o", "anthropic/claude-sonnet-4-5"}}},
		{name: "rag with ref", toolset: Toolset{Type: "rag", Ref: "docs", Name: "kb"}},
		{name: "rag with inline config", toolset: Toolset{Type: "rag", RAGConfig: &RAGConfig{}}},
		{name: "background_agents", toolset: Toolset{Type: "background_agents"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, tt.toolset.validate())
		})
	}
}

// TestToolsetValidateUnknownType documents current behavior: validate only
// checks field/type pairings and per-type requirements, so an unknown type
// with no extra fields passes because the trailing type switch has no
// default case. Unknown types are still rejected elsewhere: agent-schema.json
// constrains `type` to an enum, and toolset creation fails with "unknown
// toolset type" in the teamloader registry (pkg/teamloader/registry.go).
func TestToolsetValidateUnknownType(t *testing.T) {
	t.Parallel()

	toolset := Toolset{Type: "no-such-toolset"}
	require.NoError(t, toolset.validate())
}

func TestValidateNonEmptyEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []string
		wantErr string
	}{
		{name: "nil list", entries: nil},
		{name: "valid entries", entries: []string{"github", "slack"}},
		{name: "empty entry", entries: []string{""}, wantErr: "field[0] must not be empty"},
		{name: "whitespace entry", entries: []string{"github", " \t"}, wantErr: "field[1] must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNonEmptyEntries("field", tt.entries)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidatePathRootEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []string
		wantErr string
	}{
		{name: "nil list", entries: nil},
		{name: "valid entries", entries: []string{".", "~", "/srv/data", "relative/dir"}},
		{name: "empty entry", entries: []string{""}, wantErr: "allow_list[0] must not be empty"},
		{name: "whitespace entry", entries: []string{".", "   "}, wantErr: "allow_list[1] must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validatePathRootEntries("allow_list", tt.entries)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateDomainPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		patterns []string
		wantErr  string
	}{
		{name: "nil list", patterns: nil},
		{name: "plain hostname", patterns: []string{"example.com"}},
		{name: "leading dot subdomain form", patterns: []string{".example.com"}},
		{name: "leading wildcard", patterns: []string{"*.example.com"}},
		{name: "ipv4 cidr", patterns: []string{"10.0.0.0/8"}},
		{name: "ipv6 cidr", patterns: []string{"fd00::/8"}},
		{name: "untrimmed valid entry", patterns: []string{"  example.com  "}},
		{name: "empty entry", patterns: []string{"example.com", " "}, wantErr: "allowed_domains[1] must not be empty"},
		{name: "invalid cidr", patterns: []string{"10.0.0.0/33"}, wantErr: `allowed_domains[0] "10.0.0.0/33" is invalid: not a valid CIDR`},
		{name: "trailing wildcard", patterns: []string{"foo.*"}, wantErr: "'*' is only allowed as a leading '*.' wildcard"},
		{name: "surrounding wildcards", patterns: []string{"*foo*"}, wantErr: "'*' is only allowed as a leading '*.' wildcard"},
		{name: "double wildcard", patterns: []string{"**.example.com"}, wantErr: "'*' is only allowed as a leading '*.' wildcard"},
		{name: "bare wildcard prefix", patterns: []string{"*."}, wantErr: "'*' is only allowed as a leading '*.' wildcard"},
		{name: "mid-label wildcard", patterns: []string{"sub.*.example.com"}, wantErr: "'*' is only allowed as a leading '*.' wildcard"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateDomainPatterns("allowed_domains", tt.patterns)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "localhost", host: "localhost", want: true},
		{name: "localhost mixed case", host: "LocalHost", want: true},
		{name: "localhost with port", host: "localhost:8080", want: true},
		{name: "ipv4 loopback", host: "127.0.0.1", want: true},
		{name: "ipv4 loopback with port", host: "127.0.0.1:9090", want: true},
		{name: "ipv4 loopback range", host: "127.1.2.3", want: true},
		{name: "ipv6 loopback", host: "::1", want: true},
		{name: "bracketed ipv6 loopback", host: "[::1]", want: true},
		{name: "bracketed ipv6 loopback with port", host: "[::1]:8080", want: true},
		{name: "public hostname", host: "example.com", want: false},
		{name: "public hostname with port", host: "example.com:443", want: false},
		{name: "private non-loopback ip", host: "192.168.1.10", want: false},
		{name: "unspecified address", host: "0.0.0.0", want: false},
		{name: "empty string", host: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isLoopbackHost(tt.host))
		})
	}
}

func TestValidateCallbackRedirectURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "https non-loopback", raw: "https://redirect.example.com/oauth?state=x"},
		{name: "https with placeholder", raw: "https://redirect.example.com/cb/${callbackPort}"},
		{name: "http ipv4 loopback", raw: "http://127.0.0.1:8080/callback"},
		{name: "http localhost with placeholder", raw: "http://localhost:${callbackPort}/callback"},
		{name: "http ipv6 loopback with placeholder", raw: "http://[::1]:${callbackPort}/callback"},
		{name: "http non-loopback", raw: "http://redirect.example.com/callback", wantErr: "oauth callbackRedirectURL must use https for non-loopback hosts"},
		{name: "unsupported scheme", raw: "ftp://example.com/cb", wantErr: `oauth callbackRedirectURL scheme must be http or https, got "ftp"`},
		{name: "scheme without host", raw: "javascript:alert(1)", wantErr: "oauth callbackRedirectURL must be an absolute URL"},
		{name: "relative url", raw: "/callback", wantErr: "oauth callbackRedirectURL must be an absolute URL"},
		{name: "empty string", raw: "", wantErr: "oauth callbackRedirectURL must be an absolute URL"},
		{name: "unparseable url", raw: "://bad", wantErr: "oauth callbackRedirectURL must be an absolute URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCallbackRedirectURL(tt.raw)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestConfigValidateErrorWrapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  Config
		wantErr string
	}{
		{
			name:    "provider auth error",
			config:  Config{Providers: map[string]ProviderConfig{"corp": {Provider: "openai", Auth: &AuthConfig{}}}},
			wantErr: "providers.corp: auth.type is required",
		},
		{
			name:    "model first_available error",
			config:  Config{Models: map[string]ModelConfig{"pick": {FirstAvailable: []string{}}}},
			wantErr: "models.pick: first_available must contain at least one candidate",
		},
		{
			name:    "model compaction threshold error",
			config:  Config{Models: map[string]ModelConfig{"main": {Provider: "openai", Model: "gpt-4o", CompactionThreshold: new(1.5)}}},
			wantErr: "models.main: compaction_threshold must be greater than 0 and at most 1",
		},
		{
			name:    "model auth provider mismatch",
			config:  Config{Models: map[string]ModelConfig{"main": {Provider: "openai", Model: "gpt-4o", Auth: &AuthConfig{Type: AuthTypeWorkloadIdentityFederation}}}},
			wantErr: "models.main: auth.type \"workload_identity_federation\" is only supported with the anthropic provider",
		},
		{
			name:    "named toolset error",
			config:  Config{Toolsets: map[string]Toolset{"web": {Type: "fetch", AllowedDomains: []string{""}}}},
			wantErr: "toolsets.web: allowed_domains[0] must not be empty",
		},
		{
			name:    "agent fallback error",
			config:  Config{Agents: Agents{{Name: "root", Fallback: &FallbackConfig{Retries: -2}}}},
			wantErr: "fallback.retries must be >= -1",
		},
		{
			name:    "agent harness error",
			config:  Config{Agents: Agents{{Name: "root", Harness: &HarnessConfig{}}}},
			wantErr: "harness.type is required",
		},
		{
			name:    "agent compaction threshold error",
			config:  Config{Agents: Agents{{Name: "root", CompactionThreshold: new(0.0)}}},
			wantErr: "agents.root: compaction_threshold must be greater than 0 and at most 1",
		},
		{
			name:    "agent toolset error",
			config:  Config{Agents: Agents{{Name: "root", Toolsets: []Toolset{{Type: "a2a"}}}}},
			wantErr: "a2a toolset requires a url to be set",
		},
		{
			name:    "agent hooks error",
			config:  Config{Agents: Agents{{Name: "root", Hooks: &HooksConfig{Stop: HookDefinitions{{}}}}}},
			wantErr: "hooks.stop[0]: type is required",
		},
		{
			name:    "runtime safety error",
			config:  Config{Runtime: &RuntimeDefaults{Safety: "yolo"}},
			wantErr: "runtime.safety: invalid safety mode \"yolo\" (valid: strict, balanced, restricted, autonomous)",
		},
		{
			name:    "runtime safety legacy alias rejected",
			config:  Config{Runtime: &RuntimeDefaults{Safety: "unsafe"}},
			wantErr: "runtime.safety: invalid safety mode \"unsafe\"",
		},
		{
			name:    "agent safety error",
			config:  Config{Agents: Agents{{Name: "root", Safety: "Strict"}}},
			wantErr: "agents.root.safety: invalid safety mode \"Strict\" (valid: strict, balanced, restricted, autonomous)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, tt.config.Validate(), tt.wantErr)
		})
	}
}

func TestConfigValidateValidConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Version: Version,
		Providers: map[string]ProviderConfig{
			"corp": {Provider: "anthropic", BaseURL: "https://llm.example.com/v1"},
		},
		Models: map[string]ModelConfig{
			"main": {Provider: "openai", Model: "gpt-4o", CompactionThreshold: new(0.8)},
			"pick": {FirstAvailable: []string{"anthropic/claude-sonnet-4-5", "dmr/local"}},
			// Auth on a model routed through a custom provider exercises the
			// EffectiveProviderType indirection in Config.Validate.
			"wif": {
				Provider: "corp",
				Model:    "claude-sonnet-4-5",
				Auth: &AuthConfig{
					Type: AuthTypeWorkloadIdentityFederation,
					Federation: &FederationAuthConfig{
						FederationRuleID: "fdrl_123",
						OrganizationID:   "org-id-for-tests",
						IdentityToken:    &IdentityTokenSourceConfig{Env: "ANTHROPIC_OIDC_TOKEN"},
					},
				},
			},
		},
		Toolsets: map[string]Toolset{
			"web": {Type: "fetch", AllowedDomains: []string{"example.com", "*.example.org"}},
		},
		// All four canonical modes are explicitly permitted at runtime and
		// agent scope, autonomous included.
		Runtime: &RuntimeDefaults{Safety: SafetyModeAutonomous},
		Agents: Agents{
			{
				Name:                "root",
				Model:               "main",
				Safety:              SafetyModeBalanced,
				Fallback:            &FallbackConfig{Models: []string{"pick"}, Retries: -1, Cooldown: Duration{Duration: time.Minute}},
				Harness:             &HarnessConfig{Type: "claude-code", Effort: "high"},
				CompactionThreshold: new(1.0),
				Toolsets: []Toolset{
					{Type: "shell"},
					{Type: "mcp", Command: "server"},
				},
				Hooks: &HooksConfig{Stop: HookDefinitions{{Type: "command", Command: "echo done"}}},
			},
			{Name: "careful", Model: "main", Safety: SafetyModeStrict},
			{Name: "headless", Model: "main", Safety: SafetyModeRestricted},
		},
	}

	require.NoError(t, cfg.Validate())
}

func TestSafetyModeValidate(t *testing.T) {
	t.Parallel()

	cases := map[SafetyMode]bool{
		"":                   true,
		SafetyModeStrict:     true,
		SafetyModeBalanced:   true,
		SafetyModeRestricted: true,
		SafetyModeAutonomous: true,

		// Legacy session aliases and near-misses are rejected: only the
		// four canonical modes may appear in YAML safety fields.
		"unsafe":     false,
		"safer":      false,
		"safe-auto":  false,
		"yolo":       false,
		"Balanced":   false, // case-sensitive on purpose
		"Restricted": false,
	}

	for in, wantOK := range cases {
		err := in.Validate()
		if wantOK {
			require.NoErrorf(t, err, "SafetyMode(%q).Validate()", string(in))
		} else {
			require.ErrorContainsf(t, err, "invalid safety mode", "SafetyMode(%q).Validate()", string(in))
			require.ErrorContainsf(t, err, "strict, balanced, restricted, autonomous", "SafetyMode(%q).Validate()", string(in))
		}
	}
}

func TestStructuredOutputValidateMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		so      *StructuredOutput
		wantErr string
	}{
		{name: "nil is valid", so: nil},
		{name: "mode absent", so: &StructuredOutput{Name: "x", Schema: map[string]any{"type": "object"}}},
		{name: "mode native", so: &StructuredOutput{Name: "x", Mode: StructuredOutputModeNative}},
		{name: "mode tool", so: &StructuredOutput{Name: "x", Mode: StructuredOutputModeTool}},
		{name: "unknown mode", so: &StructuredOutput{Name: "x", Mode: "json"}, wantErr: `unsupported mode "json" (must be one of: native, tool)`},
		{name: "case sensitive", so: &StructuredOutput{Name: "x", Mode: "Tool"}, wantErr: "unsupported mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.so.validate()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestConfigValidateStructuredOutputMode pins that an unknown
// structured_output.mode is rejected at config level with the agent's name
// in the error, and the two documented modes (plus absent) pass.
func TestConfigValidateStructuredOutputMode(t *testing.T) {
	t.Parallel()

	build := func(mode string) *Config {
		return &Config{Agents: []AgentConfig{{
			Name: "root",
			StructuredOutput: &StructuredOutput{
				Name:   "result",
				Schema: map[string]any{"type": "object"},
				Mode:   mode,
			},
		}}}
	}

	require.NoError(t, build("").Validate())
	require.NoError(t, build(StructuredOutputModeNative).Validate())
	require.NoError(t, build(StructuredOutputModeTool).Validate())

	err := build("strict").Validate()
	require.ErrorContains(t, err, "agents.root.structured_output")
	require.ErrorContains(t, err, `unsupported mode "strict"`)
}

func TestStructuredOutputToolMode(t *testing.T) {
	t.Parallel()

	var nilSO *StructuredOutput
	require.False(t, nilSO.ToolMode())
	require.False(t, (&StructuredOutput{}).ToolMode())
	require.False(t, (&StructuredOutput{Mode: StructuredOutputModeNative}).ToolMode())
	require.True(t, (&StructuredOutput{Mode: StructuredOutputModeTool}).ToolMode())
}
