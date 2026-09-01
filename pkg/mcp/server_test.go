package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/httpsec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// annot is a shorthand for building tools.ToolAnnotations in tests.
func annot(readOnly, idempotent bool, destructive, openWorld *bool) tools.ToolAnnotations {
	return tools.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		IdempotentHint:  idempotent,
		DestructiveHint: destructive,
		OpenWorldHint:   openWorld,
	}
}

func TestStartHTTPServer_RejectsAutonomousYAMLSafety(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	err = StartHTTPServer(t.Context(), "testdata/autonomous.yaml", "root", &config.RuntimeConfig{}, ln, HTTPOptions{})
	require.ErrorContains(t, err, "--safety autonomous")
}

func TestStartHTTPServer_RejectsKeepAlive(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	// A nonexistent agent file proves the guard fires before config and team
	// loading: reaching them would fail with a different error.
	runConfig := &config.RuntimeConfig{}
	runConfig.MCPKeepAlive = 30 * time.Second
	err = StartHTTPServer(t.Context(), "testdata/does-not-exist.yaml", "root", runConfig, ln, HTTPOptions{})
	require.ErrorContains(t, err, "keep-alive")
	require.ErrorContains(t, err, "stdio transport")
}

func TestCreateMCPServer_AcceptsAutonomousYAMLSafetyForStdio(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	server, cleanup, err := createMCPServer(t.Context(), "testdata/autonomous.yaml", "root", &config.RuntimeConfig{})
	require.NoError(t, err)
	require.NotNil(t, server)
	cleanup()
}

func TestHTTPBearerAuth(t *testing.T) {
	t.Parallel()

	handler := httpsec.BearerAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", header: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "correct", header: "Bearer secret", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", http.NoBody)
			req.Header.Set("Authorization", tc.header)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestAgentToolAnnotations(t *testing.T) {
	t.Parallel()

	pFalse := new(false)
	pTrue := new(true)

	tests := []struct {
		name            string
		tools           []tools.Tool
		wantReadOnly    bool
		wantDestructive *bool // nil means default (true)
		wantIdempotent  bool
		wantOpenWorld   *bool // nil means default (true)
	}{
		{
			name:            "no tools yields most conservative defaults",
			wantReadOnly:    true,
			wantDestructive: pFalse,
			wantIdempotent:  true,
			wantOpenWorld:   pFalse,
		},
		{
			name: "all read-only tools",
			tools: []tools.Tool{
				{Name: "a", Annotations: annot(true, true, pFalse, pFalse)},
				{Name: "b", Annotations: annot(true, true, pFalse, pFalse)},
			},
			wantReadOnly:    true,
			wantDestructive: pFalse,
			wantIdempotent:  true,
			wantOpenWorld:   pFalse,
		},
		{
			name: "mixed read-only",
			tools: []tools.Tool{
				{Name: "reader", Annotations: annot(true, false, pFalse, pFalse)},
				{Name: "writer", Annotations: annot(false, false, pTrue, pFalse)},
			},
			wantReadOnly:   false,
			wantIdempotent: false,
			wantOpenWorld:  pFalse,
			// wantDestructive nil → at least one destructive tool
		},
		{
			name: "nil destructive hint treated as destructive",
			tools: []tools.Tool{
				{Name: "tool", Annotations: annot(false, false, nil, pFalse)},
			},
			wantOpenWorld: pFalse,
			// wantDestructive nil → nil DestructiveHint defaults to true
		},
		{
			name: "nil open world hint treated as open world",
			tools: []tools.Tool{
				{Name: "tool", Annotations: annot(false, false, pFalse, nil)},
			},
			wantDestructive: pFalse,
			// wantOpenWorld nil → nil OpenWorldHint defaults to true
		},
		{
			name: "open world tool makes agent open world",
			tools: []tools.Tool{
				{Name: "closed", Annotations: annot(true, false, pFalse, pFalse)},
				{Name: "web", Annotations: annot(true, false, pFalse, pTrue)},
			},
			wantReadOnly:    true,
			wantDestructive: pFalse,
			// wantOpenWorld nil → open world
		},
		{
			name: "all idempotent",
			tools: []tools.Tool{
				{Name: "a", Annotations: annot(false, true, pFalse, pFalse)},
				{Name: "b", Annotations: annot(false, true, pFalse, pFalse)},
			},
			wantDestructive: pFalse,
			wantIdempotent:  true,
			wantOpenWorld:   pFalse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ag := agent.New("test", "test agent", agent.WithTools(tt.tools...))
			got, err := agentToolAnnotations(t.Context(), ag)
			require.NoError(t, err)

			assert.Equal(t, tt.wantReadOnly, got.ReadOnlyHint, "ReadOnlyHint")
			assert.Equal(t, tt.wantDestructive, got.DestructiveHint, "DestructiveHint")
			assert.Equal(t, tt.wantIdempotent, got.IdempotentHint, "IdempotentHint")
			assert.Equal(t, tt.wantOpenWorld, got.OpenWorldHint, "OpenWorldHint")
		})
	}
}

func TestCreateMCPServer_ToolNameRejectsMultipleAgents(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	runConfig := &config.RuntimeConfig{}
	runConfig.MCPToolName = "my-alias"

	_, _, err := createMCPServer(t.Context(), "testdata/multi.yaml", "", runConfig)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--tool-name")
	assert.Contains(t, err.Error(), "exactly one agent")
}

// newTestHTTPHandler builds the production stateless handler around a server
// created through the production config-loading path.
func newTestHTTPHandler(t *testing.T) http.Handler {
	t.Helper()

	server, cleanup, err := createMCPServer(t.Context(), "testdata/autonomous.yaml", "root", &config.RuntimeConfig{})
	require.NoError(t, err)
	t.Cleanup(cleanup)

	return newStreamableHTTPHandler(server)
}

// recordingRoundTripper captures the JSON-RPC method of every outgoing
// request and any Mcp-Session-Id response header, so tests can assert on the
// exact wire exchange the production handler produces.
type recordingRoundTripper struct {
	mu         sync.Mutex
	methods    []string
	sessionIDs []string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var method string
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		method = msg.Method
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if method != "" {
		rt.methods = append(rt.methods, method)
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		rt.sessionIDs = append(rt.sessionIDs, id)
	}
	return resp, nil
}

func (rt *recordingRoundTripper) recordedMethods() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.methods...)
}

func (rt *recordingRoundTripper) recordedSessionIDs() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.sessionIDs...)
}

// TestStreamableHTTPHandler_StatelessNegotiates20260728 proves the production
// HTTP handler negotiates the sessionless MCP 2026-07-28 revision with an SDK
// v1.7 client: negotiation happens via server/discover (never the legacy
// initialize handshake), tool listing succeeds, and no response ever carries
// an Mcp-Session-Id header.
func TestStreamableHTTPHandler_StatelessNegotiates20260728(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	httpSrv := httptest.NewServer(newTestHTTPHandler(t))
	defer httpSrv.Close()

	rec := &recordingRoundTripper{}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   httpSrv.URL,
		HTTPClient: &http.Client{Transport: rec},
	}, nil)
	require.NoError(t, err)
	defer clientSession.Close()

	assert.Equal(t, "2026-07-28", clientSession.InitializeResult().ProtocolVersion,
		"stateless handler must negotiate the 2026-07-28 revision with a v1.7 client")

	toolsRes, err := clientSession.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, toolsRes.Tools, 1)
	assert.Equal(t, "root", toolsRes.Tools[0].Name)

	methods := rec.recordedMethods()
	assert.Contains(t, methods, "server/discover",
		"a v1.7 client must negotiate via server/discover")
	assert.NotContains(t, methods, "initialize",
		"the client must not fall back to the legacy initialize handshake")
	assert.Empty(t, rec.recordedSessionIDs(),
		"stateless responses must not carry an Mcp-Session-Id header")
}

// TestStreamableHTTPHandler_StatelessRejectsGETAndDELETE pins the stateless
// transport contract: only POST is served; GET (standalone SSE) and DELETE
// (session teardown) answer 405 with an explicit Allow header.
func TestStreamableHTTPHandler_StatelessRejectsGETAndDELETE(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	httpSrv := httptest.NewServer(newTestHTTPHandler(t))
	defer httpSrv.Close()

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequestWithContext(t.Context(), method, httpSrv.URL, http.NoBody)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, method)
		assert.Equal(t, "POST", resp.Header.Get("Allow"), method)
	}
}

// TestStreamableHTTPHandler_LegacyInitializeStillAccepted drives the raw
// legacy handshake a pre-2026-07-28 client performs: initialize, the
// initialized notification, then tools/list — all without a session ID. The
// stateless handler must synthesize request-local state and serve them.
func TestStreamableHTTPHandler_LegacyInitializeStillAccepted(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	httpSrv := httptest.NewServer(newTestHTTPHandler(t))
	defer httpSrv.Close()

	initResp := postJSONRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"legacy-client","version":"0.0.1"}}}`)
	require.Empty(t, string(initResp.rpcError), "unexpected JSON-RPC error")
	assert.Empty(t, initResp.header.Get("Mcp-Session-Id"),
		"stateless initialize must not assign a session")
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	require.NoError(t, json.Unmarshal(initResp.result, &initResult))
	assert.Equal(t, "2025-06-18", initResult.ProtocolVersion,
		"legacy initialize must keep the client's protocol revision")

	// notifications/initialized has no id; a 202 acknowledges it.
	notifResp := doJSONRPCRequest(t, httpSrv.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	notifResp.Body.Close()
	assert.Equal(t, http.StatusAccepted, notifResp.StatusCode)

	listResp := postJSONRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	require.Empty(t, string(listResp.rpcError), "unexpected JSON-RPC error")
	var listResult struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(listResp.result, &listResult))
	require.Len(t, listResult.Tools, 1,
		"tools/list without a session ID must be served with synthesized state")
	assert.Equal(t, "root", listResult.Tools[0].Name)
}

type jsonRPCResponse struct {
	header   http.Header
	result   json.RawMessage
	rpcError json.RawMessage
}

func doJSONRPCRequest(t *testing.T, url, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// postJSONRPC sends a raw JSON-RPC request and decodes the single response
// message, which the handler delivers as JSON or as an SSE stream.
func postJSONRPC(t *testing.T, url, body string) jsonRPCResponse {
	t.Helper()

	resp := doJSONRPCRequest(t, url, body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload []byte
	contentType := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(contentType, "text/event-stream"):
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				payload = []byte(data)
				break
			}
		}
		require.NoError(t, scanner.Err())
	case strings.HasPrefix(contentType, "application/json"):
		var err error
		payload, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
	default:
		t.Fatalf("unexpected Content-Type %q", contentType)
	}
	require.NotEmpty(t, payload, "no JSON-RPC message in response")

	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(payload, &msg))
	return jsonRPCResponse{header: resp.Header, result: msg.Result, rpcError: msg.Error}
}

// TestStreamableHTTPHandler_StatelessToolsCallInvalidInput drives a real
// stateless tools/call through the production handler. The arguments fail
// input-schema validation (message must be a string), so the transport and
// RPC dispatch run end to end while the agent handler — and any model call —
// is never reached.
func TestStreamableHTTPHandler_StatelessToolsCallInvalidInput(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	httpSrv := httptest.NewServer(newTestHTTPHandler(t))
	defer httpSrv.Close()

	resp := postJSONRPC(t, httpSrv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"root","arguments":{"message":123}}}`)
	require.Empty(t, string(resp.rpcError),
		"input validation failures must surface as a tool error result, not a protocol error")
	assert.Empty(t, resp.header.Get("Mcp-Session-Id"),
		"stateless tools/call must not assign a session")

	var callResult struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(resp.result, &callResult))
	assert.True(t, callResult.IsError)
	require.NotEmpty(t, callResult.Content)
	assert.Contains(t, callResult.Content[0].Text, `validating "arguments"`)
}

// TestStreamableHTTPHandler_ConcurrentStatelessClients runs several
// independent clients against the shared production *mcp.Server at once;
// with -race this pins the stateless handler's thread-safety.
func TestStreamableHTTPHandler_ConcurrentStatelessClients(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	httpSrv := httptest.NewServer(newTestHTTPHandler(t))
	defer httpSrv.Close()

	g, ctx := errgroup.WithContext(t.Context())
	for range 8 {
		g.Go(func() error {
			client := mcp.NewClient(&mcp.Implementation{Name: "concurrent-client", Version: "0.0.1"}, nil)
			clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
			if err != nil {
				return err
			}
			defer clientSession.Close()
			res, err := clientSession.ListTools(ctx, nil)
			if err != nil {
				return err
			}
			if len(res.Tools) != 1 || res.Tools[0].Name != "root" {
				return fmt.Errorf("unexpected tools: %+v", res.Tools)
			}
			return nil
		})
	}
	require.NoError(t, g.Wait())
}

// TestAgentToolAnnotationsJSONKeepsFalseHints pins the SDK v1.7 (spec
// 2026-07-28) serialization change: false readOnlyHint and idempotentHint
// are emitted explicitly instead of omitted.
func TestAgentToolAnnotationsJSONKeepsFalseHints(t *testing.T) {
	t.Parallel()

	ag := agent.New("test", "test agent", agent.WithTools(
		tools.Tool{Name: "writer", Annotations: annot(false, false, nil, nil)},
	))
	annotations, err := agentToolAnnotations(t.Context(), ag)
	require.NoError(t, err)
	require.False(t, annotations.ReadOnlyHint)
	require.False(t, annotations.IdempotentHint)

	data, err := json.Marshal(annotations)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"readOnlyHint":false`)
	assert.Contains(t, string(data), `"idempotentHint":false`)
}

func TestNewToolCallSession(t *testing.T) {
	t.Parallel()

	ag := agent.New("root", "test agent", agent.WithMaxIterations(7))
	sess := newToolCallSession(ag, "hello", session.SafetyPolicyAutonomous, "/srv/workspace")

	assert.Equal(t, "MCP tool call", sess.Title)
	assert.Equal(t, 7, sess.MaxIterations)
	assert.True(t, sess.ToolsApproved)
	assert.True(t, sess.NonInteractive)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.Equal(t, "hello", sess.GetLastUserMessageContent())
	assert.Equal(t, "/srv/workspace", sess.WorkingDir)
}
