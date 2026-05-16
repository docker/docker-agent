package openai

import "github.com/openai/openai-go/v3/responses"

// responseEventStream abstracts over SSE and WebSocket transports for
// streaming Responses API events.
//
// The ssestream.Stream[responses.ResponseStreamEventUnion] type already
// satisfies this interface, so it can be used directly.
type responseEventStream interface {
	// Next advances the stream to the next event.
	// Returns false when the stream is exhausted or an error occurred.
	Next() bool

	// Current returns the most recently decoded event.
	Current() responses.ResponseStreamEventUnion

	// Err returns the first non-EOF error encountered by the stream.
	Err() error

	// Close releases resources held by the stream.
	Close() error
}
