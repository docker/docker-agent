package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
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

func TestAgentConfig_Validate_ToolChoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "valid tool_choice auto",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    tool_choice: auto
`,
			wantErr: "",
		},
		{
			name: "valid tool_choice required",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    tool_choice: required
`,
			wantErr: "",
		},
		{
			name: "valid tool_choice none",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    tool_choice: none
`,
			wantErr: "",
		},
		{
			name: "no tool_choice set",
			config: `
agents:
  root:
    model: "openai/gpt-4"
`,
			wantErr: "",
		},
		{
			name: "invalid tool_choice value",
			config: `
agents:
  root:
    model: "openai/gpt-4"
    tool_choice: force
`,
			wantErr: "tool_choice must be one of: auto, required, none",
		},
	}

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
