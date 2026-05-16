package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/openai/openai-go/v3/responses"
)

const (
	// wsHandshakeTimeout is the maximum time allowed for the WebSocket handshake.
	wsHandshakeTimeout = 45 * time.Second
)

// wsCreateMessage is the envelope sent over WebSocket to start a new response.
// It wraps ResponseNewParams with the required "type" discriminator.
type wsCreateMessage struct {
	Type string `json:"type"`

	// Embed the params as a raw message so that its MarshalJSON is used
	// and we simply add the "type" key on top.
	Params json.RawMessage `json:"-"`
}

func (m wsCreateMessage) MarshalJSON() ([]byte, error) {
	// Marshal the params first, then inject "type".
	raw := m.Params
	if raw == nil {
		raw = []byte("{}")
	}

	// Merge: start with the params object, add "type".
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("wsCreateMessage: unmarshal params: %w", err)
	}
	typeVal, _ := json.Marshal(m.Type)
	obj["type"] = typeVal
	return json.Marshal(obj)
}

// wsStream implements responseEventStream for a single request/response
// exchange over a WebSocket connection.
//
// After the terminal event (response.completed, response.failed, etc.) is
// delivered via Current(), the next call to Next() returns false.
type wsStream struct {
	conn    *websocket.Conn
	current responses.ResponseStreamEventUnion
	err     error
	done    bool
}

// Compile-time check: wsStream satisfies responseEventStream.
var _ responseEventStream = (*wsStream)(nil)

// sendResponseCreate marshals params and writes a response.create message
// on the given WebSocket connection.
func sendResponseCreate(conn *websocket.Conn, params responses.ResponseNewParams) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("websocket: marshal params: %w", err)
	}

	msg := wsCreateMessage{
		Type:   "response.create",
		Params: paramsJSON,
	}

	if err := conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("websocket: write response.create: %w", err)
	}

	return nil
}

// dialWebSocket opens a WebSocket connection, sends the response.create
// message, and returns a stream that yields server events.
func dialWebSocket(
	ctx context.Context,
	wsURL string,
	headers http.Header,
	params responses.ResponseNewParams,
) (*wsStream, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: wsHandshakeTimeout,
	}

	slog.DebugContext(ctx, "Opening WebSocket connection", "url", wsURL)

	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			slog.ErrorContext(ctx, "WebSocket handshake failed",
				"status", resp.StatusCode,
				"error", err)
		}
		return nil, fmt.Errorf("websocket dial %s: %w", wsURL, err)
	}

	if err := sendResponseCreate(conn, params); err != nil {
		conn.Close()
		return nil, err
	}

	slog.DebugContext(ctx, "WebSocket response.create sent", "url", wsURL)

	return &wsStream{conn: conn}, nil
}

// Next reads the next event from the WebSocket. Returns false when the
// response is complete or an error occurred.
func (s *wsStream) Next() bool {
	if s.done {
		return false
	}

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) {
				s.done = true
				return false
			}
			s.err = fmt.Errorf("websocket read: %w", err)
			s.done = true
			return false
		}

		if len(bytes.TrimSpace(data)) == 0 {
			slog.Debug("Ignoring empty WebSocket frame")
			continue
		}

		var event responses.ResponseStreamEventUnion
		if err := json.Unmarshal(data, &event); err != nil {
			s.err = fmt.Errorf("websocket unmarshal event: %w", err)
			s.done = true
			return false
		}

		s.current = event
		break
	}

	event := s.current
	slog.Debug("WebSocket event received", "type", event.Type)

	// Check for server-side error events.
	if event.Type == "error" {
		s.err = fmt.Errorf("openai websocket error: %s (param: %s)", event.Message, event.Param)
		s.done = true
		// Still return true so the caller can inspect the event.
		return true
	}

	// Terminal events: deliver this event then stop on next call.
	if isTerminalEvent(event.Type) {
		s.done = true
		// Return true so the adapter receives usage/finish data.
		return true
	}

	return true
}

// Current returns the most recently decoded event.
func (s *wsStream) Current() responses.ResponseStreamEventUnion {
	return s.current
}

// Err returns the first non-EOF error encountered by the stream.
func (s *wsStream) Err() error {
	return s.err
}

// Close sends a close frame and releases the connection.
func (s *wsStream) Close() error {
	s.done = true
	// Best-effort close handshake.
	_ = s.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	return s.conn.Close()
}

// isTerminalEvent returns true for event types that signal the end of a
// response on the WebSocket stream.
func isTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done",
		"response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

// httpToWSURL converts an HTTP(S) base URL to its WebSocket equivalent.
// "https://api.openai.com/v1" → "wss://api.openai.com/v1/responses"
func httpToWSURL(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	if !strings.HasSuffix(u, "/responses") {
		u += "/responses"
	}
	return u
}
