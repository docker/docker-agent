package openai

// responseEventStream abstracts over SSE and WebSocket transports for
// streaming Responses API events.
type responseEventStream interface {
	Next() bool
	Current() ResponseStreamEvent
	Err() error
	Close() error
}
