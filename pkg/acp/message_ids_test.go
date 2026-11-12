package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestMessageIDsLivePersistenceAndReplay(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	prov := &mockProvider{id: modelsdev.NewID("test", "ids"), stream: &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ReasoningContent: "think "}}}},
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ReasoningContent: "more", Content: "hello "}}}},
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "world"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}}
	a := NewAgent(config.NewBytesSource("test", nil), &config.RuntimeConfig{}, store)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov))))}, nil
	}
	_, err = a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.NoError(t, err)
	out := &captureWriter{}
	captureReplay(t, a, out)
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	req := acpsdk.PromptRequest{SessionId: created.SessionId, MessageId: new("draft-client-id"), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("user")}}
	response, err := a.Prompt(t.Context(), req)
	require.NoError(t, err)
	assert.Nil(t, response.UserMessageId)
	var liveText, liveThought string
	for _, update := range replayUpdates(t, out, string(created.SessionId)) {
		if chunk := update.AgentMessageChunk; chunk != nil {
			require.NotNil(t, chunk.MessageId)
			if liveText == "" {
				liveText = *chunk.MessageId
			}
			assert.Equal(t, liveText, *chunk.MessageId)
		}
		if chunk := update.AgentThoughtChunk; chunk != nil {
			require.NotNil(t, chunk.MessageId)
			if liveThought == "" {
				liveThought = *chunk.MessageId
			}
			assert.Equal(t, liveThought, *chunk.MessageId)
		}
	}
	require.NotEmpty(t, liveText)
	require.NotEmpty(t, liveThought)
	assert.NotEqual(t, liveText, liveThought)
	saved, err := store.GetSession(t.Context(), string(created.SessionId))
	require.NoError(t, err)
	var userID string
	for _, msg := range saved.OwnMessages() {
		if msg.Message.Role == chat.MessageRoleUser {
			require.NotEmpty(t, msg.Message.MessageID)
			assert.NotEqual(t, "draft-client-id", msg.Message.MessageID)
			userID = messageDisplayID(saved.ID, msg.Message.MessageID, "content")
		}
		if msg.Message.Role == chat.MessageRoleAssistant {
			assert.Equal(t, liveText, messageDisplayID(saved.ID, msg.Message.MessageID, "content"))
			assert.Equal(t, liveThought, messageDisplayID(saved.ID, msg.Message.MessageID, "thought"))
		}
	}
	for _, cold := range []bool{false, true} {
		if cold {
			_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
			require.NoError(t, err)
		}
		replayOut := &captureWriter{}
		captureReplay(t, a, replayOut)
		_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: created.SessionId, Cwd: saved.WorkingDir})
		require.NoError(t, err)
		var seenUser, seenText, seenThought bool
		for _, update := range replayUpdates(t, replayOut, saved.ID) {
			if c := update.UserMessageChunk; c != nil {
				require.NotNil(t, c.MessageId)
				assert.Equal(t, userID, *c.MessageId)
				seenUser = true
			}
			if c := update.AgentMessageChunk; c != nil {
				assert.Equal(t, liveText, *c.MessageId)
				seenText = true
			}
			if c := update.AgentThoughtChunk; c != nil {
				assert.Equal(t, liveThought, *c.MessageId)
				seenThought = true
			}
		}
		assert.True(t, seenUser && seenText && seenThought)
	}
}

func TestMessageIDsLegacyNestedAndRichReplay(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	rich := session.UserMessage("", chat.MessagePart{Type: chat.MessagePartTypeText, Text: strings.Repeat("x", replayTextChunkBytes+1)}, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "audio", MimeType: "audio/wav", Source: chat.DocumentSource{InlineData: []byte{1, 2}}}})
	rich.Message.MessageID = "logical"
	s.sess.AddMessage(rich)
	s.sess.AddMessage(session.UserMessage("legacy"))
	child := session.New()
	child.AddMessage(rich)
	child.AddMessage(session.UserMessage("legacy"))
	s.sess.AddSubSession(child)
	var baseline []string
	for range 2 {
		out := &captureWriter{}
		captureReplay(t, a, out)
		require.NoError(t, a.replayHistory(t.Context(), s, s.sess.Clone()))
		var ids []string
		for _, u := range replayUpdates(t, out, s.id) {
			require.NotNil(t, u.UserMessageChunk)
			require.NotNil(t, u.UserMessageChunk.MessageId)
			ids = append(ids, *u.UserMessageChunk.MessageId)
		}
		require.Len(t, ids, 8)
		assert.Equal(t, ids[0], ids[1])
		assert.Equal(t, ids[0], ids[2])
		assert.NotEqual(t, ids[0], ids[3])
		assert.NotEqual(t, ids[0], ids[4])
		assert.NotEqual(t, ids[3], ids[7])
		if baseline == nil {
			baseline = ids
		} else {
			assert.Equal(t, baseline, ids)
		}
	}
}

func TestMessageIDsCacheHitsAreVisibleAndDistinct(t *testing.T) {
	t.Parallel()
	c, err := cache.New(cache.Config{Enabled: true})
	require.NoError(t, err)
	c.Store("question", "cached answer")
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "cache")}), agent.WithCache(c))))}, nil
	}
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	var ids []string
	for range 2 {
		out := &captureWriter{}
		captureReplay(t, a, out)
		_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("question")}})
		require.NoError(t, err)
		var count int
		for _, u := range replayUpdates(t, out, string(created.SessionId)) {
			if c := u.AgentMessageChunk; c != nil {
				assert.Equal(t, "cached answer", c.Content.Text.Text)
				require.NotNil(t, c.MessageId)
				ids = append(ids, *c.MessageId)
				count++
			}
		}
		assert.Equal(t, 1, count)
	}
	require.Len(t, ids, 2)
	assert.NotEqual(t, ids[0], ids[1])
}

func TestToolNameWriterBoundaries(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"session/update", "session/request_permission"} {
		for _, kind := range []string{"tool_call", "tool_call_update"} {
			call := map[string]any{"sessionUpdate": kind, "toolCallId": "opaque", "title": "Read file", "_meta": map[string]any{toolNameKey: "read_file", "other": "kept"}, "rawInput": json.RawMessage(`{"large":9007199254740993,"nested":{"_meta":{"` + toolNameKey + `":"untouched"}}}`)}
			key := "update"
			if method == "session/request_permission" {
				key = "toolCall"
				delete(call, "sessionUpdate")
			}
			input, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{key: call}})
			require.NoError(t, err)
			var out bytes.Buffer
			n, err := (&toolNameWriter{output: &out}).Write(input)
			require.NoError(t, err)
			assert.Equal(t, len(input), n)
			assert.Contains(t, out.String(), `"name":"read_file"`)
			assert.Contains(t, out.String(), `9007199254740993`)
			assert.Contains(t, out.String(), `"other":"kept"`)
			assert.Contains(t, out.String(), `"`+toolNameKey+`":"untouched"`)
		}
	}
	for _, input := range []string{`{"method":"other","params":{"_meta":{"` + toolNameKey + `":"x"}}}`, `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","_meta":{"` + toolNameKey + `":"x"}}}}`} {
		var out bytes.Buffer
		n, err := (&toolNameWriter{output: &out}).Write([]byte(input))
		require.NoError(t, err)
		assert.Equal(t, len(input), n)
		assert.Equal(t, input, out.String())
	}
	input := []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","name":"conflict","_meta":{"` + toolNameKey + `":"x"}}}}`)
	var out bytes.Buffer
	_, err := (&toolNameWriter{output: &out}).Write(input)
	require.Error(t, err)
	assert.Empty(t, out.String())
	input = []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","_meta":{"` + toolNameKey + `":"x"}}}}`)
	_, err = (&toolNameWriter{output: shortToolNameWriter{}}).Write(input)
	require.ErrorIs(t, err, io.ErrShortWrite)
}

type shortToolNameWriter struct{}

func (shortToolNameWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestToolNamesLiveReplayAndPermissionWire(t *testing.T) {
	t.Parallel()
	out := &captureWriter{}
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	input, send := io.Pipe()
	conn := a.NewConnection(out, input)
	t.Cleanup(func() { _ = send.Close(); <-conn.Done() })
	s := &Session{id: "session", sess: session.New()}
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "acp_generation_alias", Arguments: `{"value":1}`}}
	definition := tools.Tool{Name: call.Function.Name, Annotations: tools.ToolAnnotations{Title: "Human title"}}
	var tracker toolCallTracker
	_, err := tracker.report(t.Context(), a, s, "root", call, definition, acpsdk.ToolCallStatusPending)
	require.NoError(t, err)
	_, err = tracker.report(t.Context(), a, s, "root", call, definition, acpsdk.ToolCallStatusInProgress)
	require.NoError(t, err)
	require.NoError(t, tracker.complete(t.Context(), a, s, runtime.ToolCallResponse(call.ID, definition, tools.ResultSuccess("ok"), "ok", "root").(*runtime.ToolCallResponseEvent)))
	call.ID = "interrupted"
	_, err = tracker.report(t.Context(), a, s, "root", call, definition, acpsdk.ToolCallStatusInProgress)
	require.NoError(t, err)
	require.NoError(t, tracker.interrupt(t.Context(), a, s))
	require.NoError(t, tracker.complete(t.Context(), a, s, runtime.ToolCallResponse("orphan", definition, tools.ResultSuccess("ok"), "ok", "root").(*runtime.ToolCallResponseEvent)))
	for _, line := range out.lines() {
		assert.Contains(t, line, `"name":"acp_generation_alias"`)
		assert.NotContains(t, line, toolNameKey)
	}
	require.Len(t, out.lines(), 6)
	permission := buildToolCallUpdate(call, definition, acpsdk.ToolCallStatusPending, "")
	assert.Equal(t, "acp_generation_alias", permission.Meta[toolNameKey], "direct SDK embedders retain metadata fallback")
	wire, err := json.Marshal(map[string]any{"method": "session/request_permission", "params": acpsdk.RequestPermissionRequest{SessionId: "session", ToolCall: permission}})
	require.NoError(t, err)
	var written bytes.Buffer
	_, err = (&toolNameWriter{output: &written}).Write(wire)
	require.NoError(t, err)
	assert.Contains(t, written.String(), `"name":"acp_generation_alias"`)
	assert.Contains(t, written.String(), `"title":"Human title"`)
	history := session.New()
	call.Function.Arguments = `{"private":"never replay"}`
	history.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}))
	history.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: "safe"}))
	before := len(out.lines())
	require.NoError(t, a.replayHistory(t.Context(), s, history))
	for _, line := range out.lines()[before:] {
		assert.Contains(t, line, `"name":"acp_generation_alias"`)
		assert.NotContains(t, line, "private")
		assert.NotContains(t, line, "rawInput")
	}
	for _, bad := range []string{"", strings.Repeat("a", 1025), "bad\nname", string([]byte{255})} {
		assert.Nil(t, toolNameMeta(bad))
	}
}
