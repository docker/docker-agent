package toolexec_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestDispatcherRedactsInlineTextToolDocuments pins that the
// tool_response_transform rewrite also covers the text of inline tool
// attachments. MCP servers return text resources as
// tools.DocumentContent{Text: ...}; those documents are emitted to event
// subscribers and persisted in the session by recordToolResponse, so a
// transform that only rewrites the textual output leaves the secret in
// both places.
func TestDispatcherRedactsInlineTextToolDocuments(t *testing.T) {
	t.Parallel()

	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	reg := hooks.NewRegistry()
	require.NoError(t, builtins.Register(reg))
	require.NoError(t, reg.RegisterBuiltin("observe", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return nil, nil
	}))
	cfg := &hooks.Config{
		ToolResponseTransform: []hooks.MatcherConfig{{Matcher: "*", Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.RedactSecrets}}}},
		PostToolUse:           []hooks.MatcherConfig{{Matcher: "*", Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "observe"}}}},
	}
	hd := &postCapturingDispatcher{execHookDispatcher: &execHookDispatcher{exec: hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, reg)}}
	sess := session.New()
	sess.ToolsApproved = true
	a := newAgent()
	tool := tools.Tool{Name: "fetch", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		return &tools.ToolCallResult{
			Output: "fetched resource",
			Documents: []tools.DocumentContent{
				{Name: "notes.md", URI: "mcp://docs/notes.md", MimeType: "text/markdown", Text: "token=" + secret},
				{Name: "clean.md", MimeType: "text/markdown", Text: "nothing to see"},
				{Name: "report.pdf", MimeType: "application/pdf", Data: "cGRm"},
			},
		}, nil
	}}
	emitter := &metaEmitter{}
	dispatcher := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }, Hooks: hd}
	dispatcher.Process(t.Context(), sess, []tools.ToolCall{{ID: "fetch", Function: tools.FunctionCall{Name: "fetch", Arguments: `{}`}}}, []tools.Tool{tool}, emitter)

	require.NotNil(t, emitter.result)
	require.Len(t, emitter.result.Documents, 3)

	// The emitted result feeds SSE / ACP / TUI consumers.
	event, err := json.Marshal(emitter.result)
	require.NoError(t, err)
	assert.NotContains(t, string(event), secret, "emitted attachment text must be scrubbed")
	assert.Contains(t, string(event), portcullis.Marker)

	// The persisted session message feeds every later turn and export.
	stored, err := json.Marshal(sess)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), secret, "persisted attachment text must be scrubbed")
	assert.Contains(t, string(stored), portcullis.Marker)

	// Only secret-bearing text is rewritten; clean and binary documents
	// must survive byte-for-byte.
	assert.Equal(t, "nothing to see", emitter.result.Documents[1].Text)
	assert.Equal(t, "cGRm", emitter.result.Documents[2].Data)

	msg := lastToolMessage(t, sess)
	require.Len(t, msg.MultiContent, 4)
	require.NotNil(t, msg.MultiContent[1].Document)
	assert.NotContains(t, msg.MultiContent[1].Document.Source.InlineText, secret)
	assert.Contains(t, msg.MultiContent[1].Document.Source.InlineText, portcullis.Marker)

	// post_tool_use sees the already-rewritten attachments, matching the
	// documented "post_tool_use receives the transformed result" contract.
	require.NotNil(t, hd.postInput)
	require.Len(t, hd.postInput.ToolDocuments, 3)
	assert.NotContains(t, hd.postInput.ToolDocuments[0].Text, secret)
	assert.Equal(t, "nothing to see", hd.postInput.ToolDocuments[1].Text)
}

// postCapturingDispatcher records the post_tool_use input so tests can
// assert the hook sees the already-rewritten attachments.
type postCapturingDispatcher struct {
	*execHookDispatcher

	postInput *hooks.Input
}

func (d *postCapturingDispatcher) Dispatch(ctx context.Context, a *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result {
	if event == hooks.EventPostToolUse {
		d.postInput = in
	}
	return d.execHookDispatcher.Dispatch(ctx, a, event, in)
}

func lastToolMessage(t *testing.T, sess *session.Session) chat.Message {
	t.Helper()
	require.NotEmpty(t, sess.Messages)
	item := sess.Messages[len(sess.Messages)-1]
	require.NotNil(t, item.Message)
	msg := item.Message.Message
	require.Equal(t, chat.MessageRoleTool, msg.Role)
	return msg
}
