package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadTeamRequest_RoundTrip(t *testing.T) {
	t.Parallel()

	in := LoadTeamRequest{
		ModelOverrides: []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet-20240620"},
		PromptFiles:    []string{"prompts/role.md", "prompts/tone.md"},
	}

	data, err := json.Marshal(in)
	require.NoError(t, err)

	var out LoadTeamRequest
	require.NoError(t, json.Unmarshal(data, &out))

	assert.Equal(t, in.ModelOverrides, out.ModelOverrides)
	assert.Equal(t, in.PromptFiles, out.PromptFiles)
}

func TestLoadTeamRequest_OmitsEmptyFields(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(LoadTeamRequest{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(data))
}

func TestLoadTeamRequest_NonSerializableFieldsExcluded(t *testing.T) {
	t.Parallel()

	// Source and RunConfig are intentionally json:"-" because they don't
	// have a wire form yet. This test pins that contract so a future
	// edit that adds a tag without designing a wire shape gets caught.
	data, err := json.Marshal(LoadTeamRequest{
		ModelOverrides: []string{"x"},
	})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "source")
	assert.NotContains(t, string(data), "run_config")
}

func TestCreateSessionRequest_RoundTrip(t *testing.T) {
	t.Parallel()

	in := CreateSessionRequest{
		AgentName:        "main",
		ToolsApproved:    true,
		HideToolResults:  true,
		SessionDB:        "/tmp/sessions.db",
		ResumeSessionID:  "01HF8...",
		SnapshotsEnabled: true,
		WorkingDir:       "/tmp/work",
	}

	data, err := json.Marshal(in)
	require.NoError(t, err)

	var out CreateSessionRequest
	require.NoError(t, json.Unmarshal(data, &out))

	assert.Equal(t, in, out)
}

func TestCreateSessionRequest_OmitsEmptyFields(t *testing.T) {
	t.Parallel()

	// Boolean fields are always serialized (no `omitempty`) so that an
	// explicit `false` survives the wire round-trip and stays distinct
	// from "unset" semantics owned by the server. String fields with
	// `omitempty` still drop out of the empty case.
	data, err := json.Marshal(CreateSessionRequest{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"tools_approved":false,"hide_tool_results":false,"snapshots_enabled":false}`, string(data))
}

func TestCreateSessionRequest_PreservesExplicitFalseOnTheWire(t *testing.T) {
	t.Parallel()

	// Pins the contract that `omitempty` on booleans is intentionally
	// absent: a client sending an explicit `false` must reach the
	// server as `false`, not as the field's absence. If the server's
	// default ever flips to `true`, this guarantee keeps existing
	// clients' "no, really, false" intact.
	in := CreateSessionRequest{
		AgentName:        "main",
		ToolsApproved:    false,
		HideToolResults:  false,
		SnapshotsEnabled: false,
	}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"tools_approved":false`)
	assert.Contains(t, string(data), `"hide_tool_results":false`)
	assert.Contains(t, string(data), `"snapshots_enabled":false`)
}

func TestCreateSessionRequest_NonSerializableFieldsExcluded(t *testing.T) {
	t.Parallel()

	// GlobalPermissions has no wire form yet.
	data, err := json.Marshal(CreateSessionRequest{AgentName: "x"})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "global_permissions")
}
