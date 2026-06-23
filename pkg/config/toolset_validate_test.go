package config

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestToolset_Validate_LSP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "valid lsp with command",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: lsp
        command: gopls
`,
			wantErr: "",
		},
		{
			name: "lsp missing command",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: lsp
`,
			wantErr: "lsp toolset requires a command to be set",
		},
		{
			name: "lsp with args",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: lsp
        command: gopls
        args:
          - -remote=auto
`,
			wantErr: "",
		},
		{
			name: "lsp with env",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: lsp
        command: gopls
        env:
          GOFLAGS: "-mod=vendor"
`,
			wantErr: "",
		},
		{
			name: "lsp with file_types",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: lsp
        command: gopls
        file_types: [".go", ".mod"]
`,
			wantErr: "",
		},
		{
			name: "file_types on non-lsp toolset",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        file_types: [".go"]
`,
			wantErr: "file_types can only be used with type 'lsp'",
		},
		{
			name: "lsp with working_dir",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: lsp
        command: gopls
        working_dir: ./backend
`,
			wantErr: "",
		},
		{
			name: "working_dir on non-mcp-lsp toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        working_dir: ./backend
`,
			wantErr: "working_dir can only be used with type 'mcp' or 'lsp'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestToolset_Validate_SudoAskpass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "sudo_askpass on shell is allowed",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        sudo_askpass: true
`,
			wantErr: "",
		},
		{
			name: "sudo_askpass false on shell is allowed",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        sudo_askpass: false
`,
			wantErr: "",
		},
		{
			name: "sudo_askpass on non-shell toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: filesystem
        sudo_askpass: true
`,
			wantErr: "sudo_askpass can only be used with type 'shell'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestToolset_Validate_MCP_WorkingDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    string
		wantErr   string
		wantValue string
	}{
		{
			name: "mcp with working_dir",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        command: my-mcp-server
        working_dir: ./tools/mcp
`,
			wantErr:   "",
			wantValue: "./tools/mcp",
		},
		{
			name: "mcp without working_dir defaults to empty",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        command: my-mcp-server
`,
			wantErr:   "",
			wantValue: "",
		},
		{
			name: "working_dir on remote mcp is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
        working_dir: ./tools
`,
			wantErr:   "working_dir is not valid for remote MCP toolsets",
			wantValue: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantValue, cfg.Agents.First().Toolsets[0].WorkingDir)
			}
		})
	}
}

func TestToolset_Validate_Fetch_Domains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "fetch with allowed_domains",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - docker.com
          - github.com
`,
			wantErr: "",
		},
		{
			name: "fetch with blocked_domains",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        blocked_domains:
          - 169.254.169.254
`,
			wantErr: "",
		},
		{
			name: "fetch with both is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - docker.com
        blocked_domains:
          - example.com
`,
			wantErr: "allowed_domains and blocked_domains are mutually exclusive",
		},
		{
			name: "allowed_domains on non-fetch toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        allowed_domains:
          - docker.com
`,
			wantErr: "allowed_domains can only be used with type 'fetch'",
		},
		{
			name: "blocked_domains on non-fetch toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        blocked_domains:
          - docker.com
`,
			wantErr: "blocked_domains can only be used with type 'fetch'",
		},
		{
			name: "allow_private_ips on unsupported toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        allow_private_ips: true
`,
			wantErr: "allow_private_ips can only be used with type 'fetch', 'api', 'openapi', 'a2a' or remote MCP toolsets",
		},
		{
			name: "allow_private_ips on fetch toolset is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allow_private_ips: true
`,
		},
		{
			name: "allow_private_ips on api toolset is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: api
        allow_private_ips: true
        api_config:
          name: probe
          method: GET
          endpoint: http://10.0.0.1/health
          instruction: probe
`,
		},
		{
			name: "allow_private_ips on openapi toolset is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: openapi
        url: http://10.0.0.1/openapi.json
        allow_private_ips: true
`,
		},
		{
			name: "allow_private_ips on a2a toolset is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: a2a
        url: http://10.0.0.1/.well-known/agent-card.json
        allow_private_ips: true
`,
		},
		{
			name: "allow_private_ips on remote mcp toolset is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        allow_private_ips: true
        remote:
          url: "https://mcp.example.com/mcp"
          transport_type: streamable
`,
		},
		{
			name: "allow_private_ips on local mcp command is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        allow_private_ips: true
        command: docker
`,
			wantErr: "allow_private_ips can only be used with type 'fetch', 'api', 'openapi', 'a2a' or remote MCP toolsets",
		},
		{
			name: "empty allowed_domains entry is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - ""
          - docker.com
`,
			wantErr: "allowed_domains[0] must not be empty",
		},
		{
			name: "whitespace-only blocked_domains entry is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        blocked_domains:
          - "   "
`,
			wantErr: "blocked_domains[0] must not be empty",
		},
		{
			name: "fetch with wildcard subdomain pattern",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - "*.example.com"
`,
			wantErr: "",
		},
		{
			name: "fetch with ipv4 CIDR pattern",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        blocked_domains:
          - 169.254.0.0/16
          - 10.0.0.0/8
`,
			wantErr: "",
		},
		{
			name: "fetch with ipv6 CIDR pattern",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        blocked_domains:
          - "fc00::/7"
          - "::1/128"
`,
			wantErr: "",
		},
		{
			name: "malformed CIDR is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        blocked_domains:
          - 10.0.0.0/33
`,
			wantErr: "not a valid CIDR",
		},
		{
			name: "interior wildcard is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - "foo.*"
`,
			wantErr: "'*' is only allowed as a leading '*.' wildcard",
		},
		{
			name: "double wildcard is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - "*.*.example.com"
`,
			wantErr: "'*' is only allowed as a leading '*.' wildcard",
		},
		{
			name: "bare star is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        allowed_domains:
          - "*"
`,
			wantErr: "'*' is only allowed as a leading '*.' wildcard",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestToolset_Validate_Fetch_Headers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "fetch with headers is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: fetch
        headers:
          Authorization: "Bearer token"
          X-Api-Key: "key-123"
`,
			wantErr: "",
		},
		{
			name: "openapi with headers is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: openapi
        url: https://api.example.com/openapi.json
        headers:
          Authorization: "Bearer token"
`,
			wantErr: "",
		},
		{
			name: "a2a with headers is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: a2a
        url: https://a2a.example.com
        headers:
          Authorization: "Bearer token"
`,
			wantErr: "",
		},
		{
			name: "headers on shell toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        headers:
          Authorization: "Bearer token"
`,
			wantErr: "headers can only be used with type 'openapi', 'a2a' or 'fetch'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestToolset_Validate_MCPCatalog_ServerLists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "mcp_catalog with allowed_servers",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp_catalog
        allowed_servers:
          - docker-docs
          - microsoft-learn
`,
			wantErr: "",
		},
		{
			name: "mcp_catalog with blocked_servers",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp_catalog
        blocked_servers:
          - gitmcp
`,
			wantErr: "",
		},
		{
			name: "mcp_catalog with both lists is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp_catalog
        allowed_servers:
          - docker-docs
          - gitmcp
        blocked_servers:
          - gitmcp
`,
			wantErr: "",
		},
		{
			name: "allowed_servers on non-mcp_catalog toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        allowed_servers:
          - docker-docs
`,
			wantErr: "allowed_servers can only be used with type 'mcp_catalog'",
		},
		{
			name: "blocked_servers on non-mcp_catalog toolset is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        blocked_servers:
          - docker-docs
`,
			wantErr: "blocked_servers can only be used with type 'mcp_catalog'",
		},
		{
			name: "empty allowed_servers entry is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp_catalog
        allowed_servers:
          - ""
          - docker-docs
`,
			wantErr: "allowed_servers[0] must not be empty",
		},
		{
			name: "whitespace-only blocked_servers entry is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp_catalog
        blocked_servers:
          - "   "
`,
			wantErr: "blocked_servers[0] must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestToolset_Validate_MCP_RemoteOAuth_CallbackRedirectURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "callbackRedirectURL absolute URL is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: https://redirect.example.com/cb
`,
			wantErr: "",
		},
		{
			name: "callbackRedirectURL with placeholder is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: "https://redirect.example.com/cb?port=${callbackPort}"
`,
			wantErr: "",
		},
		{
			name: "http on loopback is accepted",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: "http://localhost:${callbackPort}/cb"
`,
			wantErr: "",
		},
		{
			name: "http on non-loopback host is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: "http://redirect.example.com/cb"
`,
			wantErr: "must use https for non-loopback hosts",
		},
		{
			name: "javascript scheme is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: "javascript:alert(1)"
`,
			wantErr: "must be an absolute URL",
		},
		{
			name: "ftp scheme is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: "ftp://example.com/cb"
`,
			wantErr: "scheme must be http or https",
		},
		{
			name: "relative callbackRedirectURL is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: /just/a/path
`,
			wantErr: "oauth callbackRedirectURL must be an absolute URL",
		},
		{
			name: "garbage callbackRedirectURL is rejected",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/sse
          oauth:
            clientId: cid
            callbackRedirectURL: "://bad-url"
`,
			wantErr: "oauth callbackRedirectURL must be an absolute URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg latest.Config
			err := yaml.Unmarshal([]byte(tt.config), &cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
