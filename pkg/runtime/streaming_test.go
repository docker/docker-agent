package runtime

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// AddToolCallWithStop appends a single chunk that carries BOTH a complete tool
// call AND a terminal finish_reason ("stop"), the way LiteLLM/Gemini emit a
// function call atomically. The OpenAI-native streaming protocol never does
// this (the tool call deltas and the terminal finish_reason live in separate
// chunks), which is why this case was never exercised before.
func (b *streamBuilder) AddToolCallWithStop(id, name, args string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: chat.FinishReasonStop,
			Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
				ID:       id,
				Type:     "function",
				Function: tools.FunctionCall{Name: name, Arguments: args},
			}}},
		}},
		Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1},
	})
	return b
}

// AddRefusal appends a terminal chunk carrying finish_reason "refusal", the
// way the Anthropic adapter surfaces a safety-classifier refusal (HTTP 200,
// no content).
func (b *streamBuilder) AddRefusal() *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: chat.FinishReasonRefusal,
		}},
		Usage: &chat.Usage{InputTokens: 1},
	})
	return b
}

// TestHandleStream_Refusal verifies that a refusal terminates the stream with
// the refusal finish reason and stops the loop instead of being mistaken for a
// normal empty completion.
func TestHandleStream_Refusal(t *testing.T) {
	stream := newStreamBuilder().
		AddRefusal().
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh),
	)
	require.NoError(t, err)

	assert.Equal(t, chat.FinishReasonRefusal, res.FinishReason)
	assert.True(t, res.Stopped, "a refusal ends the turn")
	assert.Empty(t, res.Calls)
	require.NotNil(t, res.Usage)
}

// TestHandleStream_RefusalDropsPartialToolCalls verifies that tool calls
// streamed before the safety classifier ends the turn with "refusal" are NOT
// executed: the refusal voids the whole turn.
func TestHandleStream_RefusalDropsPartialToolCalls(t *testing.T) {
	stream := newStreamBuilder().
		AddToolCallName("call_1", "rm_rf").
		AddToolCallArguments("call_1", `{"path":"/"}`).
		AddRefusal().
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh),
	)
	require.NoError(t, err)

	assert.Equal(t, chat.FinishReasonRefusal, res.FinishReason)
	assert.Empty(t, res.Calls, "tool calls from a refused turn must not be executed")
	assert.True(t, res.Stopped, "a refusal ends the turn")
}

// TestHandleStream_ToolCallAndStopInSameChunk reproduces the LiteLLM/Gemini bug
// where a subagent's tool call is silently dropped because the provider packs
// the tool call and finish_reason:"stop" into the same streaming chunk. The
// dropped tool call leaves the assistant message empty, which surfaces upstream
// as "No response from agent".
func TestHandleStream_ToolCallAndStopInSameChunk(t *testing.T) {
	stream := newStreamBuilder().
		AddToolCallWithStop("call_1", "company_search", `{"query":"x"}`).
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64) // buffered so handleStream never blocks on Emit
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh),
	)
	require.NoError(t, err)

	require.Len(t, res.Calls, 1, "the tool call from the terminal chunk must not be dropped")
	assert.Equal(t, "company_search", res.Calls[0].Function.Name)
	assert.JSONEq(t, `{"query":"x"}`, res.Calls[0].Function.Arguments)
	assert.Equal(t, chat.FinishReasonToolCalls, res.FinishReason)
	assert.False(t, res.Stopped, "must not stop: a tool call is pending execution")
}

// TestHandleStream_ToolCallThenSeparateStop is the OpenAI-native shape: the tool
// call deltas arrive first, then a separate terminal chunk carries the finish
// reason. This already works today and guards against a regression when fixing
// the same-chunk case above.
func TestHandleStream_ToolCallThenSeparateStop(t *testing.T) {
	stream := newStreamBuilder().
		AddToolCallName("call_1", "company_search").
		AddToolCallArguments("call_1", `{"query":"x"}`).
		AddStopWithUsage(1, 1).
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh),
	)
	require.NoError(t, err)

	require.Len(t, res.Calls, 1)
	assert.Equal(t, "company_search", res.Calls[0].Function.Name)
	assert.JSONEq(t, `{"query":"x"}`, res.Calls[0].Function.Arguments)
	assert.Equal(t, chat.FinishReasonToolCalls, res.FinishReason)
	assert.False(t, res.Stopped)
}

// stalledStream is a chat.MessageStream that blocks in Recv() until
// either unblocked or the stream is closed. It is used to simulate a
// half-open TCP connection where the remote side stops sending data.
type stalledStream struct {
	// unblock is closed (or sent on) to release a blocked Recv call.
	unblock chan struct{}
	// closed is set when Close is called.
	closed bool
}

func newStalledStream() *stalledStream {
	return &stalledStream{unblock: make(chan struct{})}
}

// Recv blocks until unblock is closed, then returns io.EOF.
func (s *stalledStream) Recv() (chat.MessageStreamResponse, error) {
	<-s.unblock
	return chat.MessageStreamResponse{}, io.EOF
}

func (s *stalledStream) Close() {
	if !s.closed {
		s.closed = true
		close(s.unblock)
	}
}

// withShortStreamIdleTimeout temporarily overrides defaultStreamIdleTimeout
// to shorten it for tests. Restores the original value via t.Cleanup.
func withShortStreamIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := defaultStreamIdleTimeout
	defaultStreamIdleTimeout = d
	t.Cleanup(func() { defaultStreamIdleTimeout = orig })
}

// TestHandleStream_IdleTimeout verifies that handleStream returns an error
// wrapping errStreamIdle when no SSE chunk arrives within the idle window.
// It also checks that the provided cancelStream function is called so the
// HTTP transport can close the underlying TCP connection.
func TestHandleStream_IdleTimeout(t *testing.T) {
	withShortStreamIdleTimeout(t, 50*time.Millisecond)

	stream := newStalledStream()
	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	cancelCalled := false
	cancelStream := func(cause error) {
		cancelCalled = true
		stream.Close() // unblock the stalled Recv so the reader goroutine can exit
	}

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), cancelStream, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh),
	)

	require.Error(t, err)
	require.ErrorIs(t, err, errStreamIdle, "error must wrap errStreamIdle")
	assert.True(t, res.Stopped)
	assert.True(t, cancelCalled, "cancelStream must be called on idle timeout")
}

// TestHandleStream_ContextCancellation verifies that handleStream returns
// promptly when the caller's context is cancelled, even while a Recv call
// is blocked. This covers the SIGTERM / graceful-shutdown path.
func TestHandleStream_ContextCancellation(t *testing.T) {
	// Use a long idle timeout so only context cancellation can trigger.
	withShortStreamIdleTimeout(t, 10*time.Minute)

	stream := newStalledStream()
	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	ctx, cancel := context.WithCancel(t.Context())

	// Cancel the context shortly after handleStream starts.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
		stream.Close() // unblock the stalled Recv so the reader goroutine can exit
	}()

	evCh := make(chan Event, 64)
	_, cancelStream := context.WithCancelCause(ctx)
	res, err := handleStream(
		ctx, cancelStream, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh),
	)

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "error must be context.Canceled")
	assert.True(t, res.Stopped)
}
