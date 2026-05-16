package toolexec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestParseToolInput_EmptyString(t *testing.T) {
	assert.Nil(t, ParseToolInput(""))
}

func TestParseToolInput_InvalidJSON(t *testing.T) {
	assert.Nil(t, ParseToolInput("{not json"))
}

func TestParseToolInput_ValidJSON(t *testing.T) {
	got := ParseToolInput(`{"path":"a.txt","mode":"r"}`)
	assert.Equal(t, map[string]any{"path": "a.txt", "mode": "r"}, got)
}

func TestParseToolInput_EmptyObject(t *testing.T) {
	got := ParseToolInput(`{}`)
	assert.Equal(t, map[string]any{}, got)
}

func TestNewHooksInput_PopulatesFields(t *testing.T) {
	sess := session.New()
	tc := tools.ToolCall{
		ID: "call_42",
		Function: tools.FunctionCall{
			Name:      "read_file",
			Arguments: `{"path":"a.txt"}`,
		},
	}

	in := NewHooksInput(sess, tc)
	require.NotNil(t, in)
	assert.Equal(t, sess.ID, in.SessionID)
	assert.Equal(t, "read_file", in.ToolName)
	assert.Equal(t, "call_42", in.ToolUseID)
	assert.Equal(t, map[string]any{"path": "a.txt"}, in.ToolInput)
}

func TestNewHooksInput_InvalidArgumentsYieldsNilToolInput(t *testing.T) {
	sess := session.New()
	tc := tools.ToolCall{
		ID: "call_43",
		Function: tools.FunctionCall{
			Name:      "noop",
			Arguments: "",
		},
	}

	in := NewHooksInput(sess, tc)
	require.NotNil(t, in)
	assert.Nil(t, in.ToolInput)
}
