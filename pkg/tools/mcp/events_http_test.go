package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestHTTPEventSubscriptions(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprintf("sessionOwned=%t", owned), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var active atomic.Int32
			var headersOK atomic.Bool
			headersOK.Store(true)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if r.Header.Get("X-Event-Test") != "present" {
					headersOK.Store(false)
				}
				writeResult := func(result any) {
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
				}
				if len(req.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "server/discover":
					writeResult(map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"extensions": map[string]any{eventsExtension: map[string]any{}}}})
				case "events/list":
					writeResult(map[string]any{"resultType": "complete", "events": []any{map[string]any{"name": "demo.message", "delivery": []string{"push"}, "inputSchema": map[string]any{"type": "object"}, "payloadSchema": map[string]any{"type": "object"}}}})
				case "events/stream":
					if strings.Contains(string(req.Params), localSubscriptionMeta) {
						headersOK.Store(false)
					}
					var p eventParams
					if json.Unmarshal(req.Params, &p) != nil {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					label, _ := p.Arguments["label"].(string)
					w.Header().Set("Content-Type", "text/event-stream")
					active.Add(1)
					defer active.Add(-1)
					notify := func(method string, values map[string]any) {
						values["_meta"] = map[string]any{subscriptionIDMeta: req.ID}
						data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": values})
						_, _ = fmt.Fprintf(w, "event: message\r\ndata: %s\r\n\r\n", data)
						_ = http.NewResponseController(w).Flush()
					}
					notify("notifications/events/active", map[string]any{"cursor": nil})
					notify("notifications/events/heartbeat", map[string]any{"cursor": nil})
					notify("notifications/events/event", map[string]any{"eventId": "shared-id", "name": "demo.message", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]any{"label": label}})
					<-r.Context().Done()
				case "tools/list":
					writeResult(map[string]any{"resultType": "complete", "tools": []any{}})
				default:
					writeResult(map[string]any{"resultType": "complete"})
				}
			}))
			defer srv.Close()
			var sc *sessionClient
			var closeClient func(context.Context) error
			if owned {
				c := &sessionRemoteClient{endpoint: srv.URL, transport: "streamable", headers: map[string]string{"X-Event-Test": "present"}, base: http.DefaultTransport}
				c.setExperimentalEvents(true)
				_, err := c.Initialize(ctx, nil)
				require.NoError(t, err)
				sc, closeClient = &c.sessionClient, c.Close
			} else {
				c := newRemoteClient(srv.URL, "streamable", map[string]string{"X-Event-Test": "present"}, nil, nil, true, nil)
				c.setExperimentalEvents(true)
				_, err := c.Initialize(ctx, nil)
				require.NoError(t, err)
				sc, closeClient = &c.sessionClient, c.Close
			}
			defer func() { require.NoError(t, closeClient(ctx)) }()
			localTools := sc.eventTools("remote")
			require.Len(t, localTools, 3)
			runtimes := []*eventRuntime{{messages: make(chan string, 4)}, {messages: make(chan string, 4)}}
			for i, rt := range runtimes {
				args := fmt.Sprintf(`{"name":"demo.message","arguments":{"label":"stream-%d"}}`, i)
				_, err := localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: args}}, rt)
				require.NoError(t, err)
				select {
				case message := <-rt.messages:
					assert.Contains(t, message, fmt.Sprintf("stream-%d", i))
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			assert.EqualValues(t, 2, active.Load())
			// Ordinary RPCs must still work while both streams are open.
			_, err := sc.getSession().ListTools(ctx, nil)
			require.NoError(t, err)
			_, err = localTools[2].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"id":"event-1"}`}}, runtimes[0])
			require.NoError(t, err)
			require.Eventually(t, func() bool { return active.Load() == 1 }, 3*time.Second, 10*time.Millisecond)
			require.NoError(t, closeClient(ctx))
			require.Eventually(t, func() bool { return active.Load() == 0 }, 3*time.Second, 10*time.Millisecond)
			assert.True(t, headersOK.Load())
		})
	}
}

func TestHTTPEventBody(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := &eventSubscription{wireID: "7", cancel: cancel, messages: make(chan *jsonrpc.Request, 2)}
	events := &eventClient{subscriptions: map[string]*eventSubscription{"test": sub}}
	notification := "event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/active\",\r\ndata: \"params\":{\"_meta\":{\"io.modelcontextprotocol/subscriptionId\":7}}}\r\n\r\n"
	response := "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n"
	body := io.NopCloser(strings.NewReader(": keepalive\n\n" + notification + response))
	filtered := &eventHTTPBody{ReadCloser: body, reader: bufio.NewReader(body), events: events}
	output, err := io.ReadAll(filtered)
	require.NoError(t, err)
	assert.Equal(t, ": keepalive\n\n"+response, string(output))
	assert.Len(t, sub.messages, 1)
	require.NoError(t, cancelCtx.Err())
	oversized := io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 1024*1024)))
	_, err = io.ReadAll(&eventHTTPBody{ReadCloser: oversized, reader: bufio.NewReader(oversized), events: events})
	require.ErrorContains(t, err, "exceeds 1 MiB")
}
