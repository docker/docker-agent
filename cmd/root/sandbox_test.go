package root

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/sandbox/kit"
)

// TestDockerAgentArgs_NoDuplicateArgs is a regression test for a bug where the
// agent file and --config-dir were appended twice, causing the agent file to be
// passed as the first message inside the sandbox.
func TestDockerAgentArgs_NoDuplicateArgs(t *testing.T) {
	cmd := &cobra.Command{
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var sandboxFlag bool
	cmd.PersistentFlags().BoolVar(&sandboxFlag, "sandbox", false, "")

	args := []string{"./pokemon.yaml"}
	require.NoError(t, cmd.ParseFlags([]string{"--sandbox"}))

	got := dockerAgentArgs(cmd, args, "/some/config/dir")

	// The agent file must appear exactly once.
	count := 0
	for _, a := range got {
		if a == "./pokemon.yaml" {
			count++
		}
	}
	assert.Equal(t, 1, count, "agent file should appear once in args, got: %v", got)

	// --config-dir must appear exactly once.
	configDirCount := 0
	for _, a := range got {
		if a == "--config-dir" {
			configDirCount++
		}
	}
	assert.Equal(t, 1, configDirCount, "--config-dir should appear once in args, got: %v", got)

	// The agent file should come before --config-dir so the cobra run command
	// sees it as the first positional argument (the agent) and not as a message.
	agentIdx := slices.Index(got, "./pokemon.yaml")
	cfgIdx := slices.Index(got, "--config-dir")
	assert.Less(t, agentIdx, cfgIdx, "agent file should precede --config-dir, got: %v", got)

	// --sandbox and --sbx flags must be stripped so we don't recurse into
	// another sandbox.
	assert.NotContains(t, got, "--sandbox")
	assert.NotContains(t, got, "--sbx")

	// --yolo is added by default so tool calls run unattended in the sandbox.
	assert.Contains(t, got, "--yolo")
}

// TestDockerAgentArgs_PreservesUserYolo ensures that if the user explicitly
// set --yolo, it is not duplicated.
func TestDockerAgentArgs_PreservesUserYolo(t *testing.T) {
	cmd := &cobra.Command{
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var sandboxFlag, yolo bool
	cmd.PersistentFlags().BoolVar(&sandboxFlag, "sandbox", false, "")
	cmd.PersistentFlags().BoolVar(&yolo, "yolo", false, "")

	require.NoError(t, cmd.ParseFlags([]string{"--sandbox", "--yolo"}))

	got := dockerAgentArgs(cmd, []string{"./agent.yaml"}, "/cfg")

	yoloCount := 0
	for _, a := range got {
		if strings.HasPrefix(a, "--yolo") {
			yoloCount++
		}
	}
	assert.Equal(t, 1, yoloCount, "--yolo should not be duplicated, got: %v", got)
	assert.Contains(t, got, "--yolo=true")
}

func TestDockerAgentArgs_PreservesExplicitFalseBool(t *testing.T) {
	cmd := &cobra.Command{
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var sandboxFlag, lean bool
	cmd.PersistentFlags().BoolVar(&sandboxFlag, "sandbox", false, "")
	cmd.PersistentFlags().BoolVar(&lean, "lean", true, "")

	require.NoError(t, cmd.ParseFlags([]string{"--sandbox", "--lean=false"}))

	got := dockerAgentArgs(cmd, []string{"./agent.yaml"}, "/cfg")

	assert.Contains(t, got, "--lean=false")
	assert.NotContains(t, got, "--lean")
}

func TestGatewayHostPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"bare host", "example.com", "example.com"},
		{"bare authority", "example.com:443", "example.com:443"},
		{"https URL", "https://example.com/proxy", "example.com"},
		{"https URL with port", "https://example.com:8443/proxy", "example.com:8443"},
		{"production gateway", "https://ai-backend-service.docker.com/proxy", "ai-backend-service.docker.com"},
		{"staging gateway with path", "https://ai-backend-service-stage.docker.com/proxy", "ai-backend-service-stage.docker.com"},
		{"bare authority with path", "example.com:443/proxy", "example.com:443"},
		{"bare authority with query", "example.com:443?foo=bar", "example.com:443"},
		{"protocol-relative authority", "//example.com/proxy", "example.com"},
		{"https URL with userinfo", "https://user:pw@example.com/proxy", "example.com"},
		{"https URL with fragment", "https://example.com/proxy#frag", "example.com"},
		{"IPv6 host", "https://[::1]:8443/proxy", "[::1]:8443"},
		{"scheme without host", "https:///proxy", ""},
		{"only fragment", "#fragment", ""},
		{"only path", "/path", ""},
		{"opaque scheme", "mailto:foo@example.com", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, gatewayHostPort(tt.raw))
		})
	}
}

func TestDisplayGatewayURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no userinfo", "https://example.com/proxy", "https://example.com/proxy"},
		{"bare authority unchanged", "example.com:443", "example.com:443"},
		{
			name: "username only is masked",
			raw:  "https://user@example.com/proxy",
			want: "https://***@example.com/proxy",
		},
		{
			name: "username and password are masked",
			raw:  "https://user:supersecret@example.com:443/proxy?token=abc",
			want: "https://***@example.com:443/proxy?token=abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := displayGatewayURL(tt.raw)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "supersecret",
				"displayGatewayURL must not preserve a password")
		})
	}
}

func TestPrintModelsGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		gateway string
		want    string
	}{
		{
			name:    "no gateway",
			gateway: "",
			want:    "Models gateway: none configured\n",
		},
		{
			name:    "URL gateway shows allow-listed host",
			gateway: "https://ai-backend-service-stage.docker.com/proxy",
			want:    "Models gateway: https://ai-backend-service-stage.docker.com/proxy (allowlisting ai-backend-service-stage.docker.com in the sandbox proxy)\n",
		},
		{
			name:    "bare authority is its own host",
			gateway: "ai-backend-service.docker.com:443",
			want:    "Models gateway: ai-backend-service.docker.com:443\n",
		},
		{
			name:    "URL with credentials is rendered without them",
			gateway: "https://user:supersecret@gw.example.com/proxy",
			want:    "Models gateway: https://***@gw.example.com/proxy (allowlisting gw.example.com in the sandbox proxy)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			printModelsGateway(&buf, tt.gateway)
			assert.Equal(t, tt.want, buf.String())
			assert.NotContains(t, buf.String(), "supersecret",
				"printed gateway must never include credentials")
		})
	}
}

func TestPrintModelsDevAllowance(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	printModelsDevAllowance(&buf)
	assert.Equal(t, "Models catalog: allowlisting models.dev in the sandbox proxy\n", buf.String())
}

func TestPrintToolInstallAllowance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  *kit.Result
		want    string
		wantNot []string
	}{
		{
			name:   "nil result is silent",
			result: nil,
			want:   "",
		},
		{
			name:   "no auto-install is silent",
			result: &kit.Result{NeedsToolInstall: false},
			want:   "",
		},
		{
			name: "lists every host on its own line",
			result: &kit.Result{
				NeedsToolInstall: true,
				ToolInstallHosts: []string{
					"api.github.com", "proxy.golang.org", "sum.golang.org",
				},
			},
			want: "Tool install: agent has at least one MCP/LSP toolset, allowlisting 3 package host(s) in the sandbox proxy:\n" +
				"  - api.github.com\n" +
				"  - proxy.golang.org\n" +
				"  - sum.golang.org\n",
		},
		{
			name: "resolution errors are surfaced after the host list",
			result: &kit.Result{
				NeedsToolInstall: true,
				ToolInstallHosts: []string{"api.github.com"},
				ToolInstallHostsResolutionErr: []kit.ToolHostError{
					{Command: "gopls", Version: "golang/tools@v0.21.0", Err: errors.New("boom")},
				},
			},
			want: "Tool install: agent has at least one MCP/LSP toolset, allowlisting 1 package host(s) in the sandbox proxy:\n" +
				"  - api.github.com\n" +
				"  ! resolving install hosts for \"gopls\"@\"golang/tools@v0.21.0\": boom (using fallback host set)\n" +
				"  hint: persist a missing host with `docker agent sandbox allow <host>`\n",
			wantNot: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			printToolInstallAllowance(&buf, tt.result)
			assert.Equal(t, tt.want, buf.String())
			for _, ne := range tt.wantNot {
				assert.NotContains(t, buf.String(), ne)
			}
		})
	}
}

func TestResolveSandboxDefault(t *testing.T) {
	dir := t.TempDir()
	sbxPath := filepath.Join(dir, "runtime-sandbox.yaml")
	require.NoError(t, os.WriteFile(sbxPath,
		[]byte("runtime:\n  sandbox: true\nagents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n"),
		0o600))
	plainPath := filepath.Join(dir, "plain.yaml")
	require.NoError(t, os.WriteFile(plainPath,
		[]byte("agents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n"),
		0o600))

	tests := []struct {
		name     string
		agentRef string
		current  bool
		want     bool
		wantCfg  bool
	}{
		{"empty ref, flag false", "", false, false, false},
		{"empty ref, flag already true", "", true, true, false},
		{"runtime.sandbox: true picked up", sbxPath, false, true, true},
		{"plain agent stays false", plainPath, false, false, true},
		{"current=true short-circuits the decision", plainPath, true, true, true},
		{"unresolvable ref stays false", filepath.Join(dir, "missing.yaml"), false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cfg := resolveSandboxDefault(t.Context(), tt.agentRef, tt.current)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantCfg, cfg != nil)
		})
	}
}

func TestPeekAgentSandbox(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "runtime sandbox true",
			yaml: "runtime:\n  sandbox: true\nagents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n",
			want: true,
		},
		{
			name: "runtime sandbox false",
			yaml: "runtime:\n  sandbox: false\nagents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n",
			want: false,
		},
		{
			name: "runtime block absent",
			yaml: "agents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o600))

			cfg := loadAgentConfig(t.Context(), path)
			got := cfg != nil && cfg.Runtime != nil && cfg.Runtime.Sandbox
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("empty ref", func(t *testing.T) {
		assert.Nil(t, loadAgentConfig(t.Context(), ""))
	})

	t.Run("unresolvable ref", func(t *testing.T) {
		assert.Nil(t, loadAgentConfig(t.Context(), "/nonexistent/agent.yaml"))
	})
}

func TestAgentNetworkAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	yamlBody := "runtime:\n" +
		"  network_allowlist:\n" +
		"    - api.example.com\n" +
		"    - registry.npmjs.org:443\n" +
		"agents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n"
	require.NoError(t, os.WriteFile(path, []byte(yamlBody), 0o600))

	cfg := loadAgentConfig(t.Context(), path)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"api.example.com", "registry.npmjs.org:443"},
		agentNetworkAllowlist(t.Context(), cfg))
}

func TestAgentNetworkAllowlist_FiltersMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	// A YAML where two hosts were typed as one comma-separated string
	// must not feed a single bogus rule into the proxy policy.
	yamlBody := "runtime:\n" +
		"  network_allowlist:\n" +
		"    - api.example.com\n" +
		"    - \"bad.example.com, also.bad.com\"\n" +
		"    - \"has space.example.com\"\n" +
		"    - registry.npmjs.org:443\n" +
		"agents:\n  root:\n    model: openai/gpt-4o\n    description: t\n    instruction: t\n"
	require.NoError(t, os.WriteFile(path, []byte(yamlBody), 0o600))

	cfg := loadAgentConfig(t.Context(), path)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"api.example.com", "registry.npmjs.org:443"},
		agentNetworkAllowlist(t.Context(), cfg))
}

func TestAgentNetworkAllowlist_NilCfg(t *testing.T) {
	assert.Nil(t, agentNetworkAllowlist(t.Context(), nil))
}

func TestPrintAgentNetworkAllowlist(t *testing.T) {
	tests := []struct {
		name  string
		hosts []string
		want  string
	}{
		{
			name:  "empty",
			hosts: nil,
			want:  "",
		},
		{
			name:  "single host",
			hosts: []string{"api.example.com"},
			want: "Agent network allowlist: allowlisting 1 host(s) declared in runtime.network_allowlist:\n" +
				"  - api.example.com\n",
		},
		{
			name:  "multiple",
			hosts: []string{"a.example.com", "b.example.com"},
			want: "Agent network allowlist: allowlisting 2 host(s) declared in runtime.network_allowlist:\n" +
				"  - a.example.com\n" +
				"  - b.example.com\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			printAgentNetworkAllowlist(&buf, tt.hosts)
			assert.Equal(t, tt.want, buf.String())
		})
	}
}
