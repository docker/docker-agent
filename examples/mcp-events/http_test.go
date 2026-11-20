package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPDemoLogging(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp?token=secret-query", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"secret":"private-payload"}}`))
	req.Header.Set("Authorization", "Bearer secret-header")
	serveHTTP(recorder, req)
	assert.Contains(t, logs.String(), `MCP request received: method="server/discover"`)
	assert.Contains(t, logs.String(), `MCP request finished: method="server/discover"`)

	writer := &sseWriter{response: httptest.NewRecorder(), requestID: "2"}
	_, err := writer.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/events/event","params":{"eventId":"event-1","data":"private-payload"}}` + "\n"))
	require.NoError(t, err)
	assert.Contains(t, logs.String(), `method="notifications/events/event" event_id="event-1"`)
	for _, secret := range []string{"secret-query", "secret-header", "private-payload"} {
		assert.NotContains(t, logs.String(), secret)
	}

	serveHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp", http.NoBody))
	assert.Contains(t, logs.String(), "status=405")
	serveHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader("invalid")))
	assert.Contains(t, logs.String(), "invalid JSON-RPC status=400")
}

func TestHTTPDemo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(serveHTTP))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`))
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	discovery, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)
	assert.Contains(t, string(discovery), extension)
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"events/stream","params":{"name":"demo.message","arguments":{"interval_seconds":1,"count":1}}}`))
	require.NoError(t, err)
	resp, err = srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	scanner := bufio.NewScanner(resp.Body)
	var messages []map[string]any
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var message map[string]any
			require.NoError(t, json.Unmarshal([]byte(data), &message))
			messages = append(messages, message)
		}
	}
	require.NoError(t, scanner.Err())
	require.Len(t, messages, 3)
	assert.Equal(t, "notifications/events/active", messages[0]["method"])
	assert.Equal(t, "notifications/events/event", messages[1]["method"])
	assert.EqualValues(t, 2, messages[2]["id"])
}
