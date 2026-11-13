package toolexec_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestDispatcherPersistsToolResultMeta(t *testing.T) {
	t.Parallel()
	meta, err := filesystem.NewEditFileMeta("file.txt", "before\n", "after\n")
	require.NoError(t, err)
	sess := session.New()
	sess.ToolsApproved = true
	tool := tools.Tool{Name: "edit_file", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		return &tools.ToolCallResult{Output: "edited", Meta: meta}, nil
	}}
	a := newAgent()
	dispatcher := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }}
	dispatcher.Process(t.Context(), sess, []tools.ToolCall{{ID: "edit", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{}`}}}, []tools.Tool{tool}, &captureEmitter{})
	require.Len(t, sess.Messages, 1)
	assert.Equal(t, meta, sess.Messages[0].Message.Message.ToolResultMeta)
	data, err := json.Marshal(sess)
	require.NoError(t, err)
	var restored session.Session
	require.NoError(t, json.Unmarshal(data, &restored))
	decoded, ok := filesystem.DecodeEditFileMeta(restored.Messages[0].Message.Message.ToolResultMeta)
	require.True(t, ok)
	assert.Equal(t, meta, decoded)
}
