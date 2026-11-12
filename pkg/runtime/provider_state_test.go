package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func testProviderState() *chat.ProviderState {
	return &chat.ProviderState{
		Provider:  "anthropic",
		MessageID: "msg_1",
		Content:   json.RawMessage(`[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"Hello"}]`),
	}
}

// AddStopWithProviderState appends a terminal chunk carrying the provider's
// raw response, the way the Anthropic adapters do on message_stop.
func (b *streamBuilder) AddStopWithProviderState(finishReason chat.FinishReason, state *chat.ProviderState) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: finishReason,
			Delta:        chat.MessageDelta{ProviderState: state},
		}},
		Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1},
	})
	return b
}

func runHandleStream(t *testing.T, stream *mockStream) streamResult {
	t.Helper()
	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess,
		NewChannelSink(make(chan Event, 64)), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)
	return res
}

func TestHandleStream_ProviderStatePropagates(t *testing.T) {
	t.Parallel()

	t.Run("on terminal chunk", func(t *testing.T) {
		t.Parallel()
		res := runHandleStream(t, newStreamBuilder().
			AddContent("Hello").
			AddStopWithProviderState(chat.FinishReasonStop, testProviderState()).
			Build())
		require.NotNil(t, res.ProviderState)
		assert.Equal(t, "msg_1", res.ProviderState.MessageID)
	})

	t.Run("with tool calls then bare EOF", func(t *testing.T) {
		t.Parallel()
		res := runHandleStream(t, newStreamBuilder().
			AddToolCallName("toolu_1", "read_file").
			AddToolCallArguments("toolu_1", `{}`).
			AddStopWithProviderState(chat.FinishReasonToolCalls, testProviderState()).
			Build())
		require.Len(t, res.Calls, 1)
		require.NotNil(t, res.ProviderState)
	})

	t.Run("kept on a length turn that retracts nothing", func(t *testing.T) {
		t.Parallel()
		res := runHandleStream(t, newStreamBuilder().
			AddThinkingSignature("sig-signed").
			AddContent("partial answer").
			AddStopWithProviderState(chat.FinishReasonLength, testProviderState()).
			Build())

		assert.Equal(t, chat.FinishReasonLength, res.FinishReason)
		assert.True(t, res.Stopped)
		require.NotNil(t, res.ProviderState,
			"a length turn that retracts no calls keeps the raw response the next request replays")
		assert.Equal(t, "sig-signed", res.ThinkingSignature)
	})

	t.Run("dropped with the calls it retracts on a length turn", func(t *testing.T) {
		t.Parallel()
		res := runHandleStream(t, newStreamBuilder().
			AddThinkingSignature("sig-signed").
			AddToolCallName("toolu_1", "read_file").
			AddToolCallArguments("toolu_1", `{"pa`).
			AddStopWithProviderState(chat.FinishReasonLength, testProviderState()).
			Build())

		assert.Equal(t, chat.FinishReasonLength, res.FinishReason)
		assert.Empty(t, res.Calls)
		assert.Nil(t, res.ProviderState, "the retracted tool_use blocks live in the raw response")
		assert.Empty(t, res.ThinkingSignature,
			"the kept signature no longer describes the flattened thinking")
	})

	t.Run("dropped on refusal", func(t *testing.T) {
		t.Parallel()
		res := runHandleStream(t, newStreamBuilder().
			AddToolCallName("toolu_1", "rm_rf").
			AddStopWithProviderState(chat.FinishReasonRefusal, testProviderState()).
			Build())
		assert.Equal(t, chat.FinishReasonRefusal, res.FinishReason)
		assert.Empty(t, res.Calls)
		assert.Nil(t, res.ProviderState, "raw response still holds the refused tool_use blocks")
	})

	t.Run("dropped on XML tool call fallback", func(t *testing.T) {
		t.Parallel()
		res := runHandleStream(t, newStreamBuilder().
			AddContent(`<tool_call>{"name":"read_file","arguments":{}}</tool_call>`).
			AddStopWithProviderState(chat.FinishReasonStop, testProviderState()).
			Build())
		require.Len(t, res.Calls, 1)
		assert.Nil(t, res.ProviderState, "raw response has no tool_use block for the extracted call")
	})
}

func TestRecordAssistantMessage_SealsProviderState(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddReasoning("t").
		AddContent("Hello").
		AddStopWithProviderState(chat.FinishReasonStop, testProviderState()).
		Build()
	sess := session.New(session.WithUserMessage("Hi"))
	runSession(t, sess, stream)

	msgs := sess.GetAllMessages()
	last := msgs[len(msgs)-1].Message
	require.Equal(t, chat.MessageRoleAssistant, last.Role)
	require.NotNil(t, last.ProviderState)
	assert.Equal(t, last.VisibleContentHash(), last.ProviderState.ContentHash)
	assert.NotNil(t, last.ReplayableProviderState("anthropic"))

	// Persistence round trip keeps the state replayable; a later edit does not.
	data, err := json.Marshal(last)
	require.NoError(t, err)
	var restored chat.Message
	require.NoError(t, json.Unmarshal(data, &restored))
	assert.NotNil(t, restored.ReplayableProviderState("anthropic"))
	restored.Content = "Hello, edited"
	assert.Nil(t, restored.ReplayableProviderState("anthropic"))
}

func TestRecordAssistantMessage_SealsAfterMedia(t *testing.T) {
	store := session.NewInMemorySessionStore()
	stream := &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: "Hello", Media: []chat.MediaDelta{{Data: []byte("png"), MimeType: "image/png", Name: "cat.png", Size: 3}}}}}},
	}}
	stream.responses = append(stream.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop, Delta: chat.MessageDelta{ProviderState: testProviderState()}}},
		Usage:   &chat.Usage{InputTokens: 1, OutputTokens: 1},
	})
	root := agent.New("root", "instructions", agent.WithModel(&mockProvider{id: "test/media", stream: stream}))
	rt, err := New(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithSessionStore(store), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	sess := session.New(session.WithUserMessage("draw a cat"))
	sess.WorkingDir = t.TempDir()
	for range rt.RunStream(t.Context(), sess) {
	}

	stored, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	messages := stored.GetAllMessages()
	require.Len(t, messages, 2)
	assistant := messages[1].Message
	require.NotEmpty(t, assistant.MultiContent, "media is attached")
	assert.NotNil(t, assistant.ReplayableProviderState("anthropic"), "the seal covers the media parts")
}
