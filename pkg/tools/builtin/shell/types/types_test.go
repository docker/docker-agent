package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/safety"
)

func TestRunShellArgs_UnmarshalJSON_AcceptsCmdAndCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantCmd string
		wantCwd string
		wantTO  int
	}{
		{
			name:    "canonical cmd",
			input:   `{"cmd":"ls -la","cwd":"/tmp","timeout":10}`,
			wantCmd: "ls -la",
			wantCwd: "/tmp",
			wantTO:  10,
		},
		{
			name:    "alias command",
			input:   `{"command":"ls -la","cwd":"/tmp","timeout":10}`,
			wantCmd: "ls -la",
			wantCwd: "/tmp",
			wantTO:  10,
		},
		{
			name:    "both present cmd wins",
			input:   `{"cmd":"from-cmd","command":"from-command"}`,
			wantCmd: "from-cmd",
		},
		{
			name:    "blank cmd falls back to command alias",
			input:   `{"cmd":"   ","command":"from-command"}`,
			wantCmd: "from-command",
		},
		{
			name:    "empty cmd falls back to command alias",
			input:   `{"cmd":"","command":"from-command"}`,
			wantCmd: "from-command",
		},
		{
			name:    "empty object leaves cmd empty",
			input:   `{}`,
			wantCmd: "",
		},
		{
			// encoding/json struct decoding would let a mixed-case key
			// override the exact one the runtime classified.
			name:    "mixed-case keys are not aliases",
			input:   `{"cmd":"git status","CMD":"rm -rf /tmp/x","Command":"rm -rf /"}`,
			wantCmd: "git status",
		},
		{
			name:    "non-string cmd is ignored, alias runs",
			input:   `{"cmd":42,"command":"from-command"}`,
			wantCmd: "from-command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got RunShellArgs
			require.NoError(t, json.Unmarshal([]byte(tt.input), &got))
			assert.Equal(t, tt.wantCmd, got.Cmd)
			assert.Equal(t, tt.wantCwd, got.Cwd)
			assert.Equal(t, tt.wantTO, got.Timeout)

			// The executed command must be exactly what the runtime labels.
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.input), &fields))
			classified, _ := safety.CommandArg(fields)
			assert.Equal(t, classified, got.Cmd)
		})
	}
}
