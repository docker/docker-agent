package genai

import (
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// StreamAttributer is an optional interface that provider stream adapters
// may implement to surface provider-specific attributes to the chat span
// once the response is complete. The wrapper queries the underlying stream
// on Close (in addition to the per-chunk Recv path) and applies whatever
// attributes the provider chose to expose. Implementations are expected to
// be safe to call after Close.
type StreamAttributer interface {
	GenAIStreamAttributes() []KeyValue
}

// KeyValue is a re-exported attribute key/value pair used by the optional
// StreamAttributer interface so providers can implement it without
// importing go.opentelemetry.io/otel/attribute directly. The decorator
// converts these back into OTel attributes before applying them to the
// span.
type KeyValue struct {
	Key   string
	Value any
}

// WrapStream wraps a chat.MessageStream so that consuming the stream
// drives the lifecycle of a ChatSpan: per-chunk timing, response-level
// attributes (id / response.model / finish reasons), usage capture, and
// final span End on stream close or terminal error.
//
// The returned stream forwards all Recv/Close calls to the underlying
// stream verbatim and adds no other behaviour, so swapping it in is
// invisible to callers.
func WrapStream(span *ChatSpan, stream chat.MessageStream) chat.MessageStream {
	if span == nil || stream == nil {
		return stream
	}
	return &instrumentedStream{
		span:    span,
		inner:   stream,
		capture: IsContentCaptureEnabled(),
	}
}

type instrumentedStream struct {
	span  *ChatSpan
	inner chat.MessageStream

	// mu guards the lifecycle flags and the streaming-state buffers
	// so a Recv that errors concurrently with the consumer's Close
	// does not race on the check-then-set in endOnce or
	// double-apply attributes through SetOutputMessages.
	mu sync.Mutex

	// ended is set when the span has been finalised (output flushed
	// and `End` called). innerClosed is set when the inner stream's
	// `Close` has been called. They are tracked separately so an
	// error in `Recv` can end the span without preempting the
	// caller's `Close` that releases the inner stream's resources.
	ended       bool
	innerClosed bool

	// capture buffers the streamed deltas for emission as
	// `gen_ai.output.messages` on Close. Filled only when content
	// capture is opted in (`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true`)
	// so the buffer cost stays out of the default request path.
	capture       bool
	contentBuf    strings.Builder
	reasoningBuf  strings.Builder
	pendingTools  map[string]*tools.ToolCall
	toolCallOrder []string
}

func (s *instrumentedStream) Recv() (chat.MessageStreamResponse, error) {
	resp, err := s.inner.Recv()
	if err != nil {
		// io.EOF is the normal stream terminator and is not an error
		// for the span's purposes — End handles closing.
		// For non-EOF errors we end the span here too: callers that
		// abandon the stream after an error (a common pattern for
		// network failures) would otherwise leak the span and skip the
		// duration metric. Close remains idempotent so the canonical
		// `defer Close()` path still works.
		if !errors.Is(err, io.EOF) {
			s.span.RecordError(err, ClassifyError(err))
			s.endOnce()
		}
		return resp, err
	}

	// First chunk arrival is meaningful for the time_to_first_chunk
	// metric. Mark on every Recv that produced any content so we cover
	// cases where the provider opens with an empty preamble.
	if hasChunkPayload(&resp) {
		s.span.MarkChunk()
	}

	if resp.ID != "" {
		s.span.SetResponseID(resp.ID)
	}
	if resp.Model != "" {
		s.span.SetResponseModel(resp.Model)
	}
	for i := range resp.Choices {
		if resp.Choices[i].FinishReason != "" {
			s.span.AddFinishReason(string(resp.Choices[i].FinishReason))
		}
	}
	if resp.Usage != nil {
		s.span.RecordUsage(
			resp.Usage.InputTokens,
			resp.Usage.OutputTokens,
			resp.Usage.CachedInputTokens,
			resp.Usage.CacheWriteTokens,
			resp.Usage.ReasoningTokens,
		)
	}

	if s.capture {
		s.mu.Lock()
		s.bufferDeltas(&resp)
		s.mu.Unlock()
	}
	return resp, nil
}

// bufferDeltas accumulates content and tool-call deltas for the
// gen_ai.output.messages attribute. Tool calls arrive across multiple
// chunks (id once, name once, arguments in pieces), so we keep a map
// keyed by id and concatenate arguments as they stream in.
func (s *instrumentedStream) bufferDeltas(resp *chat.MessageStreamResponse) {
	for i := range resp.Choices {
		d := &resp.Choices[i].Delta
		if d.Content != "" {
			s.contentBuf.WriteString(d.Content)
		}
		if d.ReasoningContent != "" {
			s.reasoningBuf.WriteString(d.ReasoningContent)
		}
		for j := range d.ToolCalls {
			tc := &d.ToolCalls[j]
			id := tc.ID
			if id == "" {
				// Provider didn't include the id on this delta — fall
				// back to the most recent in-progress tool call.
				if len(s.toolCallOrder) == 0 {
					continue
				}
				id = s.toolCallOrder[len(s.toolCallOrder)-1]
			}
			if s.pendingTools == nil {
				s.pendingTools = map[string]*tools.ToolCall{}
			}
			existing, ok := s.pendingTools[id]
			if !ok {
				existing = &tools.ToolCall{ID: id, Type: tc.Type}
				s.pendingTools[id] = existing
				s.toolCallOrder = append(s.toolCallOrder, id)
			}
			if tc.Function.Name != "" {
				existing.Function.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				existing.Function.Arguments += tc.Function.Arguments
			}
		}
	}
}

func (s *instrumentedStream) Close() {
	s.mu.Lock()
	closeInner := !s.innerClosed
	s.innerClosed = true
	s.mu.Unlock()
	if closeInner {
		s.inner.Close()
	}
	s.endOnce()
}

// endOnce flushes captured content, applies provider-supplied attributes,
// and ends the span — at most once per stream. Both the error path in
// `Recv` and the explicit `Close` path go through here so a stream that
// errors mid-flight still ends its span without waiting for the caller.
// `inner.Close` is intentionally NOT called here: leaving it to the
// explicit `Close` path keeps the contract that the wrapper releases
// the underlying stream exactly when the caller asks.
func (s *instrumentedStream) endOnce() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	// Snapshot the buffers under the lock so we don't race against a
	// concurrent Recv writing more deltas. Release before calling out
	// to the OTel SDK and the StreamAttributer hook to avoid holding
	// the mutex across third-party code.
	var (
		extras       []KeyValue
		captured     bool
		content      string
		reasoning    string
		collected    []tools.ToolCall
		streamAttrer StreamAttributer
	)
	if attrer, ok := s.inner.(StreamAttributer); ok {
		streamAttrer = attrer
	}
	if s.capture {
		captured = true
		content = s.contentBuf.String()
		reasoning = s.reasoningBuf.String()
		for _, id := range s.toolCallOrder {
			if tc, ok := s.pendingTools[id]; ok {
				collected = append(collected, *tc)
			}
		}
	}
	s.mu.Unlock()

	if streamAttrer != nil {
		extras = streamAttrer.GenAIStreamAttributes()
	}
	for _, kv := range extras {
		applyExtraAttribute(s.span, kv)
	}
	if captured {
		SetOutputMessages(s.span, content, reasoning, collected)
	}
	s.span.End()
}

// hasChunkPayload reports whether the response carries content that should
// count as an output chunk (text, reasoning, tool call, etc.). Empty
// keep-alive frames do not advance the per-chunk timing metrics.
func hasChunkPayload(resp *chat.MessageStreamResponse) bool {
	for i := range resp.Choices {
		d := &resp.Choices[i].Delta
		if d.Content != "" || d.ReasoningContent != "" || d.ThinkingSignature != "" {
			return true
		}
		if len(d.ToolCalls) > 0 || d.FunctionCall != nil {
			return true
		}
	}
	return false
}
