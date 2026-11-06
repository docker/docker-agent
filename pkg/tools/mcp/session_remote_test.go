package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/upstream"
)

func localSessionRemote(t *testing.T, endpoint, transport string, headers map[string]string) *Toolset {
	t.Helper()
	ts := NewSessionRemoteToolset("client", endpoint, transport, headers)
	ts.mcpClient.(*sessionRemoteClient).base = &http.Transport{Proxy: nil}
	t.Cleanup(func() { _ = ts.Stop(context.WithoutCancel(t.Context())) })
	return ts
}

func sessionRemoteHandler(transport string, server *gomcp.Server) http.Handler {
	get := func(*http.Request) *gomcp.Server { return server }
	if transport == "sse" {
		return gomcp.NewSSEHandler(get, nil)
	}
	return gomcp.NewStreamableHTTPHandler(get, nil)
}

func TestSessionRemoteLifetimeAndLiteralHeaders(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"streamable", "sse"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			server := gomcp.NewServer(&gomcp.Implementation{Name: "remote", Version: "1"}, &gomcp.ServerOptions{Instructions: "remote instructions"})
			gomcp.AddTool(server, &gomcp.Tool{Name: "inspect"}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, any, error) {
				return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "remote result"}}}, nil, nil
			})
			var calls atomic.Int32
			var mu sync.Mutex
			cookies := map[string]bool{}
			handler := sessionRemoteHandler(transport, server)
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "${headers.Authorization} ${env.TOKEN} op://vault/secret", r.Header.Get("Authorization"))
				value := r.Header.Get("X-Session")
				cookie, err := r.Cookie("owner")
				if err == nil {
					assert.Equal(t, value, cookie.Value)
					mu.Lock()
					cookies[value] = true
					mu.Unlock()
				}
				http.SetCookie(w, &http.Cookie{Name: "owner", Value: value, Path: "/"})
				calls.Add(1)
				handler.ServeHTTP(w, r)
			}))
			defer hs.Close()
			first := localSessionRemote(t, hs.URL, transport, map[string]string{"Authorization": "${headers.Authorization} ${env.TOKEN} op://vault/secret", "X-Session": "first"})
			second := localSessionRemote(t, hs.URL, transport, map[string]string{"Authorization": "${headers.Authorization} ${env.TOKEN} op://vault/secret", "X-Session": "second"})
			// Stop before closing the server's long-lived SSE responses.
			defer func() { require.NoError(t, first.Stop(t.Context())); require.NoError(t, second.Stop(t.Context())) }()
			ctx, cancel := context.WithCancel(upstream.WithHeaders(t.Context(), http.Header{"Authorization": {"host secret"}}))
			require.NoError(t, first.Start(ctx))
			cancel()
			require.NoError(t, second.Start(t.Context()))
			for _, ts := range []*Toolset{first, second} {
				available, err := ts.Tools(t.Context())
				require.NoError(t, err)
				require.Len(t, available, 1)
				result, err := available[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: available[0].Name, Arguments: "{}"}}, tools.NopRuntime{})
				require.NoError(t, err)
				assert.Equal(t, "remote result", result.Output)
				assert.Equal(t, "remote instructions", ts.Instructions())
			}
			assert.Positive(t, calls.Load())
			mu.Lock()
			defer mu.Unlock()
			assert.True(t, cookies["first"])
			assert.True(t, cookies["second"])
		})
	}
}

func TestSessionRemoteCancelsSetup(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"headers", "endpoint", "initialize", "subscription"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			entered := make(chan struct{})
			var once sync.Once
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "subscription" {
					var request struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					if request.Method == "server/discover" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, request.ID)
						return
					}
					assert.Equal(t, "subscriptions/listen", request.Method)
				}
				if phase == "endpoint" || phase == "initialize" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				once.Do(func() { close(entered) })
				<-r.Context().Done()
			}))
			defer hs.Close()
			transport := "streamable"
			if phase == "headers" || phase == "endpoint" {
				transport = "sse"
			}
			ts := localSessionRemote(t, hs.URL, transport, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- ts.Start(ctx) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("setup did not enter request")
			}
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("canceled setup did not drain")
			}
		})
	}
}

func TestSessionRemoteRejectsAuthRedirectsAndDiscoveredOrigins(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"401", "redirect", "endpoint", "userinfo"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var leaked atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { leaked.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
			defer destination.Close()
			var requests atomic.Int32
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch kind {
				case "401":
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+destination.URL+`"`)
					w.WriteHeader(http.StatusUnauthorized)
				case "redirect":
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
				default:
					endpoint := destination.URL
					if kind == "userinfo" {
						endpoint = "http://user:secret@" + r.Host + "/messages"
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer hs.Close()
			ts := localSessionRemote(t, hs.URL+"?api_key=private-query", "sse", map[string]string{"X-Secret": "private-header"})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := ts.Start(ctx)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-query")
			assert.NotContains(t, err.Error(), "private-header")
			assert.EqualValues(t, 0, leaked.Load())
			assert.EqualValues(t, 1, requests.Load(), "no OAuth discovery, redirect fetch, or same-origin userinfo POST")
		})
	}
}

func TestSessionRemotePrivateNetworkPolicy(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer hs.Close()
	ts := NewSessionRemoteToolset("restricted", hs.URL, "sse", nil)
	defer func() { require.NoError(t, ts.Stop(t.Context())) }()
	require.Error(t, ts.Start(t.Context()))
	assert.Zero(t, requests.Load())
}

func TestSessionRemoteCloseDeadlineAndNoReconnect(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"streamable", "sse"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			server := gomcp.NewServer(&gomcp.Implementation{Name: "close", Version: "1"}, nil)
			called := make(chan struct{})
			release := make(chan struct{})
			gomcp.AddTool(server, &gomcp.Tool{Name: "wait"}, func(ctx context.Context, _ *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
				close(called)
				select {
				case <-ctx.Done():
				case <-release:
				}
				return nil, nil, ctx.Err()
			})
			var requests atomic.Int32
			handler := sessionRemoteHandler(transport, server)
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); handler.ServeHTTP(w, r) }))
			defer hs.Close()
			defer close(release)
			ts := localSessionRemote(t, hs.URL, transport, nil)
			require.NoError(t, ts.Start(t.Context()))
			available, err := ts.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, available, 1)
			done := make(chan error, 1)
			go func() {
				_, err := available[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: available[0].Name, Arguments: "{}"}}, tools.NopRuntime{})
				done <- err
			}()
			select {
			case <-called:
			case <-time.After(5 * time.Second):
				t.Fatal("tool not called")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_ = ts.Stop(ctx)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("close did not join call")
			}
			before := requests.Load()
			_, err = available[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: available[0].Name, Arguments: "{}"}}, tools.NopRuntime{})
			require.Error(t, err)
			assert.Equal(t, before, requests.Load())
		})
	}
}

func TestSessionRemoteHTTPBodyCancellationAndOrigin(t *testing.T) {
	t.Parallel()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer hs.Close()
	ctx, cancel := context.WithCancel(t.Context())
	h := &sessionRemoteHTTP{ctx: ctx, endpoint: hs.URL, base: newTestTransport(t)}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL, http.NoBody)
	require.NoError(t, err)
	response, err := h.RoundTrip(request)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(response.Body)
	require.Error(t, err)
	require.NoError(t, response.Body.Close())
	h.wait()
	response, err = h.RoundTrip(request)
	if response != nil {
		_ = response.Body.Close()
	}
	require.ErrorIs(t, err, context.Canceled)
	for _, target := range []string{"https://example.org/", strings.Replace(hs.URL, "http://", "http://user:secret@", 1), hs.URL + "#fragment"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
		require.NoError(t, err)
		response, err = h.RoundTrip(request)
		if response != nil {
			_ = response.Body.Close()
		}
		require.ErrorContains(t, err, "origin")
	}
	assert.Equal(t, "client remote MCP request failed", sessionRemoteError(errors.New("secret URL")).Error())
}

func TestSessionRemoteLegacyDelete(t *testing.T) {
	t.Parallel()
	for _, stall := range []bool{false, true} {
		t.Run(strconv.FormatBool(stall), func(t *testing.T) {
			t.Parallel()
			deleted := make(chan struct{})
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					assert.Equal(t, "owned", r.Header.Get("Mcp-Session-Id"))
					close(deleted)
					if stall {
						<-r.Context().Done()
					} else {
						w.WriteHeader(http.StatusOK)
					}
					return
				}
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch req.Method {
				case "server/discover":
					_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"legacy"}}`, req.ID)
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "owned")
					_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"legacy","version":"1"}}}`, req.ID)
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			defer hs.Close()
			ts := NewSessionRemoteToolset("legacy", hs.URL, "streamable", nil)
			ts.mcpClient.(*sessionRemoteClient).base = &http.Transport{Proxy: nil}
			require.NoError(t, ts.Start(t.Context()))
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			err := ts.Stop(ctx)
			if stall {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, err)
			}
			select {
			case <-deleted:
			default:
				t.Fatal("graceful DELETE never attempted")
			}
		})
	}
}

func TestSessionRemoteFailedSubscriptionCleanup(t *testing.T) {
	t.Parallel()
	breakStream := make(chan struct{})
	subscriptionCtx := make(chan context.Context, 1)
	var requests atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, req.ID)
			return
		}
		if req.Method == "subscriptions/listen" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-breakStream
			_, _ = fmt.Fprint(w, "event: message\ndata: invalid-json\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hs.Close()
	ts := localSessionRemote(t, hs.URL, "streamable", nil)
	c := ts.mcpClient.(*sessionRemoteClient)
	base := c.base
	c.base = remoteRoundTripper(func(r *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		if strings.Contains(string(data), `"subscriptions/listen"`) {
			subscriptionCtx <- r.Context()
		}
		return base.RoundTrip(r)
	})
	require.NoError(t, ts.Start(t.Context()))
	ctx := <-subscriptionCtx
	close(breakStream)
	require.Eventually(t, func() bool { return !ts.IsStarted() }, time.Second, time.Millisecond)
	_ = ts.Stop(t.Context())
	select {
	case <-ctx.Done():
	default:
		t.Fatal("subscription request still live after failed connection cleanup")
	}
	c.http.mu.Lock()
	assert.True(t, c.http.closed, "failed connections must drain HTTP ownership")
	c.http.mu.Unlock()
	assert.EqualValues(t, 2, requests.Load(), "failed stream must not reconnect")
}

type remoteRoundTripper func(*http.Request) (*http.Response, error)

func (f remoteRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionRemoteDoesNotResumeInterruptedStream(t *testing.T) {
	t.Parallel()
	var gets atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "server/discover":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, req.ID)
		case "tools/list":
			// A resumable stream ends before its RPC result arrives.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "id: resume-token\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"1\",\"progress\":0}}\n\n")
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer hs.Close()
	ts := localSessionRemote(t, hs.URL, "streamable", nil)
	require.NoError(t, ts.Start(t.Context()))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := ts.Tools(ctx)
	require.Error(t, err)
	assert.Zero(t, gets.Load(), "SDK must not resume a client-owned SSE stream")
}

func TestSessionRemoteStartAfterStop(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"streamable", "sse"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			server := gomcp.NewServer(&gomcp.Implementation{Name: "resume", Version: "1"}, nil)
			gomcp.AddTool(server, &gomcp.Tool{Name: "ping"}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, any, error) {
				return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "pong"}}}, nil, nil
			})
			hs := httptest.NewServer(sessionRemoteHandler(transport, server))
			defer hs.Close()
			ts := localSessionRemote(t, hs.URL, transport, nil)
			defer func() { require.NoError(t, ts.Stop(t.Context())) }()
			for range 3 {
				require.NoError(t, ts.Start(t.Context()))
				available, err := ts.Tools(t.Context())
				require.NoError(t, err)
				require.Len(t, available, 1)
				result, err := available[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: available[0].Name, Arguments: "{}"}}, tools.NopRuntime{})
				require.NoError(t, err)
				assert.Equal(t, "pong", result.Output)
				require.NoError(t, ts.Stop(t.Context()))
			}
		})
	}
}
