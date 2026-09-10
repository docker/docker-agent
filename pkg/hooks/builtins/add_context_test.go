package builtins_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

func TestAddContext(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddContext)
	in := &hooks.Input{
		SessionID:     "session-123",
		AgentName:     "root",
		Cwd:           "/workspace",
		HookEventName: hooks.EventSessionStart,
		Prompt:        "<text> & $(echo unsafe) {{ .SessionID }}",
		ToolInput:     map[string]any{"cmd": "go test ./..."},
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "session ID",
			args: []string{"Current session ID: {{ .SessionID }}"},
			want: "Current session ID: session-123",
		},
		{
			name: "multiple arguments",
			args: []string{"Session: {{ .SessionID }}", "Agent: {{ .AgentName }}", "Working directory: {{ .Cwd }}"},
			want: "Session: session-123\nAgent: root\nWorking directory: /workspace",
		},
		{
			name: "literal text and whitespace",
			args: []string{"  First line\nSecond line  "},
			want: "  First line\nSecond line  ",
		},
		{
			name: "skip empty results",
			args: []string{"", " \n\t", "{{ .Source }}", "Session: {{ .SessionID }}"},
			want: "Session: session-123",
		},
		{
			name: "values remain plain text",
			args: []string{"{{ .Prompt }}"},
			want: in.Prompt,
		},
		{
			name: "event-specific fields and conditionals",
			args: []string{"{{ if .ToolInput }}Command: {{ .ToolInput.cmd }}{{ end }}"},
			want: "Command: go test ./...",
		},
		{
			name: "standard template functions",
			args: []string{`{{ printf "Session: %s" .SessionID }}`},
			want: "Session: session-123",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := fn(t.Context(), in, tc.args)
			require.NoError(t, err)
			require.NotNil(t, out)
			require.NotNil(t, out.HookSpecificOutput)
			assert.Equal(t, in.HookEventName, out.HookSpecificOutput.HookEventName)
			assert.Equal(t, tc.want, out.HookSpecificOutput.AdditionalContext)
		})
	}
}

func TestAddContextNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddContext)
	for _, tc := range []struct {
		name string
		in   *hooks.Input
		args []string
	}{
		{"nil input", nil, []string{"{{ .SessionID }}"}},
		{"no arguments", &hooks.Input{}, nil},
		{"empty output", &hooks.Input{}, []string{"", " \n\t", "{{ .SessionID }}"}},
		{"guarded nil map", &hooks.Input{}, []string{"{{ if .ToolInput }}{{ .ToolInput.cmd }}{{ end }}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := fn(t.Context(), tc.in, tc.args)
			require.NoError(t, err)
			assert.Nil(t, out)
		})
	}
}

func TestAddContextTemplateErrors(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddContext)
	for _, tc := range []struct {
		name string
		arg  string
		in   *hooks.Input
		want string
	}{
		{"invalid syntax", "{{", &hooks.Input{}, "parse arg 2"},
		{"unknown function", "{{ unknown }}", &hooks.Input{}, "parse arg 2"},
		{"unknown field", "{{ .SessionId }}", &hooks.Input{}, "render arg 2"},
		{"JSON field name", "{{ .session_id }}", &hooks.Input{}, "render arg 2"},
		{"missing map key", "{{ .ToolInput.missing }}", &hooks.Input{ToolInput: map[string]any{}}, "render arg 2"},
		{"nil map", "{{ .ToolInput.cmd }}", &hooks.Input{}, "render arg 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := fn(t.Context(), tc.in, []string{"No partial output", tc.arg})
			require.ErrorContains(t, err, "add_context: "+tc.want)
			assert.Nil(t, out)
		})
	}
}

func TestAddContextDispatch(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, builtins.Register(r))
	for _, event := range []hooks.EventType{hooks.EventSessionStart, hooks.EventTurnStart} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()

			hook := hooks.Hook{
				Type:    hooks.HookTypeBuiltin,
				Command: builtins.AddContext,
				Args:    []string{"{{ .SessionID }} / {{ .HookEventName }} / {{ .Cwd }}", `{"decision":"block"}`},
			}
			exec := hooks.NewExecutorWithRegistry(&hooks.Config{
				SessionStart: []hooks.Hook{hook},
				TurnStart:    []hooks.Hook{hook},
			}, "/workspace", nil, r)

			for _, id := range []string{"session-1", "session-2"} {
				result, err := exec.Dispatch(t.Context(), event, &hooks.Input{SessionID: id})
				require.NoError(t, err)
				assert.True(t, result.Allowed, "rendered text must not be interpreted as a hook decision")
				assert.Equal(t, id+" / "+string(event)+" / /workspace\n"+`{"decision":"block"}`, result.AdditionalContext)
			}
		})
	}
}
