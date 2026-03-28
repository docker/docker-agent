package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

type validateConfigCase struct {
	name    string
	config  string
	wantErr string
}

func runValidateConfigCases(t *testing.T, tests []validateConfigCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg Config
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

func TestToolset_Validate_LSP(t *testing.T) {
	t.Parallel()

	tests := []validateConfigCase{
		{
			name: "valid lsp with command",
			config: `
version: "3"
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
version: "3"
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
version: "3"
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
version: "3"
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
version: "5"
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
version: "5"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        file_types: [".go"]
`,
			wantErr: "file_types can only be used with type 'lsp'",
		},
	}

	runValidateConfigCases(t, tests)
}

func TestToolset_Validate_RemoteOAuth(t *testing.T) {
	t.Parallel()

	tests := []validateConfigCase{
		{
			name: "valid oauth with clientId",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.slack.com/mcp
          transport_type: streamable
          oauth:
            clientId: "my-client-id"
`,
			wantErr: "",
		},
		{
			name: "valid oauth with all fields",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.slack.com/mcp
          transport_type: streamable
          oauth:
            clientId: "my-client-id"
            clientSecret: "my-secret"
            callbackPort: 3118
            scopes:
              - search:read
              - chat:write
`,
			wantErr: "",
		},
		{
			name: "valid oauth with zero callbackPort (random port)",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.slack.com/mcp
          oauth:
            clientId: "my-client-id"
            callbackPort: 0
`,
			wantErr: "",
		},
		{
			name: "oauth missing clientId",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.slack.com/mcp
          oauth:
            clientSecret: "my-secret"
`,
			wantErr: "remote.oauth.clientId must be set when oauth is configured",
		},
		{
			name: "oauth with negative callbackPort",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.slack.com/mcp
          oauth:
            clientId: "my-client-id"
            callbackPort: -1
`,
			wantErr: "remote.oauth.callbackPort must be >= 0",
		},
		{
			name: "oauth on non-mcp toolset",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: shell
        remote:
          oauth:
            clientId: "my-client-id"
`,
			wantErr: "remote.oauth can only be used with type 'mcp'",
		},
		{
			name: "oauth without remote url",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        command: some-mcp-server
        remote:
          oauth:
            clientId: "my-client-id"
`,
			wantErr: "remote.oauth requires remote.url to be set",
		},
		{
			name: "remote mcp without oauth passes",
			config: `
version: "3"
agents:
  root:
    model: "openai/gpt-4"
    toolsets:
      - type: mcp
        remote:
          url: https://mcp.example.com/mcp
          transport_type: streamable
`,
			wantErr: "",
		},
	}

	runValidateConfigCases(t, tests)
}
