package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWSServerCapture starts a test WebSocket server that captures each
// response.create message into the returned slice and replies with the
// given canned events.
func testWSServerCapture(t *testing.T, events []map[string]any) (*httptest.Server, *[]map[string]json.RawMessage) {
	t.Helper()

	var captured []map[string]json.RawMessage

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		for {
			// Read a response.create message.
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}

			var createMsg map[string]json.RawMessage
			if err := json.Unmarshal(data, &createMsg); err != nil {
				t.Errorf("Failed to unmarshal response.create: %v", err)
				return
			}
			captured = append(captured, createMsg)

			// Send events.
			for _, event := range events {
				eventData, _ := json.Marshal(event)
				if err := conn.WriteMessage(websocket.TextMessage, eventData); err != nil {
					return
				}
			}
		}
	}))

	return srv, &captured
}

func completedEvent(responseID string) map[string]any {
	return map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     responseID,
			"output": []any{},
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 1,
				"total_tokens":  6,
				"input_tokens_details": map[string]any{
					"cached_tokens": 0,
				},
				"output_tokens_details": map[string]any{
					"reasoning_tokens": 0,
				},
			},
		},
	}
}

func TestWSPool_InjectsPreviousResponseID(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		completedEvent("resp_first"),
	}

	srv, captured := testWSServerCapture(t, events)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	pool := newWSPool(wsURL, func(_ context.Context) (http.Header, error) {
		return http.Header{}, nil
	})
	defer pool.Close()

	ctx := t.Context()

	// --- First request: no previous_response_id should be set.
	stream1, err := pool.Stream(ctx, defaultTestParams())
	require.NoError(t, err)
	drainStream(t, stream1)

	// After draining, the pool should have captured the response ID.
	assert.Equal(t, "resp_first", pool.lastResponseID)

	// --- Second request: the pool should inject previous_response_id automatically.
	// Change events for the second request to return a different ID.
	// (The server always sends the same events we initialized, so we verify
	// the injection from the captured request.)
	stream2, err := pool.Stream(ctx, defaultTestParams())
	require.NoError(t, err)
	drainStream(t, stream2)

	// Verify captured messages.
	require.Len(t, *captured, 2)

	// First request: no previous_response_id.
	assertPreviousResponseID(t, (*captured)[0], "")

	// Second request: pool injects the ID from the first response.
	assertPreviousResponseID(t, (*captured)[1], "resp_first")
}

func TestWSPool_CallerPreviousResponseIDNotOverwritten(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		completedEvent("resp_pool"),
	}

	srv, captured := testWSServerCapture(t, events)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	pool := newWSPool(wsURL, func(_ context.Context) (http.Header, error) {
		return http.Header{}, nil
	})
	defer pool.Close()

	ctx := t.Context()

	// First request — populate lastResponseID.
	stream1, err := pool.Stream(ctx, defaultTestParams())
	require.NoError(t, err)
	drainStream(t, stream1)

	assert.Equal(t, "resp_pool", pool.lastResponseID)

	// Second request with caller-provided previous_response_id.
	params := defaultTestParams()
	params.PreviousResponseID = param.NewOpt("caller_resp_999")

	stream2, err := pool.Stream(ctx, params)
	require.NoError(t, err)
	drainStream(t, stream2)

	require.Len(t, *captured, 2)

	// The caller's ID must NOT be overwritten by the pool.
	assertPreviousResponseID(t, (*captured)[1], "caller_resp_999")
}

func TestWSPool_LastResponseIDSurvivesReconnect(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		completedEvent("resp_survive"),
	}

	srv, captured := testWSServerCapture(t, events)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	pool := newWSPool(wsURL, func(_ context.Context) (http.Header, error) {
		return http.Header{}, nil
	})
	defer pool.Close()

	ctx := t.Context()

	// First request.
	stream1, err := pool.Stream(ctx, defaultTestParams())
	require.NoError(t, err)
	drainStream(t, stream1)

	assert.Equal(t, "resp_survive", pool.lastResponseID)

	// Force a reconnect by closing the pooled connection.
	pool.Close()

	// Second request after reconnection.
	stream2, err := pool.Stream(ctx, defaultTestParams())
	require.NoError(t, err)
	drainStream(t, stream2)

	require.Len(t, *captured, 2)

	// The lastResponseID should survive the reconnect.
	assertPreviousResponseID(t, (*captured)[1], "resp_survive")
}

// drainStream reads all events from a responseEventStream until exhausted.
func drainStream(t *testing.T, stream responseEventStream) {
	t.Helper()
	for stream.Next() {
		// consume
	}
	require.NoError(t, stream.Err())
	require.NoError(t, stream.Close())
}

// assertPreviousResponseID checks that the captured response.create message
// contains (or omits) the expected previous_response_id.
func assertPreviousResponseID(t *testing.T, msg map[string]json.RawMessage, expected string) {
	t.Helper()

	raw, ok := msg["previous_response_id"]
	if expected == "" {
		// Either absent or null.
		if ok {
			assert.JSONEq(t, "null", string(raw),
				"expected previous_response_id to be absent or null")
		}
		return
	}

	require.True(t, ok, "expected previous_response_id in request")
	var got string
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, expected, got)
}
