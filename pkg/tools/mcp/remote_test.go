package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSanitizeRemoteAddress verifies that URLs with embedded credentials
// (basic-auth userinfo, query-string secrets) collapse to a host-only
// string before reaching the `server.address` span attribute. The point
// is exfiltration safety: a URL like `https://user:token@host/?api_key=…`
// would otherwise be replicated verbatim into every CLIENT span and
// shipped to the trace backend.
func TestSanitizeRemoteAddress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{name: "plain", url: "https://example.com/mcp", want: "example.com"},
		{name: "host with port", url: "https://example.com:8443/mcp", want: "example.com:8443"},
		{name: "userinfo stripped", url: "https://alice:s3cret@example.com/mcp", want: "example.com"},
		{name: "query stripped", url: "https://example.com/mcp?api_key=s3cret", want: "example.com"},
		{name: "userinfo and query stripped", url: "https://alice:s3cret@example.com:8443/mcp?api_key=x", want: "example.com:8443"},
		{name: "fragment stripped", url: "https://example.com/mcp#frag", want: "example.com"},
		{name: "hostless empty fallback", url: "not-a-url", want: ""},
		{name: "empty input", url: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeRemoteAddress(tc.url)
			assert.Equal(t, tc.want, got, "sanitizeRemoteAddress(%q)", tc.url)
		})
	}
}

// TestRemoteClientCustomHeaders verifies that custom headers passed to the remote
// MCP client are actually applied to HTTP requests sent to the MCP server.
func TestRemoteClientCustomHeaders(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	// Create a test SSE server that captures the request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		// Send a minimal SSE response to satisfy the client
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: {\"uri\":\"/message\"}\n\n")

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client WITH custom headers
	expectedHeaders := map[string]string{
		"X-Test-Header": "test-value",
		"X-API-Key":     "secret-key-12345",
		"Authorization": "Bearer custom-token",
	}

	client := newRemoteClient(server.URL, "sse", expectedHeaders, NewInMemoryTokenStore(), nil, false)

	// Try to initialize (which will make the HTTP request)
	// We don't care if it succeeds or fails, we just need it to make the request
	_, _ = client.Initialize(t.Context(), nil)

	// Wait for the request to be captured
	select {
	case <-requestCaptured:
		// Verify that custom headers were applied
		for key, expectedValue := range expectedHeaders {
			actualValue := capturedRequest.Header.Get(key)
			assert.Equal(t, expectedValue, actualValue,
				"Expected header %s to have value %q, but got %q",
				key, expectedValue, actualValue)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestRemoteClientHeadersWithStreamable verifies that custom headers work with streamable transport
func TestRemoteClientHeadersWithStreamable(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	// Create a test server for streamable transport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		// Send a minimal response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","result":{"protocolVersion":"1.0.0","capabilities":{},"serverInfo":{"name":"test","version":"1.0.0"}},"id":1}`)

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client WITH custom headers using streamable transport
	expectedHeaders := map[string]string{
		"X-Custom-Auth": "custom-auth-value",
	}

	client := newRemoteClient(server.URL, "streamable", expectedHeaders, NewInMemoryTokenStore(), nil, false)

	// Try to initialize
	_, _ = client.Initialize(t.Context(), nil)

	// Wait for the request to be captured
	select {
	case <-requestCaptured:
		// Verify that custom headers were applied
		actualValue := capturedRequest.Header.Get("X-Custom-Auth")
		assert.Equal(t, "custom-auth-value", actualValue,
			"Expected header X-Custom-Auth to have value %q, but got %q",
			"custom-auth-value", actualValue)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestRemoteClientNoHeaders verifies that the client works correctly even with no headers
func TestRemoteClientNoHeaders(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: {\"uri\":\"/message\"}\n\n")

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client without custom headers (nil)
	client := newRemoteClient(server.URL, "sse", nil, NewInMemoryTokenStore(), nil, false)

	_, _ = client.Initialize(t.Context(), nil)

	// Wait for request
	select {
	case <-requestCaptured:
		// Just verify we got the request - no custom headers should be present
		require.NotNil(t, capturedRequest, "Request should have been captured")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestRemoteClientEmptyHeaders verifies that the client works correctly with an empty map
func TestRemoteClientEmptyHeaders(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: {\"uri\":\"/message\"}\n\n")

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client with empty headers map
	client := newRemoteClient(server.URL, "sse", map[string]string{}, NewInMemoryTokenStore(), nil, false)

	_, _ = client.Initialize(t.Context(), nil)

	// Wait for request
	select {
	case <-requestCaptured:
		// Just verify we got the request
		require.NotNil(t, capturedRequest, "Request should have been captured")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestOAuthHTTPClientWithHeaders_ScopesHeadersToMCPHost verifies that the
// OAuth HTTP client forwards configured custom headers to requests targeting
// the MCP server's own host — where protected-resource-metadata discovery is
// served (issue #3148) — but NOT to requests aimed at a different host, such
// as an authorization server advertised in the server's own metadata.
func TestOAuthHTTPClientWithHeaders_ScopesHeadersToMCPHost(t *testing.T) {
	t.Parallel()

	var mcpHostHeader, thirdPartyHeader string
	mcpHostHit := make(chan struct{}, 1)
	thirdPartyHit := make(chan struct{}, 1)

	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpHostHeader = r.Header.Get("X-Grafana-URL")
		w.WriteHeader(http.StatusOK)
		mcpHostHit <- struct{}{}
	}))
	defer mcpServer.Close()

	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdPartyHeader = r.Header.Get("X-Grafana-URL")
		w.WriteHeader(http.StatusOK)
		thirdPartyHit <- struct{}{}
	}))
	defer thirdParty.Close()

	headers := map[string]string{"X-Grafana-URL": "https://instance.grafana.net/"}
	client := oauthHTTPClientWithHeaders(mcpServer.URL, headers, true)

	mcpReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mcpServer.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(mcpReq)
	require.NoError(t, err)
	resp.Body.Close()
	<-mcpHostHit
	assert.Equal(t, "https://instance.grafana.net/", mcpHostHeader,
		"requests to the MCP server's own host must carry the configured header")

	thirdPartyReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, thirdParty.URL, http.NoBody)
	require.NoError(t, err)
	resp, err = client.Do(thirdPartyReq)
	require.NoError(t, err)
	resp.Body.Close()
	<-thirdPartyHit
	assert.Empty(t, thirdPartyHeader,
		"requests to a third-party host must NOT carry the configured header (credential-leak guard)")
}

// TestOAuthHTTPClientWithHeaders_NoHeadersReusesBaseClient verifies that with
// no configured headers the helper returns the shared SSRF-safe OAuth client
// unchanged — no wrapping, and the package singleton is never mutated.
func TestOAuthHTTPClientWithHeaders_NoHeadersReusesBaseClient(t *testing.T) {
	t.Parallel()

	got := oauthHTTPClientWithHeaders("https://mcp.example.com/mcp", nil, false)
	assert.Same(t, oauthHTTPClientForAllowPrivateIPs(false), got,
		"with no headers the helper must return the base OAuth client unchanged")
}

// TestInitialize_SurfacesServerErrorInReturnedError verifies that when an
// MCP server rejects the initialize call with a 4xx carrying a JSON-RPC
// error body, the error returned by Initialize contains the server's own
// explanation — not just the generic "Bad Request" from http.StatusText.
//
// Regression test for: Slack's MCP endpoint answering
//
//	400 Bad Request
//	{"jsonrpc":"2.0","id":null,"error":{"code":-32600,
//	 "message":"App is not enabled for Slack MCP server access. ..."}}
//
// where the bubbled-up error previously read only "...: Bad Request" and
// the user had no way to learn what was actually wrong.
func TestInitialize_SurfacesServerErrorInReturnedError(t *testing.T) {
	t.Parallel()

	const msg = "App is not enabled for Slack MCP server access."

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":%q}}`, msg)
	}))
	defer server.Close()

	// Pre-populate a token so the transport doesn't try to trigger OAuth on
	// the 401 path — we want to exercise the "server rejected us with a
	// non-auth failure" code path.
	store := NewInMemoryTokenStore()
	require.NoError(t, store.StoreToken(server.URL, &OAuthToken{AccessToken: "at", TokenType: "Bearer"}))

	client := newRemoteClient(server.URL, "streamable", nil, store, nil, false)

	_, err := client.Initialize(t.Context(), nil)
	require.Error(t, err, "Initialize should fail against a server that rejects initialize")
	assert.Contains(t, err.Error(), msg,
		"Initialize error must surface the server's JSON-RPC error message (%q), got: %v", msg, err)
	assert.Contains(t, err.Error(), "400",
		"Initialize error should include the HTTP status code so the user knows it was a server rejection, got: %v", err)
}

// TestInitialize_NonInteractiveCtxDefersOAuthAndDoesNotBlock verifies that
// when Initialize runs against a server that requires OAuth (responds with
// 401 + WWW-Authenticate) under a context flagged with
// WithoutInteractivePrompts, the call:
//
//   - returns promptly,
//   - returns an error that satisfies IsAuthorizationRequired,
//   - never opens a callback HTTP server (i.e. doesn't try to bind a port).
//
// Regression test for: "docker agent run ./examples/slack.yaml" hanging
// during startup. The TUI was not yet ready to render the OAuth dialog,
// the elicitation goroutine was blocked on a synchronous channel send,
// and Ctrl-C couldn't reach it.
func TestInitialize_NonInteractiveCtxDefersOAuthAndDoesNotBlock(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource="https://example.test/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newRemoteClient(server.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	nonInteractiveCtx := WithoutInteractivePrompts(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(nonInteractiveCtx, nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "Initialize should fail with a deferred-auth error in non-interactive ctx")
		assert.True(t, IsAuthorizationRequired(err),
			"non-interactive Initialize should return IsAuthorizationRequired, got: %v", err)
	case <-ctx.Done():
		t.Fatalf("Initialize blocked for too long; non-interactive ctx must short-circuit OAuth: %v", ctx.Err())
	}
}

// TestInitialize_OAuthDefersWhenElicitationBridgeNotReady verifies that
// when Initialize runs against a server that requires OAuth under a
// regular interactive context but no elicitation handler has been wired
// up yet (the runtime's configureToolsetHandlers hasn't run for this
// toolset), Initialize returns the same recognisable
// AuthorizationRequiredError as the explicit non-interactive deferral
// path — not an opaque "OAuth flow failed: ... no elicitation handler
// configured" message.
//
// Pairs with TestInitialize_NonInteractiveCtxDefersOAuthAndDoesNotBlock:
// that test exercises the explicit deferral via the
// WithoutInteractivePrompts marker; this one exercises the safety net
// for when the marker is missing (e.g. an early MCP probe issued from a
// code path that hasn't been taught about the marker yet) but the
// runtime hasn't attached its elicitation handler. In that situation
// the toolset must be quietly retried on the next conversation turn,
// when configureToolsetHandlers has wired everything up; surfacing a
// raw "no elicitation handler configured" error to the user
// communicates a confusing internal-state problem instead.
func TestInitialize_OAuthDefersWhenElicitationBridgeNotReady(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// 401 + WWW-Authenticate to drive the OAuth transport into the
		// elicitation step. The resource URL points back at our own server
		// so the metadata fetches don't blow up on DNS — we want the test
		// to actually reach the elicitation call so the no-handler branch
		// is exercised.
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource=%q`, srv.URL+"/.well-known/oauth-protected-resource"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	// 404 on every well-known endpoint so the OAuth flow falls through
	// to default metadata (no registration endpoint, no scopes) and gets
	// to the elicitation step quickly.
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	// Default newRemoteClient: managed=false, so the unmanaged OAuth
	// flow runs. That path reaches requestElicitation without needing
	// dynamic client registration, which keeps the test focused on the
	// bridge-not-ready behaviour.
	client := newRemoteClient(srv.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false)

	// Plain interactive ctx (no WithoutInteractivePrompts marker). The
	// elicitation handler is intentionally not wired up.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(ctx, nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "Initialize should fail with a deferred-auth error when no elicitation handler is wired up")
		assert.True(t, IsAuthorizationRequired(err),
			"Initialize must return AuthorizationRequiredError when the runtime hasn't attached an elicitation handler yet (so the toolset is silently retried on the next conversation turn instead of surfacing a confusing 'no elicitation handler configured' message); got: %v", err)
	case <-ctx.Done():
		t.Fatalf("Initialize blocked for too long: %v", ctx.Err())
	}
}

// TestCreateHTTPClient_PersistsCookies verifies that the *http.Client returned
// by createHTTPClient has a cookie jar, so sticky-session cookies set by the
// remote MCP ingress are echoed back on subsequent requests.
func TestCreateHTTPClient_PersistsCookies(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		switch n {
		case 1:
			if _, err := r.Cookie("mcp_session"); err == nil {
				t.Errorf("first request must not carry mcp_session cookie, got one")
			}
			w.Header().Set("Set-Cookie", "mcp_session=abc123; Path=/")
			w.WriteHeader(http.StatusOK)
		default:
			cookie := r.Header.Get("Cookie")
			if !strings.Contains(cookie, "mcp_session=abc123") {
				t.Errorf("subsequent request must carry mcp_session=abc123, got Cookie=%q", cookie)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := newRemoteClient(server.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false)
	httpClient, _, err := client.createHTTPClient()
	require.NoError(t, err)
	require.NotNil(t, httpClient.Jar, "createHTTPClient must attach a cookie jar so sticky sessions stick")

	req1, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp1, err := httpClient.Do(req1)
	require.NoError(t, err)
	_ = resp1.Body.Close()

	req2, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp2, err := httpClient.Do(req2)
	require.NoError(t, err)
	_ = resp2.Body.Close()

	require.Equal(t, int32(2), requestCount.Load(), "handler should have served both requests")
}

func TestNewRemoteToolsetWithAllowPrivateIPsPropagatesToClient(t *testing.T) {
	t.Parallel()

	ts := NewRemoteToolsetWithAllowPrivateIPs("internal", "https://mcp.example.com/mcp", "streamable", nil, nil, true)
	client, ok := ts.mcpClient.(*remoteMCPClient)
	require.True(t, ok, "remote toolset should use remoteMCPClient")
	require.True(t, client.allowPrivateIPs, "allow_private_ips should be stored on remote client")
}
