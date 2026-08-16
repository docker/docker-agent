package runtime

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// AddMedia appends a chunk carrying a generated-media delta (e.g. an inline
// image blob), the way the Gemini adapter surfaces InlineData parts. name may
// be empty to exercise the provider-omits-a-name path.
func (b *streamBuilder) AddMedia(data []byte, mimeType, name string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{Media: []chat.MediaDelta{{
				Data:     data,
				MimeType: mimeType,
				Name:     name,
				Size:     int64(len(data)),
			}}},
		}},
	})
	return b
}

// AddMultiMedia appends a SINGLE chunk carrying multiple generated-media
// blobs at once, the way Gemini can pack several inline parts (across parts
// or candidates) into one chunk.
func (b *streamBuilder) AddMultiMedia(blobs ...chat.MediaDelta) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{Media: blobs},
		}},
	})
	return b
}

// AddMediaWithStop appends a SINGLE terminal chunk carrying both a
// generated-media blob and a terminal finish_reason, the way a provider can
// pack the final image and "stop" into one chunk.
func (b *streamBuilder) AddMediaWithStop(data []byte, mimeType, name string, finishReason chat.FinishReason) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index:        0,
			FinishReason: finishReason,
			Delta: chat.MessageDelta{Media: []chat.MediaDelta{{
				Data:     data,
				MimeType: mimeType,
				Name:     name,
				Size:     int64(len(data)),
			}}},
		}},
		Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1},
	})
	return b
}

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
	t.Parallel()

	stream := newStreamBuilder().
		AddRefusal().
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
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
	t.Parallel()

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
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
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
	t.Parallel()

	stream := newStreamBuilder().
		AddToolCallWithStop("call_1", "company_search", `{"query":"x"}`).
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64) // buffered so handleStream never blocks on Emit
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
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
	t.Parallel()

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
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)

	require.Len(t, res.Calls, 1)
	assert.Equal(t, "company_search", res.Calls[0].Function.Name)
	assert.JSONEq(t, `{"query":"x"}`, res.Calls[0].Function.Arguments)
	assert.Equal(t, chat.FinishReasonToolCalls, res.FinishReason)
	assert.False(t, res.Stopped)
}

// TestHandleStream_MediaAccumulatesAlongsideText verifies that a
// generated-media delta streamed alongside text is accumulated into
// streamResult.Media without disturbing the existing text/finish-reason
// handling.
func TestHandleStream_MediaAccumulatesAlongsideText(t *testing.T) {
	t.Parallel()

	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	stream := newStreamBuilder().
		AddContent("here is your image").
		AddMedia(imgBytes, "image/png", "cat.png").
		AddStopWithUsage(1, 1).
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)

	assert.Equal(t, "here is your image", res.Content, "text must survive alongside media")
	require.Len(t, res.Media, 1)
	assert.Equal(t, imgBytes, res.Media[0].Data)
	assert.Equal(t, "image/png", res.Media[0].MimeType)
	assert.Equal(t, "cat.png", res.Media[0].Name)
	assert.Equal(t, chat.FinishReasonStop, res.FinishReason)
	assert.True(t, res.Stopped)
}

// TestHandleStream_MediaOnlyTurnNotTreatedAsEmpty is a regression test: a
// turn that streams ONLY a generated image (no text, no tool calls) and
// ends with a bare EOF must not be misclassified as the "no output" stall
// case — it is a normal completion and must report Stopped=true (turn
// ends) without going through the no-output warning path.
func TestHandleStream_MediaOnlyTurnNotTreatedAsEmpty(t *testing.T) {
	t.Parallel()

	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	stream := newStreamBuilder().
		AddMedia(imgBytes, "image/png", "").
		Build() // no terminal chunk: bare EOF, no finish reason

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)

	assert.Empty(t, res.Content)
	require.Len(t, res.Media, 1, "the generated image must be accumulated")
	assert.True(t, res.Stopped, "a media-only turn is a normal completion, not a stall")
}

// TestHandleStream_MultipleMediaBlobsInOneChunk verifies that every inline
// blob a provider packs into a SINGLE chunk is retained, not just the last
// one — a provider (Gemini in particular) can return more than one
// generated image across parts/candidates in the same streaming chunk.
func TestHandleStream_MultipleMediaBlobsInOneChunk(t *testing.T) {
	t.Parallel()

	blob1 := chat.MediaDelta{Data: []byte{0x01}, MimeType: "image/png", Name: "one.png", Size: 1}
	blob2 := chat.MediaDelta{Data: []byte{0x02}, MimeType: "image/jpeg", Name: "two.jpg", Size: 1}
	blob3 := chat.MediaDelta{Data: []byte{0x03}, MimeType: "image/webp", Name: "three.webp", Size: 1}

	stream := newStreamBuilder().
		AddMultiMedia(blob1, blob2, blob3).
		AddStopWithUsage(1, 1).
		Build()

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)

	require.Len(t, res.Media, 3, "every blob in the chunk must be retained, not just the last one")
	assert.Equal(t, blob1, res.Media[0])
	assert.Equal(t, blob2, res.Media[1])
	assert.Equal(t, blob3, res.Media[2])
}

// TestHandleStream_MediaInTerminalChunkIsAccumulated verifies that a
// generated-media blob packed into the SAME chunk as a terminal finish
// reason ("stop", "length", or "refusal") is accumulated before the early
// return, matching the same-chunk tool-call fix above. Accumulating after
// the terminal-finish-reason check would return before this chunk's media
// was ever added, silently dropping it.
func TestHandleStream_MediaInTerminalChunkIsAccumulated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		finishReason chat.FinishReason
	}{
		{"stop", chat.FinishReasonStop},
		{"length", chat.FinishReasonLength},
		{"refusal", chat.FinishReasonRefusal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
			stream := newStreamBuilder().
				AddMediaWithStop(imgBytes, "image/png", "cat.png", tc.finishReason).
				Build()

			a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
			sess := session.New(session.WithUserMessage("go"))

			evCh := make(chan Event, 64)
			res, err := handleStream(
				t.Context(), nil, stream, a, nil, sess, nil,
				defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
			)
			require.NoError(t, err)

			require.Len(t, res.Media, 1, "media sharing a chunk with the terminal finish reason must not be dropped")
			assert.Equal(t, imgBytes, res.Media[0].Data)
			assert.Equal(t, tc.finishReason, res.FinishReason)
		})
	}
}

// TestHandleStream_WhitespaceOnlyContentStops is a regression test for an
// infinite-loop risk surfaced while reviewing #3145. A turn that streams only
// whitespace content and ends with a bare EOF (no finish reason) must report
// Stopped=true. runTurn emits an empty-turn warning whenever the trimmed
// content is empty and there are no tool calls; were such a turn not stopped,
// runTurn would fall through to turnContinue and re-enter the model with
// identical messages, spinning forever.
func TestHandleStream_WhitespaceOnlyContentStops(t *testing.T) {
	stream := newStreamBuilder().
		AddContent("\n\n   "). // whitespace only
		Build()                // no terminal chunk: bare EOF, no finish reason

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)

	assert.Empty(t, res.Calls)
	assert.True(t, res.Stopped,
		"a whitespace-only, bare-EOF turn must stop so the empty-turn warning is followed by a turn exit, not an identical re-entry (#3145)")
}

// TestHandleStream_ContentOnlyBareEOFStops guards the loop that OpenAI-compatible
// gateways (litellm/CBORG) trigger: they close the SSE stream with a bare EOF and
// never send a per-choice finish_reason. A turn that produced real content but no
// tool calls has nothing left for the run loop to execute, so it must report
// Stopped=true. Keying the stop decision on empty content instead of "no tool
// calls" left such a final message with Stopped=false, so runTurn re-entered the
// model with identical messages and re-emitted the same completion text forever.
func TestHandleStream_ContentOnlyBareEOFStops(t *testing.T) {
	stream := newStreamBuilder().
		AddContent("Frontmatter ingestion complete."). // real content, no tool calls
		Build()                                        // no terminal chunk: bare EOF, no finish reason

	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	evCh := make(chan Event, 64)
	res, err := handleStream(
		t.Context(), nil, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), defaultStreamIdleTimeout,
	)
	require.NoError(t, err)

	assert.Empty(t, res.Calls)
	assert.True(t, res.Stopped,
		"a content-bearing, bare-EOF turn with no tool calls must stop so the run loop exits instead of re-entering with identical messages")
}

// stalledStream is a chat.MessageStream that blocks in Recv() until
// either unblocked or the stream is closed. It is used to simulate a
// half-open TCP connection where the remote side stops sending data.
type stalledStream struct {
	// unblock is closed to release a blocked Recv call.
	unblock chan struct{}
	// recvStarted is closed once the first Recv call is in flight, so
	// tests can cancel a context while Recv is provably blocked.
	recvStarted chan struct{}
	recvOnce    sync.Once
	// closeOnce guards unblock so Close is safe to call concurrently from
	// both the test goroutine and handleStream's deferred Close.
	closeOnce sync.Once
}

func newStalledStream() *stalledStream {
	return &stalledStream{
		unblock:     make(chan struct{}),
		recvStarted: make(chan struct{}),
	}
}

// Recv blocks until unblock is closed, then returns io.EOF.
func (s *stalledStream) Recv() (chat.MessageStreamResponse, error) {
	s.recvOnce.Do(func() { close(s.recvStarted) })
	<-s.unblock
	return chat.MessageStreamResponse{}, io.EOF
}

func (s *stalledStream) Close() {
	s.closeOnce.Do(func() { close(s.unblock) })
}

// TestHandleStream_IdleTimeout verifies that handleStream returns an error
// wrapping errStreamIdle when no SSE chunk arrives within the idle window.
// It also checks that the provided cancelStream function is called so the
// HTTP transport can close the underlying TCP connection.
func TestHandleStream_IdleTimeout(t *testing.T) {
	t.Parallel()

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
		defaultTelemetry{}, NewChannelSink(evCh), 50*time.Millisecond,
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
	t.Parallel()

	stream := newStalledStream()
	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))

	ctx, cancel := context.WithCancel(t.Context())

	// Cancel the context once handleStream is provably blocked in Recv.
	go func() {
		<-stream.recvStarted
		cancel()
		stream.Close() // unblock the stalled Recv so the reader goroutine can exit
	}()

	evCh := make(chan Event, 64)
	_, cancelStream := context.WithCancelCause(ctx)
	// Use a long idle timeout so only context cancellation can trigger.
	res, err := handleStream(
		ctx, cancelStream, stream, a, nil, sess, nil,
		defaultTelemetry{}, NewChannelSink(evCh), 10*time.Minute,
	)

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "error must be context.Canceled")
	assert.True(t, res.Stopped)
}
