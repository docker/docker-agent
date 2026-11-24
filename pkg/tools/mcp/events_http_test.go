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
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
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
					if r.Header.Get(mcpNameHeader) != p.Name {
						headersOK.Store(false)
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

type chunkedReader struct {
	chunks []string
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 {
		if r.chunks[0] == "" {
			r.chunks = r.chunks[1:]
			continue
		}
		n := copy(p, r.chunks[0])
		r.chunks[0] = r.chunks[0][n:]
		return n, nil
	}
	return 0, io.EOF
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
	oversized := io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", maxSSEFrameBytes+1)))
	_, err = io.ReadAll(&eventHTTPBody{ReadCloser: oversized, reader: bufio.NewReader(oversized), events: events})
	require.ErrorContains(t, err, "exceeds maximum size")

	// Passthrough SSE frames > 1 MiB (up to SDK's 16 MiB cap) must succeed.
	largeFrame := "data: " + strings.Repeat("a", 2*1024*1024) + "\n\n"
	largeOut, err := io.ReadAll(&eventHTTPBody{ReadCloser: io.NopCloser(strings.NewReader(largeFrame)), reader: bufio.NewReader(strings.NewReader(largeFrame)), events: events})
	require.NoError(t, err)
	assert.Len(t, largeOut, len(largeFrame))

	// Optional UTF-8 BOM on the first line must be stripped before field parsing.
	bomFrame := "\uFEFFdata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/active\",\"params\":{\"_meta\":{\"io.modelcontextprotocol/subscriptionId\":7}}}\n\n"
	bomSub := &eventSubscription{wireID: "7", cancel: cancel, messages: make(chan *jsonrpc.Request, 2)}
	bomEvents := &eventClient{subscriptions: map[string]*eventSubscription{"bom": bomSub}}
	bomOut, err := io.ReadAll(&eventHTTPBody{ReadCloser: io.NopCloser(strings.NewReader(bomFrame)), reader: bufio.NewReader(strings.NewReader(bomFrame)), events: bomEvents})
	require.NoError(t, err)
	assert.Empty(t, bomOut)
	assert.Len(t, bomSub.messages, 1)

	// Fragmented CRLF passthrough and CR-only passthrough must normalize to LF.
	fragReader := &chunkedReader{chunks: []string{
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\r",
		"\n\r",
		"\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\r\r",
	}}
	fragBody := &eventHTTPBody{ReadCloser: io.NopCloser(fragReader), reader: bufio.NewReader(fragReader), events: events}
	fragOut, err := io.ReadAll(fragBody)
	require.NoError(t, err)
	assert.Equal(t, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n", string(fragOut))

	// Lone CR line terminators must dispatch immediately on an open stream
	// without waiting for another byte, and emit an unambiguous blank-line
	// terminated metadata frame.
	pr, pw := io.Pipe()
	crFiltered := &eventHTTPBody{ReadCloser: pr, reader: bufio.NewReader(pr), events: events}
	crNotification := "id: evt-42\rretry: 2500\revent: message\rdata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events/active\",\"params\":{\"_meta\":{\"io.modelcontextprotocol/subscriptionId\":7}}}\r\r"
	go func() {
		_, _ = io.WriteString(pw, crNotification)
	}()
	buf := make([]byte, 128)
	n, err := crFiltered.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "id: evt-42\nretry: 2500\n\n", string(buf[:n]))
	assert.Len(t, sub.messages, 2)
	_ = pw.Close()
}

func TestHTTPEventToolsReconnectAndUniqueIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

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
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeResult := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "server/discover":
			writeResult(map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"extensions": map[string]any{eventsExtension: map[string]any{}}}})
		case "events/list":
			writeResult(map[string]any{"resultType": "complete", "events": []any{map[string]any{"name": "demo.message", "delivery": []string{"push"}, "inputSchema": map[string]any{"type": "object"}, "payloadSchema": map[string]any{"type": "object"}}}})
		case "events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"method":  "notifications/events/active",
				"params":  map[string]any{"cursor": nil, "_meta": map[string]any{subscriptionIDMeta: req.ID}},
			})
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		default:
			writeResult(map[string]any{"resultType": "complete"})
		}
	}))
	defer srv.Close()

	c := newRemoteClient(srv.URL, "streamable", nil, nil, nil, true, nil)
	c.setExperimentalEvents(true)
	_, err := c.Initialize(ctx, nil)
	require.NoError(t, err)

	// Capture tool handlers before reconnect.
	localTools := c.eventTools("remote")
	require.Len(t, localTools, 3)

	rt := &eventRuntime{messages: make(chan string, 4)}
	res1, err := localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message"}`}}, rt)
	require.NoError(t, err)
	assert.Equal(t, "Subscribed: event-1", res1.Output)

	// Close and reconnect.
	require.NoError(t, c.Close(ctx))
	_, err = c.Initialize(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close(ctx)) }()

	// Invoking previously listed handlers must use the new session and issue a unique ID.
	_, err = localTools[0].Handler(ctx, tools.ToolCall{}, rt)
	require.NoError(t, err)
	res2, err := localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message"}`}}, rt)
	require.NoError(t, err)
	assert.Equal(t, "Subscribed: event-2", res2.Output)

	// Stale ID from the closed connection must not cancel the new subscription.
	_, err = localTools[2].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"id":"event-1"}`}}, rt)
	require.ErrorContains(t, err, `unknown event subscription "event-1"`)
	_, err = localTools[2].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"id":"event-2"}`}}, rt)
	require.NoError(t, err)
}

func TestHTTPEventOversizedRequestRejected(t *testing.T) {
	transport := &eventHTTPTransport{
		base:   http.DefaultTransport,
		events: &eventClient{subscriptions: make(map[string]*eventSubscription)},
	}
	payload := `{"jsonrpc":"2.0","id":1,"method":"events/stream","params":{"name":"demo.message","_meta":{"` + localSubscriptionMeta + `":"event-1"},"arguments":{"pad":"` + strings.Repeat("a", 1024*1024) + `"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Mcp-Method", "events/stream")
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "exceeds 1 MiB")
}

func TestEventOnlyToolsetMethodNotFoundAndCallTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var stallList atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "server/discover":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"resultType": "complete", "supportedVersions": []string{"2026-07-28"},
				"capabilities": map[string]any{"extensions": map[string]any{eventsExtension: map[string]any{}}},
			}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{
				"code": jsonrpc.CodeMethodNotFound, "message": "tools/list not supported",
			}})
		case "events/list":
			if stallList.Load() {
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"resultType": "complete", "events": []any{},
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"resultType": "complete"}})
		}
	}))
	defer srv.Close()

	for _, sessionOwned := range []bool{false, true} {
		t.Run(fmt.Sprintf("sessionOwned=%t", sessionOwned), func(t *testing.T) {
			stallList.Store(false)
			var ts *Toolset
			if sessionOwned {
				ts = NewSessionRemoteToolset("eventsonly", srv.URL, "streamable", nil)
				ts.mcpClient.(*sessionRemoteClient).base = http.DefaultTransport
				ts.callTimeout = 50 * time.Millisecond
			} else {
				ts = NewRemoteToolsetWithAllowPrivateIPs("eventsonly", srv.URL, "streamable", nil, nil, true, lifecycle.Policy{
					CallTimeout: 50 * time.Millisecond,
				})
			}
			ts.SetExperimentalEvents(true)
			require.NoError(t, ts.Start(ctx))
			defer func() { require.NoError(t, ts.Stop(ctx)) }()

			toolList, err := ts.Tools(ctx)
			require.NoError(t, err)
			require.Len(t, toolList, 3)
			assert.Equal(t, "eventsonly_mcp_events_list", toolList[0].Name)

			stallList.Store(true)
			_, err = toolList[0].Handler(ctx, tools.ToolCall{}, &eventRuntime{messages: make(chan string, 1)})
			require.ErrorIs(t, err, tools.ErrCallTimeout)
		})
	}
}

func TestEncodeMCPHeaderName(t *testing.T) {
	assert.Equal(t, "demo.message", encodeMCPHeaderName("demo.message"))
	assert.Equal(t, "=?base64?IGRlbW8=?=", encodeMCPHeaderName(" demo"))
	assert.Equal(t, "=?base64?ZGVtbyA=?=", encodeMCPHeaderName("demo "))
	assert.Equal(t, "=?base64?Y2Fmw6k=?=", encodeMCPHeaderName("café"))
	assert.Equal(t, "=?base64?PT9iYXNlNjQ/YWJjPz0=?=", encodeMCPHeaderName("=?base64?abc?="))
}

func TestHTTPEventResumedGET(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var streamReqID atomic.Value
	var getResumed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.Header.Get("Last-Event-ID") != "prime-1" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			getResumed.Store(true)
			id, _ := streamReqID.Load().(json.RawMessage)
			w.Header().Set("Content-Type", "text/event-stream")
			notify := func(method string, values map[string]any) {
				values["_meta"] = map[string]any{subscriptionIDMeta: id}
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": values})
				_, _ = fmt.Fprintf(w, "event: message\r\ndata: %s\r\n\r\n", data)
				_ = http.NewResponseController(w).Flush()
			}
			notify("notifications/events/active", map[string]any{"cursor": "c1"})
			notify("notifications/events/event", map[string]any{
				"eventId": "resumed-1", "name": "demo.message", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
				"cursor": "c2", "data": map[string]any{"via": "get"},
			})
			result, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"resultType": "complete"}})
			_, _ = fmt.Fprintf(w, "event: message\r\ndata: %s\r\n\r\n", result)
			_ = http.NewResponseController(w).Flush()
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeResult := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "server/discover":
			writeResult(map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"extensions": map[string]any{eventsExtension: map[string]any{}}}})
		case "events/list":
			writeResult(map[string]any{"resultType": "complete", "events": []any{map[string]any{"name": "demo.message", "delivery": []string{"push"}, "inputSchema": map[string]any{"type": "object"}, "payloadSchema": map[string]any{"type": "object"}}}})
		case "events/stream":
			streamReqID.Store(req.ID)
			w.Header().Set("Content-Type", "text/event-stream")
			// Send only an SSE priming event with an ID, then close so the SDK resumes via GET.
			_, _ = fmt.Fprint(w, "id: prime-1\nretry: 10\n\n")
			_ = http.NewResponseController(w).Flush()
		default:
			writeResult(map[string]any{"resultType": "complete"})
		}
	}))
	defer srv.Close()

	c := newRemoteClient(srv.URL, "streamable", nil, nil, nil, true, nil)
	c.setExperimentalEvents(true)
	_, err := c.Initialize(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close(ctx)) }()

	localTools := c.eventTools("remote")
	require.Len(t, localTools, 3)
	rt := &eventRuntime{messages: make(chan string, 4)}
	_, err = localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message"}`}}, rt)
	require.NoError(t, err)
	select {
	case msg := <-rt.messages:
		assert.Contains(t, msg, `"eventId":"resumed-1"`)
		assert.Contains(t, msg, `"via":"get"`)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assert.True(t, getResumed.Load())
}

func TestHTTPEventErrorSanitization(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var failList atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "events/list" && failList.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
			"resultType": "complete", "supportedVersions": []string{"2026-07-28"},
			"capabilities": map[string]any{"extensions": map[string]any{eventsExtension: map[string]any{}}},
		}})
	}))
	defer srv.Close()

	c := &sessionRemoteClient{
		sessionClient: sessionClient{sanitizeError: sessionRemoteError},
		endpoint:      srv.URL + "/mcp?api_key=top-secret-query",
		transport:     "streamable",
		base:          http.DefaultTransport,
	}
	c.setExperimentalEvents(true)
	_, err := c.Initialize(ctx, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close(ctx)) }()

	failList.Store(true)
	localTools := c.eventTools("remote")
	require.Len(t, localTools, 3)
	_, err = localTools[0].Handler(ctx, tools.ToolCall{}, &eventRuntime{messages: make(chan string, 1)})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "top-secret-query")
	assert.Equal(t, "client remote MCP request failed", err.Error())
}
