package mcp

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// startMCPServer creates a minimal MCP server on addr with the given tools
// and returns a function to shut it down.
func startMCPServer(t *testing.T, addr string, mcpTools ...*gomcp.Tool) (shutdown func()) {
	t.Helper()

	s := gomcp.NewServer(&gomcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	for _, tool := range mcpTools {
		s.AddTool(tool, func(_ context.Context, _ *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "ok-" + tool.Name}},
			}, nil
		})
	}

	// Retry Listen until the port is available (e.g. after a server shutdown).
	var srvLn net.Listener
	require.Eventually(t, func() bool {
		var listenErr error
		srvLn, listenErr = net.Listen("tcp", addr)
		return listenErr == nil
	}, 2*time.Second, 50*time.Millisecond, "port %s not available in time", addr)

	srv := &http.Server{
		Handler: gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return s }, nil),
	}
	go func() { _ = srv.Serve(srvLn) }()

	return func() { _ = srv.Close() }
}

// allocateAddr returns a free TCP address on localhost.
func allocateAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// unavailableThenAvailableMock is a mock client that returns io.EOF from
// Initialize for a configurable number of calls, then starts succeeding.
// This simulates a binary that is not installed at agent startup but later
// becomes available on PATH.
type unavailableThenAvailableMock struct {
	reconnectableMockClient

	initCount     atomic.Int32
	availableFrom int32 // Initialize succeeds starting at this call number (1-based)
	listToolsFn   func(context.Context, *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error]
}

func newUnavailableThenAvailableMock(availableFrom int32) *unavailableThenAvailableMock {
	m := &unavailableThenAvailableMock{
		availableFrom: availableFrom,
	}
	m.waitCh = make(chan struct{})
	return m
}

func (m *unavailableThenAvailableMock) Initialize(ctx context.Context, req *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	n := m.initCount.Add(1)
	if n < m.availableFrom {
		return nil, io.EOF
	}
	return m.reconnectableMockClient.Initialize(ctx, req)
}

func (m *unavailableThenAvailableMock) ListTools(ctx context.Context, req *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error] {
	if m.listToolsFn != nil {
		return m.listToolsFn(ctx, req)
	}
	return m.reconnectableMockClient.ListTools(ctx, req)
}

// TestStartReturnsErrorWhenBinaryUnavailable verifies that Start() returns
// errServerUnavailable when the MCP binary is not found (EOF from Initialize).
// The toolset must NOT be marked as started so the caller can retry later.
func TestStartReturnsErrorWhenBinaryUnavailable(t *testing.T) {
	t.Parallel()

	mock := newUnavailableThenAvailableMock(1_000_000) // never available

	ts := &Toolset{
		mcpClient: mock,
		logID:     "test-unavailable",
	}

	err := ts.Start(t.Context())
	require.ErrorIs(t, err, errServerUnavailable)

	ts.mu.Lock()
	assert.False(t, ts.started, "should not be started when binary is unavailable")
	ts.mu.Unlock()
}

// TestLazyRetryStartSucceedsWhenBinaryAppears verifies the lazy retry
// pattern: Start() fails on the first call because the binary is missing,
// then succeeds on a subsequent call once the binary is installed.
// This mirrors the agent runtime's ensureToolSetsAreStarted() which calls
// Start() at the beginning of every conversation turn.
func TestLazyRetryStartSucceedsWhenBinaryAppears(t *testing.T) {
	t.Parallel()

	expectedTool := &gomcp.Tool{Name: "greet", InputSchema: &jsonschema.Schema{Type: "object"}}

	mock := newUnavailableThenAvailableMock(2) // succeed from 2nd Initialize call
	mock.listToolsFn = func(_ context.Context, _ *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error] {
		return func(yield func(*gomcp.Tool, error) bool) {
			yield(expectedTool, nil)
		}
	}

	ts := &Toolset{
		mcpClient: mock,
		logID:     "test-lazy-retry",
	}

	// Turn 1: binary not installed yet — Start fails.
	err := ts.Start(t.Context())
	require.ErrorIs(t, err, errServerUnavailable)

	ts.mu.Lock()
	assert.False(t, ts.started)
	ts.mu.Unlock()

	// Turn 2: binary is now installed — Start succeeds.
	err = ts.Start(t.Context())
	require.NoError(t, err)

	ts.mu.Lock()
	assert.True(t, ts.started)
	ts.mu.Unlock()

	// Tools are now available.
	toolList, toolErr := ts.Tools(t.Context())
	require.NoError(t, toolErr)
	require.Len(t, toolList, 1)
	assert.Equal(t, "greet", toolList[0].Name)

	_ = ts.Stop(t.Context())
}

// TestStartableToolSetRetryOnNextTurn verifies the full integration with
// StartableToolSet: when the inner MCP Start() fails with errServerUnavailable,
// StartableToolSet does not mark itself as started. On the next turn
// (simulated by calling Start() again), the retry succeeds and tools appear.
func TestStartableToolSetRetryOnNextTurn(t *testing.T) {
	t.Parallel()

	expectedTool := &gomcp.Tool{Name: "hello", InputSchema: &jsonschema.Schema{Type: "object"}}

	mock := newUnavailableThenAvailableMock(3) // succeed from 3rd Initialize call
	mock.listToolsFn = func(_ context.Context, _ *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error] {
		return func(yield func(*gomcp.Tool, error) bool) {
			yield(expectedTool, nil)
		}
	}

	inner := &Toolset{
		mcpClient: mock,
		logID:     "test-startable",
	}
	startable := tools.NewStartable(inner)

	// Turn 1: unavailable.
	err := startable.Start(t.Context())
	require.Error(t, err)
	assert.False(t, startable.IsStarted())

	// Turn 2: still unavailable.
	err = startable.Start(t.Context())
	require.Error(t, err)
	assert.False(t, startable.IsStarted())

	// Turn 3: binary appeared.
	err = startable.Start(t.Context())
	require.NoError(t, err)
	assert.True(t, startable.IsStarted())

	toolList, toolErr := startable.Tools(t.Context())
	require.NoError(t, toolErr)
	require.Len(t, toolList, 1)
	assert.Equal(t, "hello", toolList[0].Name)

	_ = startable.Stop(t.Context())
}

// TestRemoteReconnectAfterServerRestart verifies that a Toolset backed by a
// real remote (streamable-HTTP) MCP server transparently recovers when the
// server is restarted.
//
// The scenario:
//  1. Start a minimal MCP server with a "ping" tool.
//  2. Connect a Toolset, call "ping" — succeeds.
//  3. Shut down the server (simulates crash / restart).
//  4. Start a **new** server on the same address.
//  5. Call "ping" again — this must succeed after automatic reconnection.
//
// Without the ErrSessionMissing recovery logic the second call would fail
// because the new server does not know the old session ID.
func TestRemoteReconnectAfterServerRestart(t *testing.T) {
	t.Parallel()

	addr := allocateAddr(t)

	var callCount atomic.Int32

	// startServer creates a minimal MCP server on addr with a "ping" tool
	// and returns a function to shut it down.
	startServer := func(t *testing.T) (shutdown func()) {
		t.Helper()

		s := gomcp.NewServer(&gomcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
		s.AddTool(&gomcp.Tool{
			Name:        "ping",
			InputSchema: &jsonschema.Schema{Type: "object"},
		}, func(_ context.Context, _ *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			n := callCount.Add(1)
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: fmt.Sprintf("pong-%d", n)}},
			}, nil
		})

		// Retry Listen until the port is available (e.g. after a server shutdown).
		var srvLn net.Listener
		require.Eventually(t, func() bool {
			var listenErr error
			srvLn, listenErr = net.Listen("tcp", addr)
			return listenErr == nil
		}, 2*time.Second, 50*time.Millisecond, "port %s not available in time", addr)

		srv := &http.Server{
			Handler: gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return s }, nil),
		}
		go func() { _ = srv.Serve(srvLn) }()

		return func() { _ = srv.Close() }
	}

	callPing := func(t *testing.T, ts *Toolset) string {
		t.Helper()
		result, callErr := ts.callTool(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{Name: "ping", Arguments: "{}"},
		})
		require.NoError(t, callErr)
		return result.Output
	}

	// --- Step 1–2: Start first server, connect toolset ---
	shutdown1 := startServer(t)

	ts := NewRemoteToolset("test", fmt.Sprintf("http://%s/mcp", addr), "streamable-http", nil)
	require.NoError(t, ts.Start(t.Context()))

	toolList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 1)
	assert.Equal(t, "test_ping", toolList[0].Name)

	// --- Step 3: Call succeeds on original server ---
	assert.Equal(t, "pong-1", callPing(t, ts))

	// --- Step 4: Shut down the server ---
	shutdown1()

	// Capture the current restarted channel before the reconnect
	ts.mu.Lock()
	restartedCh := ts.restarted
	ts.mu.Unlock()

	// --- Step 5–6: Start a fresh server, call again ---
	shutdown2 := startServer(t)
	t.Cleanup(func() {
		_ = ts.Stop(t.Context())
		shutdown2()
	})

	// This call triggers ErrSessionMissing recovery and must succeed transparently.
	assert.Equal(t, "pong-2", callPing(t, ts))

	// Verify that watchConnection actually restarted the connection by checking
	// that the restarted channel was closed (signaling reconnect completion).
	select {
	case <-restartedCh:
		// Success: the channel was closed, meaning reconnect happened
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reconnect did not complete: restarted channel was not closed")
	}
}

// TestRemoteReconnectRefreshesTools verifies that after a remote MCP server
// restarts with a different set of tools, the Toolset picks up the new tools
// and notifies the runtime via the toolsChangedHandler.
//
// This is the scenario from https://github.com/docker/docker-agent/issues/2244:
//   - Server v1 exposes tools [alpha, shared].
//   - Client connects and caches [alpha, shared].
//   - Server v1 shuts down; server v2 starts with tools [beta, shared].
//   - A tool call to "shared" triggers reconnection.
//   - After reconnection, Tools() must return [beta, shared], not the stale [alpha, shared].
//   - The toolsChangedHandler must be called so the runtime refreshes its own state.
func TestRemoteReconnectRefreshesTools(t *testing.T) {
	t.Parallel()

	addr := allocateAddr(t)

	// "shared" exists on both servers so we can call it to trigger reconnect.
	sharedTool := &gomcp.Tool{Name: "shared", InputSchema: &jsonschema.Schema{Type: "object"}}
	alphaTool := &gomcp.Tool{Name: "alpha", InputSchema: &jsonschema.Schema{Type: "object"}}
	betaTool := &gomcp.Tool{Name: "beta", InputSchema: &jsonschema.Schema{Type: "object"}}

	// --- Start server v1 with tools "alpha" + "shared" ---
	shutdown1 := startMCPServer(t, addr, alphaTool, sharedTool)

	ts := NewRemoteToolset("ns", fmt.Sprintf("http://%s/mcp", addr), "streamable-http", nil)

	// Track toolsChangedHandler invocations.
	toolsChangedCh := make(chan struct{}, 1)
	ts.SetToolsChangedHandler(func() {
		select {
		case toolsChangedCh <- struct{}{}:
		default:
		}
	})

	require.NoError(t, ts.Start(t.Context()))

	// Verify initial tools.
	toolList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 2)
	toolNames := []string{toolList[0].Name, toolList[1].Name}
	assert.Contains(t, toolNames, "ns_alpha")
	assert.Contains(t, toolNames, "ns_shared")

	// --- Shut down server v1, start server v2 with tools "beta" + "shared" ---
	shutdown1()

	shutdown2 := startMCPServer(t, addr, betaTool, sharedTool)
	t.Cleanup(func() {
		_ = ts.Stop(t.Context())
		shutdown2()
	})

	// Call "shared" to trigger ErrSessionMissing → reconnect.
	result, callErr := ts.callTool(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{Name: "shared", Arguments: "{}"},
	})
	require.NoError(t, callErr)
	assert.Equal(t, "ok-shared", result.Output)

	// Wait for the toolsChangedHandler to be called (signals reconnect + refresh).
	select {
	case <-toolsChangedCh:
		// Good — the handler was called.
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for toolsChangedHandler after reconnect")
	}

	// Verify the toolset now reports the new server's tools.
	toolList, err = ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 2, "expected exactly two tools from the new server")
	toolNames = []string{toolList[0].Name, toolList[1].Name}
	assert.Contains(t, toolNames, "ns_beta", "expected the new server's tool, got stale tool")
	assert.Contains(t, toolNames, "ns_shared")
	assert.NotContains(t, toolNames, "ns_alpha", "stale tool from old server should not be present")
}
