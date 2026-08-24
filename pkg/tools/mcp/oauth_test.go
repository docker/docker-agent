package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
)

// newTestTransport returns a transport private to the test. Parallel tests
// must not share http.DefaultTransport: httptest.Server.Close calls
// http.DefaultTransport.CloseIdleConnections, which can break another
// test's in-flight request ("transport connection broken").
func newTestTransport(t *testing.T) *http.Transport {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// TestExchangeCodeForToken_PreservesClientCredentials verifies that
// ExchangeCodeForToken stores the client_id and client_secret on the
// returned OAuthToken so they are available for subsequent refresh calls.
func TestExchangeCodeForToken_PreservesClientCredentials(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("client_id"); got != "my-client" {
			t.Errorf("client_id = %q, want %q", got, "my-client")
		}
		if got := r.FormValue("client_secret"); got != "my-secret" {
			t.Errorf("client_secret = %q, want %q", got, "my-secret")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-new",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "rt-new",
		})
	}))
	defer srv.Close()

	token, err := ExchangeCodeForToken(t.Context(), srv.URL, "code", "verifier", "my-client", "my-secret", "http://localhost/callback")
	if err != nil {
		t.Fatalf("ExchangeCodeForToken: %v", err)
	}

	if token.ClientID != "my-client" {
		t.Errorf("ClientID = %q, want %q", token.ClientID, "my-client")
	}
	if token.ClientSecret != "my-secret" {
		t.Errorf("ClientSecret = %q, want %q", token.ClientSecret, "my-secret")
	}
}

// TestRefreshAccessToken_PreservesClientCredentials verifies that
// RefreshAccessToken carries the client credentials through to the new token.
func TestRefreshAccessToken_PreservesClientCredentials(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("client_id"); got != "cid" {
			t.Errorf("client_id = %q, want %q", got, "cid")
		}
		if got := r.FormValue("client_secret"); got != "csec" {
			t.Errorf("client_secret = %q, want %q", got, "csec")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-refreshed",
			"token_type":   "Bearer",
			"expires_in":   7200,
			// Server does NOT return a new refresh_token – old one should be preserved.
		})
	}))
	defer srv.Close()

	token, err := RefreshAccessToken(t.Context(), srv.URL, "old-rt", "cid", "csec")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}

	if token.AccessToken != "at-refreshed" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "at-refreshed")
	}
	if token.RefreshToken != "old-rt" {
		t.Errorf("RefreshToken = %q, want %q (should be preserved)", token.RefreshToken, "old-rt")
	}
	if token.ClientID != "cid" {
		t.Errorf("ClientID = %q, want %q", token.ClientID, "cid")
	}
	if token.ClientSecret != "csec" {
		t.Errorf("ClientSecret = %q, want %q", token.ClientSecret, "csec")
	}
}

// TestGetValidToken_UsesStoredCredentialsForRefresh verifies that the
// oauthTransport.getValidToken method sends the stored client credentials
// when silently refreshing an expired token.
func TestGetValidToken_UsesStoredCredentialsForRefresh(t *testing.T) {
	t.Parallel()

	var receivedClientID, receivedClientSecret string

	// Use a mux so we can reference srv.URL in closures (srv is assigned before handlers run).
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"token_endpoint":         srv.URL + "/token",
			"authorization_endpoint": srv.URL + "/authorize",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		receivedClientID = r.FormValue("client_id")
		receivedClientSecret = r.FormValue("client_secret")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-at",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "fresh-rt",
		})
	})

	// Pre-populate an expired token with stored client credentials.
	store := NewInMemoryTokenStore()
	expiredToken := &OAuthToken{
		AccessToken:     "old-at",
		TokenType:       "Bearer",
		RefreshToken:    "old-rt",
		ExpiresAt:       time.Now().Add(-1 * time.Hour), // expired
		ClientID:        "stored-cid",
		ClientSecret:    "stored-csec",
		RequestedScopes: []string{"read", "write"},
	}
	if err := store.StoreToken(srv.URL, expiredToken); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    srv.URL,
	}

	got := transport.getValidToken(t.Context())
	require.NotNil(t, got, "getValidToken returned nil, expected refreshed token")
	if got.AccessToken != "fresh-at" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "fresh-at")
	}
	if receivedClientID != "stored-cid" {
		t.Errorf("token endpoint received client_id = %q, want %q", receivedClientID, "stored-cid")
	}
	if receivedClientSecret != "stored-csec" {
		t.Errorf("token endpoint received client_secret = %q, want %q", receivedClientSecret, "stored-csec")
	}

	// Verify the refreshed token also carries the credentials forward.
	updated, err := store.GetToken(srv.URL)
	if err != nil {
		t.Fatalf("GetToken after refresh: %v", err)
	}
	if updated.ClientID != "stored-cid" {
		t.Errorf("stored ClientID = %q, want %q", updated.ClientID, "stored-cid")
	}
	if updated.ClientSecret != "stored-csec" {
		t.Errorf("stored ClientSecret = %q, want %q", updated.ClientSecret, "stored-csec")
	}
	assert.Equal(t, []string{"read", "write"}, updated.RequestedScopes,
		"RequestedScopes must survive a silent refresh unchanged")
}

// TestGetValidToken_UsesStoredAuthServerForRefresh verifies that silent
// refresh uses the discovered auth server rather than assuming the MCP
// server URL also hosts the OAuth metadata.
func TestGetValidToken_UsesStoredAuthServerForRefresh(t *testing.T) {
	t.Parallel()

	var refreshRequests int

	authMux := http.NewServeMux()
	authSrv := httptest.NewServer(authMux)
	defer authSrv.Close()

	authMux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 authSrv.URL,
			"token_endpoint":         authSrv.URL + "/token",
			"authorization_endpoint": authSrv.URL + "/authorize",
		})
	})
	authMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		refreshRequests++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("refresh_token"); got != "old-rt" {
			t.Fatalf("refresh_token = %q, want %q", got, "old-rt")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-at",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "fresh-rt",
		})
	})

	mcpSrv := httptest.NewServer(http.NotFoundHandler())
	defer mcpSrv.Close()

	store := NewInMemoryTokenStore()
	expiredToken := &OAuthToken{
		AccessToken:  "old-at",
		TokenType:    "Bearer",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ClientID:     "stored-cid",
		ClientSecret: "stored-csec",
		AuthServer:   authSrv.URL,
	}
	if err := store.StoreToken(mcpSrv.URL, expiredToken); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    mcpSrv.URL,
	}

	got := transport.getValidToken(t.Context())
	require.NotNil(t, got, "getValidToken returned nil, expected refreshed token")
	if got.AccessToken != "fresh-at" {
		t.Fatalf("AccessToken = %q, want %q", got.AccessToken, "fresh-at")
	}
	if got.AuthServer != authSrv.URL {
		t.Fatalf("AuthServer = %q, want %q", got.AuthServer, authSrv.URL)
	}
	if refreshRequests != 1 {
		t.Fatalf("refreshRequests = %d, want 1", refreshRequests)
	}
}

// TestOAuthTokenClientCredentials_JSONRoundTrip verifies that ClientID and
// ClientSecret survive JSON serialization (important for keyring storage).
func TestOAuthTokenClientCredentials_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	token := &OAuthToken{
		AccessToken:  "at",
		TokenType:    "Bearer",
		RefreshToken: "rt",
		ExpiresIn:    3600,
		ExpiresAt:    time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
		ClientID:     "cid",
		ClientSecret: "csec",
		AuthServer:   "https://auth.example.com",
	}

	data, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got OAuthToken
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ClientID != "cid" {
		t.Errorf("ClientID = %q, want %q", got.ClientID, "cid")
	}
	if got.ClientSecret != "csec" {
		t.Errorf("ClientSecret = %q, want %q", got.ClientSecret, "csec")
	}
	if got.AuthServer != "https://auth.example.com" {
		t.Errorf("AuthServer = %q, want %q", got.AuthServer, "https://auth.example.com")
	}
}

// TestOAuthTokenClientCredentials_OmittedWhenEmpty verifies the omitempty
// tag works so tokens without client credentials don't leak empty fields.
func TestOAuthTokenClientCredentials_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	token := &OAuthToken{
		AccessToken: "at",
		TokenType:   "Bearer",
	}

	data, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	if _, ok := raw["client_id"]; ok {
		t.Error("client_id should be omitted when empty")
	}
	if _, ok := raw["client_secret"]; ok {
		t.Error("client_secret should be omitted when empty")
	}
}

// TestRefreshAccessToken_SendsEmptyClientIDWhenNotStored ensures that when
// no client credentials were stored (legacy tokens), the refresh still
// sends whatever was provided (empty string), matching the old behavior.
func TestRefreshAccessToken_SendsEmptyClientIDWhenNotStored(t *testing.T) {
	t.Parallel()

	var receivedForm url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		receivedForm = r.Form

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-at",
			"token_type":   "Bearer",
		})
	}))
	defer srv.Close()

	_, err := RefreshAccessToken(t.Context(), srv.URL, "rt", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// client_id is always sent (even empty) per the current implementation.
	if got := receivedForm.Get("client_id"); got != "" {
		t.Errorf("client_id = %q, want empty", got)
	}
	// client_secret should NOT be sent when empty.
	if receivedForm.Has("client_secret") {
		t.Error("client_secret should not be sent when empty")
	}
}

// TestCallbackServer_RejectsCallbackBeforeStateSet verifies that a callback
// arriving before SetExpectedState is called is rejected (CSRF protection).
func TestCallbackServer_RejectsCallbackBeforeStateSet(t *testing.T) {
	t.Parallel()

	cs, err := NewCallbackServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Shutdown(t.Context()) }()

	// Send a callback before SetExpectedState has been called.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cs.GetRedirectURI()+"?code=authcode&state=anything", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: newTestTransport(t)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d — callback accepted without expected state set", resp.StatusCode)
	}
}

// TestExchangeCodeForToken_SlackNestedResponse verifies that Slack's
// oauth.v2.user.access response shape (where the user access_token is
// nested inside an `authed_user` object) is decoded correctly. Before
// this was supported, we would silently store an empty bearer token and
// every subsequent request to the MCP server would be rejected with 401.
func TestExchangeCodeForToken_SlackNestedResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"app_id": "A12345678",
			"authed_user": map[string]any{
				"id":            "U12345678",
				"scope":         "search:read.public,users:read",
				"token_type":    "user",
				"access_token":  "xoxp-slack-user-token",
				"expires_in":    43200,
				"refresh_token": "xoxe-1-slack-refresh",
			},
			"team": map[string]any{"id": "T12345678", "name": "My Workspace"},
		})
	}))
	defer srv.Close()

	token, err := ExchangeCodeForToken(t.Context(), srv.URL, "code", "verifier", "cid", "csec", "http://localhost/callback")
	if err != nil {
		t.Fatalf("ExchangeCodeForToken: %v", err)
	}

	if token.AccessToken != "xoxp-slack-user-token" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "xoxp-slack-user-token")
	}
	if token.TokenType != "user" {
		t.Errorf("TokenType = %q, want %q", token.TokenType, "user")
	}
	if token.RefreshToken != "xoxe-1-slack-refresh" {
		t.Errorf("RefreshToken = %q, want %q", token.RefreshToken, "xoxe-1-slack-refresh")
	}
	if token.ExpiresIn != 43200 {
		t.Errorf("ExpiresIn = %d, want 43200", token.ExpiresIn)
	}
	if token.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should be set when expires_in is non-zero")
	}
	if token.Scope != "search:read.public,users:read" {
		t.Errorf("Scope = %q, want %q", token.Scope, "search:read.public,users:read")
	}
}

// TestExchangeCodeForToken_SlackOkFalse verifies that a Slack-style
// {"ok":false,"error":"..."} payload — returned with HTTP 200 — surfaces
// as a meaningful error rather than being silently accepted as an empty
// token.
func TestExchangeCodeForToken_SlackOkFalse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "invalid_code",
		})
	}))
	defer srv.Close()

	_, err := ExchangeCodeForToken(t.Context(), srv.URL, "code", "verifier", "cid", "csec", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected an error for ok:false response, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_code") {
		t.Errorf("error = %q, want it to contain the Slack error code %q", err.Error(), "invalid_code")
	}
}

// TestExchangeCodeForToken_MissingAccessToken verifies that a 200 response
// missing any access_token (top-level or nested) is rejected with an
// explicit error instead of silently producing an empty bearer token.
func TestExchangeCodeForToken_MissingAccessToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer",
		})
	}))
	defer srv.Close()

	_, err := ExchangeCodeForToken(t.Context(), srv.URL, "code", "verifier", "cid", "csec", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected an error for response with no access_token, got nil")
	}
	if !strings.Contains(err.Error(), "access_token") {
		t.Errorf("error = %q, want it to mention access_token", err.Error())
	}
}

func TestOAuthTransport_DoesNotForwardBearerAcrossOrigins(t *testing.T) {
	t.Parallel()

	var redirectedAuthorization string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	sourceAuthorization := make(chan string, 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceAuthorization <- r.Header.Get("Authorization")
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	store := NewInMemoryTokenStore()
	require.NoError(t, store.StoreToken(source.URL, &OAuthToken{AccessToken: "stored-at"}))
	transport := &oauthTransport{
		base:       http.DefaultTransport,
		tokenStore: store,
		baseURL:    source.URL,
	}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, source.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "Bearer stored-at", <-sourceAuthorization)
	assert.Empty(t, redirectedAuthorization)
}

// TestOAuthTransport_RetryFailureExposesResponseBody verifies that when
// the authenticated retry after a successful OAuth flow still fails with
// a non-2xx status, the response body is logged and preserved for the
// caller. Without this, diagnosing post-OAuth server errors is limited
// to the generic HTTP status text, which hides useful provider-specific
// detail such as scope mismatches or payload complaints.
func TestOAuthTransport_RetryFailureExposesResponseBody(t *testing.T) {
	t.Parallel()

	const errBody = `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"insufficient_scope: missing users:read"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer stored-at" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errBody))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	store := NewInMemoryTokenStore()
	if err := store.StoreToken(srv.URL, &OAuthToken{
		AccessToken: "stored-at",
		TokenType:   "Bearer",
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    srv.URL,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.roundTrip(req, true)
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body after retry: %v", err)
	}
	if string(got) != errBody {
		t.Errorf("response body = %q, want %q", string(got), errBody)
	}
}

// TestOAuthTransport_NonRetryFailureExposesResponseBody verifies that when
// the *first* request fails with a non-2xx that we cannot retry via OAuth
// (e.g. a 400 Bad Request rather than a 401), the response body is still
// preserved and made available to the caller.
//
// Regression test for: Slack's MCP endpoint answering
//
//	400 Bad Request
//	{"jsonrpc":"2.0","id":null,"error":{"code":-32600,
//	 "message":"App is not enabled for Slack MCP server access. ..."}}
//
// where the user previously saw only "Bad Request" bubbled up from the
// MCP SDK because our transport was swallowing the body. We couldn't
// surface the single line that actually tells the user what to do.
func TestOAuthTransport_NonRetryFailureExposesResponseBody(t *testing.T) {
	t.Parallel()

	const errBody = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"App is not enabled for Slack MCP server access."}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()

	store := NewInMemoryTokenStore()
	if err := store.StoreToken(srv.URL, &OAuthToken{
		AccessToken: "stored-at",
		TokenType:   "Bearer",
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    srv.URL,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Use the exported RoundTrip, which is always called with isRetry=false.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body on first attempt: %v", err)
	}
	if string(got) != errBody {
		t.Errorf("response body = %q, want %q", string(got), errBody)
	}
}

// TestGetValidToken_DiscardsTokenWhenScopesNoLongerCovered verifies that
// a stored token whose RequestedScopes do not cover the config's current
// scopes is discarded (removed from the store and not returned), so the
// next authenticated request triggers a fresh OAuth flow.
func TestGetValidToken_DiscardsTokenWhenScopesNoLongerCovered(t *testing.T) {
	t.Parallel()

	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken:     "stale-at",
		TokenType:       "Bearer",
		RequestedScopes: []string{"users:read"},
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    "https://mcp.example.com",
		oauthConfig: &latest.RemoteOAuthConfig{
			Scopes: []string{"users:read", "channels:history"},
		},
	}

	if got := transport.getValidToken(t.Context()); got != nil {
		t.Fatalf("expected nil when configured scopes exceed stored scopes, got %+v", got)
	}

	// Token must have been purged so the next call doesn't keep returning it.
	if _, err := store.GetToken("https://mcp.example.com"); err == nil {
		t.Error("expected token to be removed from the store")
	}
}

// TestGetValidToken_ReturnsTokenWhenScopesSatisfied verifies the happy
// path: a stored token whose RequestedScopes cover every configured scope
// is returned unchanged (no re-auth, no refresh).
func TestGetValidToken_ReturnsTokenWhenScopesSatisfied(t *testing.T) {
	t.Parallel()

	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken:     "good-at",
		TokenType:       "Bearer",
		RequestedScopes: []string{"users:read", "channels:history", "extra:scope"},
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    "https://mcp.example.com",
		oauthConfig: &latest.RemoteOAuthConfig{
			Scopes: []string{"users:read", "channels:history"},
		},
	}

	got := transport.getValidToken(t.Context())
	if got == nil || got.AccessToken != "good-at" {
		t.Fatalf("expected stored token to be returned, got %+v", got)
	}
}

// TestGetValidToken_AcceptsDiscoveredWideScopesWhenNoneConfigured proves
// that a token stamped with discovery-derived RequestedScopes (e.g. an
// unconfigured DCR client that fell back to PRM scopes_supported) is still
// accepted once the config declares no scopes: sufficiency stays
// config-only and never re-derives or narrows against discovery.
func TestGetValidToken_AcceptsDiscoveredWideScopesWhenNoneConfigured(t *testing.T) {
	t.Parallel()

	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken:     "good-at",
		TokenType:       "Bearer",
		RequestedScopes: []string{"repo", "profile", "admin:everything"},
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    "https://mcp.example.com",
		// No oauthConfig at all (nil), matching the Catalog product path.
	}

	got := transport.getValidToken(t.Context())
	if got == nil || got.AccessToken != "good-at" {
		t.Fatalf("expected the discovered-scope token to be accepted unchanged, got %+v", got)
	}
}

// TestGetValidToken_LeavesLegacyTokenAlone verifies that stored tokens
// that predate the RequestedScopes field (empty slice) are treated as
// sufficient, so an upgrade doesn't forcibly invalidate every existing
// user's session.
func TestGetValidToken_LeavesLegacyTokenAlone(t *testing.T) {
	t.Parallel()

	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken: "legacy-at",
		TokenType:   "Bearer",
		// RequestedScopes intentionally nil (legacy).
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    "https://mcp.example.com",
		oauthConfig: &latest.RemoteOAuthConfig{
			Scopes: []string{"users:read"},
		},
	}

	if got := transport.getValidToken(t.Context()); got == nil {
		t.Fatal("legacy token without RequestedScopes should not be invalidated on scope mismatch")
	}
}

// TestGetValidToken_ConfiguredScopeNormalizationMatchesDCRSelection guards
// against a reauth loop: selectDCRScopes stamps the normalized (trimmed,
// deduplicated) form of the configured scopes onto a newly-issued token's
// RequestedScopes, so tokenCoversConfiguredScopes must normalize the
// configured side of the coverage check the very same way. Comparing raw
// config entries (with stray whitespace or duplicates) against the
// already-normalized stored scopes would otherwise fail on every single
// preflight, discarding an otherwise-sufficient token and forcing a fresh
// DCR/consent flow every time the config is checked — not just once.
func TestGetValidToken_ConfiguredScopeNormalizationMatchesDCRSelection(t *testing.T) {
	t.Parallel()

	configured := []string{" read ", "read", "write "}

	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken: "good-at",
		TokenType:   "Bearer",
		// What a prior successful DCR selection actually stamped: the
		// normalized form of these same configured scopes.
		RequestedScopes: selectDCRScopes(configured, nil, nil),
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:        newTestTransport(t),
		tokenStore:  store,
		baseURL:     "https://mcp.example.com",
		oauthConfig: &latest.RemoteOAuthConfig{Scopes: configured},
	}

	// Repeated preflight checks (e.g. successive tool calls) must all see the
	// same sufficient token; a normalization mismatch would discard it on
	// the first call and keep discarding the freshly re-issued one forever.
	for i := range 3 {
		got := transport.getValidToken(t.Context())
		require.NotNil(t, got, "preflight %d: whitespace/duplicate configured scopes must not discard a sufficient token", i)
		assert.Equal(t, "good-at", got.AccessToken)
	}
}

// TestOAuthTransport_NonInteractiveCtxSkipsElicitation verifies that when
// the request context is marked non-interactive (via WithoutInteractivePrompts),
// a 401 with WWW-Authenticate does NOT trigger the OAuth flow. Instead the
// transport returns a recognisable AuthorizationRequiredError, so callers can
// surface a deferred-auth notice without the goroutine getting stuck on a
// dialog the UI is not yet ready to show.
//
// We deliberately leave the transport's `client` field nil: in non-interactive
// mode the short-circuit must happen before anything in the OAuth flow
// (which would dereference `client` to send an elicitation) is reached. A
// nil-pointer panic here would be a clear, loud signal that the contract
// is broken.
//
// Regression test for: "docker agent run ./examples/slack.yaml" hanging
// during startup, with Ctrl-C unable to interrupt because the OAuth
// elicitation was synchronously waiting on a TUI prompt that hadn't been
// rendered yet.
func TestOAuthTransport_NonInteractiveCtxSkipsElicitation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource="https://example.test/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	transport := &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: NewInMemoryTokenStore(),
		baseURL:    srv.URL,
		// client intentionally left nil — see test comment above.
	}

	ctx := WithoutInteractivePrompts(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, gotErr := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}

	if gotErr == nil {
		t.Fatalf("expected an error in non-interactive mode, got resp=%v err=nil", resp)
	}
	if !IsAuthorizationRequired(gotErr) {
		t.Errorf("expected IsAuthorizationRequired(err)=true, got err=%v", gotErr)
	}
}

// TestRequestElicitation_NoHandlerReturnsAuthRequired verifies that when
// the OAuth transport asks the session client for an elicitation but no
// handler has been wired up yet (typically because the runtime hasn't
// called configureToolsetHandlers for this toolset), the session client
// surfaces a recognisable AuthorizationRequiredError rather than an
// opaque "no elicitation handler configured" message.
//
// Without this, a transient gap between the start of the OAuth flow and
// the runtime wiring up the elicitation bridge would surface to the user
// as an internal-looking error instead of being treated as a normal
// "needs auth, retry next turn" deferral. Pairs with the higher-level
// TestInitialize_OAuthDefersWhenElicitationBridgeNotReady which exercises
// the same invariant through the full Initialize → Connect → OAuth flow.
func TestRequestElicitation_NoHandlerReturnsAuthRequired(t *testing.T) {
	t.Parallel()

	var c sessionClient

	// No SetElicitationHandler call: the handler stays nil, simulating the
	// window before configureToolsetHandlers wires it up.
	_, err := c.requestElicitation(t.Context(), nil)
	if err == nil {
		t.Fatal("requestElicitation must return an error when no handler is configured")
	}
	if !IsAuthorizationRequired(err) {
		t.Errorf("requestElicitation must return AuthorizationRequiredError so the OAuth flow can be silently deferred; got: %v", err)
	}
}

// TestExtractServerMessage covers the body-to-string conversion used when
// wrapping Initialize errors. The goal is to pick the most human-readable
// string out of whatever the server returns so it can be shown as a TUI
// warning, falling back gracefully instead of leaking opaque JSON.
func TestExtractServerMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "jsonrpc_error_message",
			body: `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"App is not enabled."}}`,
			want: "App is not enabled.",
		},
		{
			name: "top_level_message",
			body: `{"message":"rate limited"}`,
			want: "rate limited",
		},
		{
			name: "slack_style_error_string",
			body: `{"ok":false,"error":"invalid_auth"}`,
			want: "invalid_auth",
		},
		{
			name: "plain_text",
			body: "Service   Unavailable\n\n",
			want: "Service Unavailable",
		},
		{
			name: "empty_body",
			body: "   ",
			want: "",
		},
		{
			name: "very_long_plaintext_is_capped",
			body: strings.Repeat("A", 1000),
			want: strings.Repeat("A", 400) + "\u2026",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractServerMessage([]byte(tc.body))
			if got != tc.want {
				t.Errorf("extractServerMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestOAuthTransportAllowPrivateIPsControlsOAuthClient(t *testing.T) {
	t.Parallel()

	authMux := http.NewServeMux()
	authSrv := httptest.NewServer(authMux)
	defer authSrv.Close()

	authMux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 authSrv.URL,
			"token_endpoint":         authSrv.URL + "/token",
			"authorization_endpoint": authSrv.URL + "/authorize",
		})
	})
	authMux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-at",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	mcpSrv := httptest.NewServer(http.NotFoundHandler())
	defer mcpSrv.Close()

	newTransport := func(client *http.Client) *oauthTransport {
		store := NewInMemoryTokenStore()
		err := store.StoreToken(mcpSrv.URL, &OAuthToken{
			AccessToken:  "old-at",
			TokenType:    "Bearer",
			RefreshToken: "old-rt",
			ExpiresAt:    time.Now().Add(-1 * time.Hour),
			AuthServer:   authSrv.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		return &oauthTransport{
			base:            newTestTransport(t),
			tokenStore:      store,
			baseURL:         mcpSrv.URL,
			oauthHTTPClient: client,
		}
	}

	safeTransport := newTransport(httpclient.NewSafeClient(time.Second, false))
	if got := safeTransport.getValidToken(t.Context()); got != nil {
		t.Fatalf("getValidToken with SSRF-safe OAuth client returned token %q, want nil", got.AccessToken)
	}

	allowPrivateIPsTransport := newTransport(oauthHTTPClientForAllowPrivateIPs(true))
	got := allowPrivateIPsTransport.getValidToken(t.Context())
	require.NotNil(t, got, "getValidToken with allow_private_ips OAuth client returned nil, want refreshed token")
	if got.AccessToken != "fresh-at" {
		t.Fatalf("AccessToken = %q, want fresh-at", got.AccessToken)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestOAuthHTTPClientWithHeaders_StripsDefaultPort verifies that an explicit
// standard port in the configured URL (e.g. https://host:443) is stripped
// when building the host-scoping transport, so it matches the port-less
// discovery URLs servers usually advertise instead of silently dropping the
// headers. Guards the construction call site.
func TestOAuthHTTPClientWithHeaders_StripsDefaultPort(t *testing.T) {
	t.Parallel()

	client := oauthHTTPClientWithHeaders("https://mcp.example.com:443/mcp",
		map[string]string{"X-Grafana-URL": "https://instance.grafana.net/"}, false, nil)
	hst, ok := client.Transport.(*hostScopedHeaderTransport)
	require.True(t, ok, "expected a host-scoped transport when headers are configured")
	assert.Equal(t, "mcp.example.com", hst.host,
		"a configured standard :443 port must be stripped so it matches port-less discovery URLs")
}

// TestHostScopedHeaderTransport_NormalizesRequestPort verifies the other side:
// when the configured host omits the port but a server-advertised discovery
// URL spells out :443, RoundTrip still routes through the header-bearing
// transport. Guards the per-request normalisation in RoundTrip.
func TestHostScopedHeaderTransport_NormalizesRequestPort(t *testing.T) {
	t.Parallel()

	var withHeadersCalled, baseCalled bool
	mark := func(flag *bool) roundTripFunc {
		return func(*http.Request) (*http.Response, error) {
			*flag = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}
	}

	tr := &hostScopedHeaderTransport{
		host:        "mcp.example.com", // configured without an explicit port
		withHeaders: mark(&withHeadersCalled),
		base:        mark(&baseCalled),
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://mcp.example.com:443/.well-known/oauth-protected-resource", http.NoBody)
	require.NoError(t, err)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.True(t, withHeadersCalled,
		"a request that spells out :443 must route through the header-bearing transport when the configured host omits it")
	assert.False(t, baseCalled, "must not fall through to the header-less branch")
}

func TestResourceMetadataFromWWWAuth_PrefersResourceMetadataParam(t *testing.T) {
	t.Parallel()

	const header = `Bearer realm="agentic-platform-mcp", resource="https://mcp.example.test/api/mcp/remote", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource"`

	assert.Equal(t,
		"https://mcp.example.test/.well-known/oauth-protected-resource",
		resourceMetadataFromWWWAuth(header),
	)
}

func TestResourceMetadataFromWWWAuth_FallsBackToLegacyResourceParam(t *testing.T) {
	t.Parallel()

	const header = `Bearer resource="https://mcp.example.test/.well-known/oauth-protected-resource"`

	assert.Equal(t,
		"https://mcp.example.test/.well-known/oauth-protected-resource",
		resourceMetadataFromWWWAuth(header),
	)
}

func TestOAuthTransportCoalescesConcurrentAuthorization(t *testing.T) {
	t.Parallel()

	authMux := http.NewServeMux()
	authSrv := httptest.NewServer(authMux)
	defer authSrv.Close()

	authMux.HandleFunc("/resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              "http://mcp.example.test/mcp",
			"authorization_servers": []string{authSrv.URL},
		})
	})
	authMux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 authSrv.URL,
			"authorization_endpoint": authSrv.URL + "/authorize",
			"token_endpoint":         authSrv.URL + "/token",
		})
	})

	client := &remoteMCPClient{}
	var elicitationCalls atomic.Int32
	enteredElicitation := make(chan struct{})
	finishElicitation := make(chan struct{})
	client.SetElicitationHandler(func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		if elicitationCalls.Add(1) == 1 {
			close(enteredElicitation)
		}
		<-finishElicitation
		return tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"access_token": "token",
				"token_type":   "Bearer",
			},
		}, nil
	})

	var unauthenticatedRequests atomic.Int32
	var authenticatedRequests atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "Bearer token" {
			authenticatedRequests.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     http.StatusText(http.StatusOK),
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}

		unauthenticatedRequests.Add(1)
		header := make(http.Header)
		header.Set("WWW-Authenticate", `Bearer resource="`+authSrv.URL+`/resource"`)
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     http.StatusText(http.StatusUnauthorized),
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("unauthorized")),
			Request:    req,
		}, nil
	})

	transport := &oauthTransport{
		base:               base,
		requestElicitation: client.requestElicitation,
		tokenStore:         NewInMemoryTokenStore(),
		baseURL:            "http://mcp.example.test/mcp",
		oauthHTTPClient:    authSrv.Client(),
	}

	firstReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, transport.baseURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	secondReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, transport.baseURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 2)
	go func() {
		resp, err := transport.RoundTrip(firstReq)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	select {
	case <-enteredElicitation:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter OAuth elicitation")
	}

	go func() {
		resp, err := transport.RoundTrip(secondReq)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	require.Eventually(t, func() bool { return unauthenticatedRequests.Load() >= 2 },
		time.Second, 10*time.Millisecond, "second request did not reach 401 path")

	close(finishElicitation)

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("RoundTrip returned error: %v", err)
		}
	}

	if got := elicitationCalls.Load(); got != 1 {
		t.Fatalf("elicitation calls = %d, want 1", got)
	}
	if got := authenticatedRequests.Load(); got != 2 {
		t.Fatalf("authenticated retry requests = %d, want 2", got)
	}
}

func TestExchangeCodeForTokenWithResourceSendsResource(t *testing.T) {
	t.Parallel()

	var gotResource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotResource = r.FormValue("resource")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
		})
	}))
	defer srv.Close()

	_, err := ExchangeCodeForTokenWithResource(
		t.Context(),
		srv.URL,
		"code",
		"verifier",
		"cid",
		"",
		"http://localhost/callback",
		"https://mcp.example.com",
	)
	if err != nil {
		t.Fatalf("ExchangeCodeForTokenWithResource: %v", err)
	}
	if gotResource != "https://mcp.example.com" {
		t.Fatalf("resource = %q, want https://mcp.example.com", gotResource)
	}
}

// TestMetadataDiscoveryURLs covers the candidate-URL builder for the four
// shapes of issuer URL we see in the wild:
//
//   - origin only (no path): one OAuth + one OIDC fallback,
//   - issuer with a path: RFC 8414 §3.1 path-aware variant first, then the
//     widely-deployed "append" form, then the OIDC equivalents,
//   - URL already pointing at a /.well-known/ endpoint: pass through.
func TestMetadataDiscoveryURLs(t *testing.T) {
	t.Parallel()

	t.Run("origin only", func(t *testing.T) {
		got, err := metadataDiscoveryURLs("https://auth.example.com")
		require.NoError(t, err)
		assert.Equal(t, []string{
			"https://auth.example.com/.well-known/oauth-authorization-server",
			"https://auth.example.com/.well-known/openid-configuration",
		}, got)
	})

	t.Run("issuer with path (Stripe-style)", func(t *testing.T) {
		// access.stripe.com/mcp serves metadata at
		//   /.well-known/oauth-authorization-server/mcp
		// (RFC 8414 §3.1) — the "append" form 404s. Both must be tried.
		got, err := metadataDiscoveryURLs("https://access.stripe.com/mcp")
		require.NoError(t, err)
		assert.Equal(t, []string{
			"https://access.stripe.com/.well-known/oauth-authorization-server/mcp",
			"https://access.stripe.com/mcp/.well-known/oauth-authorization-server",
			"https://access.stripe.com/.well-known/openid-configuration/mcp",
			"https://access.stripe.com/mcp/.well-known/openid-configuration",
		}, got)
	})

	t.Run("trailing slash on path is normalized", func(t *testing.T) {
		got, err := metadataDiscoveryURLs("https://auth.example.com/tenant/")
		require.NoError(t, err)
		assert.Equal(t, []string{
			"https://auth.example.com/.well-known/oauth-authorization-server/tenant",
			"https://auth.example.com/tenant/.well-known/oauth-authorization-server",
			"https://auth.example.com/.well-known/openid-configuration/tenant",
			"https://auth.example.com/tenant/.well-known/openid-configuration",
		}, got)
	})

	t.Run("explicit well-known URL passes through", func(t *testing.T) {
		in := "https://auth.example.com/.well-known/oauth-authorization-server"
		got, err := metadataDiscoveryURLs(in)
		require.NoError(t, err)
		assert.Equal(t, []string{in}, got)
	})

	t.Run("query string is rejected", func(t *testing.T) {
		// RFC 8414 §2 forbids query components on issuer URLs.
		// Multi-tenant Keycloak installations sometimes advertise
		// query-bearing URLs anyway; surface the misconfiguration
		// instead of silently building wrong discovery URLs.
		_, err := metadataDiscoveryURLs("https://auth.example.com/realms/r?x=1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "query")
	})

	t.Run("fragment is rejected", func(t *testing.T) {
		_, err := metadataDiscoveryURLs("https://auth.example.com/realms/r#x")
		require.Error(t, err)
	})
}

// TestGetAuthorizationServerMetadata_RFC8414PathAware reproduces the
// Stripe-remote failure surfaced by TestCatalogOAuthDiscoveryLive: the
// authorization server's metadata is published at the spec-compliant
// "well-known between origin and path" URL, while the legacy "append to
// the issuer URL" variant 404s. The discovery code must fall through and
// pick the spec-compliant one before giving up and returning default
// (i.e. broken) metadata.
func TestGetAuthorizationServerMetadata_RFC8414PathAware(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The legacy "append" URL must NOT be hit successfully — return 404.
	mux.HandleFunc("/mcp/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	// RFC 8414 §3.1 spec-compliant variant: well-known between origin and path.
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL + "/mcp",
			"authorization_endpoint": srv.URL + "/oauth/authorize",
			"token_endpoint":         srv.URL + "/oauth/token",
			"registration_endpoint":  srv.URL + "/oauth/register",
		})
	})

	o := &oauth{metadataClient: srv.Client()}
	md, err := o.getAuthorizationServerMetadata(t.Context(), srv.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/oauth/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/oauth/token", md.TokenEndpoint)
	assert.Equal(t, srv.URL+"/oauth/register", md.RegistrationEndpoint,
		"the registration endpoint must come from the RFC 8414 path-aware metadata, "+
			"not from createDefaultMetadata's empty fallback")
}

// TestGetAuthorizationServerMetadata_AppendFormStillWorks asserts the
// fallback path: many widely-deployed auth servers serve metadata at the
// "append" URL only, so that variant must still be tried and accepted.
func TestGetAuthorizationServerMetadata_AppendFormStillWorks(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/tenant/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL + "/tenant",
			"authorization_endpoint": srv.URL + "/tenant/authorize",
			"token_endpoint":         srv.URL + "/tenant/token",
		})
	})

	o := &oauth{metadataClient: srv.Client()}
	md, err := o.getAuthorizationServerMetadata(t.Context(), srv.URL+"/tenant")
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/tenant/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/tenant/token", md.TokenEndpoint)
}

// TestGetAuthorizationServerMetadata_NonFatalCandidateStatus asserts that
// a non-200/non-404 response on one candidate (e.g. a server that
// answers 403 on the path-aware variant it doesn't implement) does NOT
// abort the probe — the next candidate is still tried, and a 200 there
// wins. Without this, RFC 8414 §3.1 ordering regresses servers whose
// path-aware endpoint returns anything other than 404.
func TestGetAuthorizationServerMetadata_NonFatalCandidateStatus(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// First candidate: path-aware variant returns 403 (some gateways do this).
	mux.HandleFunc("/.well-known/oauth-authorization-server/tenant", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	// Second candidate: legacy append form returns valid metadata.
	mux.HandleFunc("/tenant/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL + "/tenant",
			"authorization_endpoint": srv.URL + "/tenant/authorize",
			"token_endpoint":         srv.URL + "/tenant/token",
		})
	})

	o := &oauth{metadataClient: srv.Client()}
	md, err := o.getAuthorizationServerMetadata(t.Context(), srv.URL+"/tenant")
	require.NoError(t, err, "a 403 on a speculative candidate must not abort the probe")
	assert.Equal(t, srv.URL+"/tenant/authorize", md.AuthorizationEndpoint)
}

// TestGetAuthorizationServerMetadata_AllUnreachableSurfacesError asserts
// that when every candidate fails with a non-404 status (i.e. nothing
// 404'd through to the "discovery is just absent" interpretation), the
// probe surfaces an error instead of silently returning fabricated
// default metadata that will fail later in the OAuth handshake.
func TestGetAuthorizationServerMetadata_AllUnreachableSurfacesError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	o := &oauth{metadataClient: srv.Client()}
	_, err := o.getAuthorizationServerMetadata(t.Context(), srv.URL+"/tenant")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// TestGetAuthorizationServerMetadata_All404FallsBackToDefaults asserts
// the legacy behaviour: a 404 from every candidate means "this server
// doesn't expose discovery metadata", which is OK and we should fall
// back to fabricated defaults rather than erroring out.
func TestGetAuthorizationServerMetadata_All404FallsBackToDefaults(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	o := &oauth{metadataClient: srv.Client()}
	md, err := o.getAuthorizationServerMetadata(t.Context(), srv.URL+"/tenant")
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/tenant/authorize", md.AuthorizationEndpoint,
		"defaults must be derived from the issuer URL")
}

// --------- Unmanaged flow with docker-agent-driven OAuth ---------

// unmanagedOAuthTestServer stands up an httptest.Server that emulates both
// the MCP server (returns 401 + WWW-Authenticate) and the authorization
// server (metadata + DCR + token endpoint) at known paths.
type unmanagedOAuthTestServer struct {
	*httptest.Server

	tokenCalls atomic.Int32
	lastForm   url.Values
	prmHeaders http.Header
}

func newUnmanagedOAuthTestServer(t *testing.T) *unmanagedOAuthTestServer {
	t.Helper()
	srv := &unmanagedOAuthTestServer{}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		srv.prmHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
			RegistrationEndpoint:  srv.URL + "/register",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id","client_secret":"registered-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		srv.tokenCalls.Add(1)
		_ = r.ParseForm()
		srv.lastForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600,"refresh_token":"refresh-tok"}`))
	})
	// The MCP endpoint at "/" returns 401 until a Bearer header is present.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource="`+srv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	srv.Server = httptest.NewServer(mux)
	return srv
}

// elicitCaptured records the elicitation request that the OAuth
// transport sent and lets the test reply with a chosen payload,
// mirroring what a real client would do.
type elicitCaptured struct {
	mu      sync.Mutex
	req     *gomcp.ElicitParams
	reply   tools.ElicitationResult
	replyFn func(req *gomcp.ElicitParams) tools.ElicitationResult
}

func (e *elicitCaptured) handler(_ context.Context, req *gomcp.ElicitParams) (tools.ElicitationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.req = req
	if e.replyFn != nil {
		return e.replyFn(req), nil
	}
	return e.reply, nil
}

// newUnmanagedTestTransport builds an oauthTransport configured for the
// unmanaged flow and wires the supplied elicitation handler.
func newUnmanagedTestTransport(t *testing.T, baseURL, redirectURI string, capture *elicitCaptured) (*oauthTransport, *remoteMCPClient) {
	t.Helper()
	client := newRemoteClient(baseURL, "streamable", nil, NewInMemoryTokenStore(), nil, false, nil)
	client.unmanagedOAuthRedirectURI = redirectURI
	client.SetElicitationHandler(capture.handler)
	// Allow private-IP destinations so the httptest server's 127.0.0.1 URLs
	// aren't blocked by the SSRF guard the production OAuth helper uses.
	client.allowPrivateIPs = true
	transport := &oauthTransport{
		base:                      newTestTransport(t),
		requestElicitation:        client.requestElicitation,
		onOAuthSuccess:            client.oauthSuccess,
		tokenStore:                client.tokenStore,
		baseURL:                   baseURL,
		managed:                   false,
		unmanagedOAuthRedirectURI: redirectURI,
		oauthHTTPClient:           oauthHTTPClientForAllowPrivateIPs(true),
	}
	return transport, client
}

// TestUnmanagedOAuthFlow_DriveFlow_ExchangesCodeForToken verifies the new
// docker-agent-driven branch end-to-end: docker-agent emits the elicitation
// with authorize_url + state, the client (test stub) replies with
// {code, state}, and docker-agent exchanges the code at the token endpoint.
func TestUnmanagedOAuthFlow_DriveFlow_ExchangesCodeForToken(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	capture := &elicitCaptured{}
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		// Echo the state docker-agent sent us, simulating a real OAuth
		// callback round-trip.
		state, _ := req.Meta["docker-agent/state"].(string)
		return tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"code":  "abc",
				"state": state,
			},
		}
	}
	transport, client := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the elicitation carried the new fields.
	require.NotNil(t, capture.req)
	assert.Equal(t, "oauth_flow", capture.req.Meta["docker-agent/type"])
	assert.Equal(t, srv.URL, capture.req.Meta["docker-agent/server_url"])
	authorizeURL, _ := capture.req.Meta["docker-agent/authorize_url"].(string)
	assert.NotEmpty(t, authorizeURL, "drive-flow elicitation must include docker-agent/authorize_url")
	assert.Contains(t, authorizeURL, "redirect_uri="+url.QueryEscape(redirectURI))
	state, _ := capture.req.Meta["docker-agent/state"].(string)
	assert.NotEmpty(t, state, "drive-flow elicitation must include docker-agent/state")

	// Verify docker-agent exchanged the code (not the client).
	require.Equal(t, int32(1), srv.tokenCalls.Load(), "token endpoint must be hit exactly once")
	assert.Equal(t, "authorization_code", srv.lastForm.Get("grant_type"))
	assert.Equal(t, "abc", srv.lastForm.Get("code"))
	assert.Equal(t, redirectURI, srv.lastForm.Get("redirect_uri"),
		"redirect_uri sent at /token must match the one used at /authorize per RFC 6749 §4.1.3")
	assert.Equal(t, "registered-client-id", srv.lastForm.Get("client_id"))
	assert.NotEmpty(t, srv.lastForm.Get("code_verifier"), "PKCE verifier must be sent at exchange")

	// Verify the token landed in the store with the credentials stamped on
	// (for silent refresh later).
	tok, err := client.tokenStore.GetToken(srv.URL)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, "exchanged-at", tok.AccessToken)
	assert.Equal(t, "refresh-tok", tok.RefreshToken)
	assert.Equal(t, "registered-client-id", tok.ClientID)
	assert.Equal(t, "registered-secret", tok.ClientSecret)
	assert.Equal(t, srv.URL, tok.AuthServer)
}

// TestInitialize_CustomHeadersReachOAuthDiscovery drives the real
// Initialize -> createHTTPClient path (not a hand-built transport) to prove
// the production wiring forwards configured custom headers to the OAuth
// protected-resource-metadata discovery request. Grafana Cloud relies on the
// X-Grafana-URL header on that request to scope the OAuth flow to the right
// instance; without it the auth screen prompts for the instance. Regression
// test for issue #3148.
func TestInitialize_CustomHeadersReachOAuthDiscovery(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	headers := map[string]string{"X-Grafana-URL": "https://instance.grafana.net/"}
	client := newRemoteClient(srv.URL, "streamable", headers, NewInMemoryTokenStore(), nil, true, nil)

	// Decline the OAuth elicitation: the unmanaged flow reaches the
	// protected-resource-metadata discovery request (which carries the
	// header) before the elicitation, so declining lets Initialize return
	// promptly while still exercising the discovery request through the real
	// createHTTPClient wiring.
	client.SetElicitationHandler(func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	})

	_, err := client.Initialize(t.Context(), nil)
	require.Error(t, err, "Initialize should fail after the OAuth elicitation is declined")

	require.NotNil(t, srv.prmHeaders, "protected-resource-metadata endpoint was never hit during the OAuth flow")
	assert.Equal(t, "https://instance.grafana.net/", srv.prmHeaders.Get("X-Grafana-URL"),
		"the configured header must reach the protected-resource-metadata discovery request (issue #3148)")
}

// TestUnmanagedOAuthFlow_ElicitationDeclineReturnsSentinel verifies that
// when the user dismisses the host's Authentication Request dialog
// (action=decline), the OAuth round-trip surfaces a recognisable
// *OAuthDeclinedError, NOT a generic error.
//
// This sentinel is the contract that lets the mcp catalog toolset break
// the "Tools() -> Start() -> elicitation" retry loop: a generic error
// looks like a transient failure and gets retried on the next agent
// loop iteration, which is exactly what would cause the dismissed
// dialog to re-appear immediately.
func TestUnmanagedOAuthFlow_ElicitationDeclineReturnsSentinel(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	capture := &elicitCaptured{}
	capture.replyFn = func(_ *gomcp.ElicitParams) tools.ElicitationResult {
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, rtErr := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, rtErr)
	assert.True(t, IsOAuthDeclined(rtErr),
		"declined elicitation must surface as IsOAuthDeclined so the catalog can short-circuit the retry loop; got: %v", rtErr)

	var declined *OAuthDeclinedError
	require.ErrorAs(t, rtErr, &declined)
	assert.Equal(t, srv.URL, declined.URL,
		"the sentinel must carry the server URL so callers can correlate it back to a catalog entry")

	assert.Equal(t, int32(0), srv.tokenCalls.Load(),
		"token endpoint must NOT be hit when the user declined the authorization")
}

// TestUnmanagedOAuthFlow_ElicitationDeclineLatchesAgainstConcurrentRoundTrips
// is the regression test for the "Cancel re-pops the dialog" bug. The MCP
// SDK's Connect() fires several initialize-stage RPCs in parallel; each
// gets a 401 and queues on oauthFlowMu. Without the sticky-decline latch
// inside authorizeOnce, the FIRST queued goroutine returns
// OAuthDeclinedError on user cancel and releases the mutex; the NEXT
// queued goroutine then sees no valid token and starts a fresh OAuth
// flow, re-emitting the elicitation the user just dismissed.
//
// This test fires N concurrent RoundTrips. The elicitation handler
// declines on the first invocation. The expected behaviour: exactly
// ONE elicitation is sent (no re-pop), and every concurrent roundtrip
// returns OAuthDeclinedError.
func TestUnmanagedOAuthFlow_ElicitationDeclineLatchesAgainstConcurrentRoundTrips(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	const concurrentRoundTrips = 4

	var elicitationCount atomic.Int32
	capture := &elicitCaptured{}
	capture.replyFn = func(_ *gomcp.ElicitParams) tools.ElicitationResult {
		elicitationCount.Add(1)
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	type rtOut struct {
		resp *http.Response
		err  error
	}
	results := make(chan rtOut, concurrentRoundTrips)
	for range concurrentRoundTrips {
		go func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
			if err != nil {
				results <- rtOut{nil, err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := transport.RoundTrip(req)
			// Close the body in the goroutine that owns the
			// response (bodyclose linter cannot see the receiver
			// side closing it through a channel send).
			if resp != nil {
				_ = resp.Body.Close()
			}
			results <- rtOut{resp, err}
		}()
	}

	for range concurrentRoundTrips {
		select {
		case out := <-results:
			require.Error(t, out.err)
			assert.True(t, IsOAuthDeclined(out.err),
				"every concurrent roundtrip must surface IsOAuthDeclined after the user declined; got: %v", out.err)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for concurrent OAuth round-trips to complete")
		}
	}

	assert.Equal(t, int32(1), elicitationCount.Load(),
		"exactly one elicitation must be sent: subsequent queued roundtrips must observe the latched decline state and short-circuit before opening a new dialog")
	assert.Equal(t, int32(0), srv.tokenCalls.Load(),
		"token endpoint must NOT be hit when the user declined")
}

// TestUnmanagedOAuthFlow_DriveFlow_RejectsStateMismatch verifies the CSRF
// check: if the client returns a `state` value that doesn't match what
// docker-agent generated and embedded in the authorize URL, the flow
// aborts WITHOUT calling the token endpoint.
func TestUnmanagedOAuthFlow_DriveFlow_RejectsStateMismatch(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	capture := &elicitCaptured{
		reply: tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"code":  "abc",
				"state": "i-made-this-up",
			},
		},
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, "https://example.test/oauth/cb", capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state mismatch")
	assert.Equal(t, int32(0), srv.tokenCalls.Load(), "token endpoint must not be hit on state mismatch")
}

// TestUnmanagedOAuthFlow_DriveFlow_AcceptsLegacyAccessTokenReply verifies
// that when docker-agent is driving the flow but the client decides to do
// the exchange itself anyway (returning {access_token, …}), the legacy
// reply shape is still honored — no error, token stored verbatim, no
// /token request from docker-agent.
func TestUnmanagedOAuthFlow_DriveFlow_AcceptsLegacyAccessTokenReply(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	capture := &elicitCaptured{
		reply: tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"access_token":  "client-provided-at",
				"token_type":    "Bearer",
				"refresh_token": "client-provided-refresh",
			},
		},
	}
	transport, client := newUnmanagedTestTransport(t, srv.URL, "https://example.test/oauth/cb", capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(0), srv.tokenCalls.Load(),
		"docker-agent must not exchange when the client supplied a ready token")

	tok, err := client.tokenStore.GetToken(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "client-provided-at", tok.AccessToken)
	assert.Equal(t, "client-provided-refresh", tok.RefreshToken)
}

// TestUnmanagedOAuthFlow_LegacyMode_NoAuthorizeURLInElicitation verifies the
// back-compat path: with no redirect URI configured, the elicitation does
// NOT include authorize_url/state, the client returns an access token, and
// docker-agent stores it directly.
func TestUnmanagedOAuthFlow_LegacyMode_NoAuthorizeURLInElicitation(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	capture := &elicitCaptured{
		reply: tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"access_token": "legacy-at",
				"token_type":   "Bearer",
			},
		},
	}
	// Empty redirect URI → legacy client-driven mode.
	transport, _ := newUnmanagedTestTransport(t, srv.URL, "", capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	require.NotNil(t, capture.req)
	_, hasAuthorizeURL := capture.req.Meta["docker-agent/authorize_url"]
	assert.False(t, hasAuthorizeURL, "legacy unmanaged flow must not include docker-agent/authorize_url")
	_, hasState := capture.req.Meta["docker-agent/state"]
	assert.False(t, hasState, "legacy unmanaged flow must not include docker-agent/state")
	// resource_metadata is still surfaced so the client can do its own DCR
	// if desired.
	assert.NotNil(t, capture.req.Meta["resource_metadata"])
}

// TestUnmanagedOAuthFlow_LegacyMode_RejectsCodeStateReply verifies that a
// client which sends {code, state} despite docker-agent not emitting an
// authorize_url is rejected — there is no stored PKCE verifier to exchange
// the code with, so the flow cannot complete.
func TestUnmanagedOAuthFlow_LegacyMode_RejectsCodeStateReply(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	capture := &elicitCaptured{
		reply: tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"code":  "abc",
				"state": "xyz",
			},
		},
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, "", capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no redirect URI was configured")
}

// TestUnmanagedOAuthFlow_DriveFlow_AcceptsDirectCallback verifies the new
// out-of-band path: the elicitation never replies (the client is a thin
// courier that opens the browser and forwards the deeplink via
// /api/mcp-oauth/callback). docker-agent's pending-OAuth registry
// delivers {code, state} directly into the waiting flow, which then
// exchanges the code at the token endpoint as on the regular drive-flow.
func TestUnmanagedOAuthFlow_DriveFlow_AcceptsDirectCallback(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	// Block the elicitation handler indefinitely (until the test
	// goroutine closes the channel below). This simulates an embedder
	// that never sends a `ResumeElicitation` -- the deeplink relay goes
	// straight to /api/mcp-oauth/callback instead.
	elicitationCanReturn := make(chan struct{})
	defer close(elicitationCanReturn)
	capture := &elicitCaptured{}
	stateSent := make(chan string, 1)
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		state, _ := req.Meta["docker-agent/state"].(string)
		stateSent <- state
		<-elicitationCanReturn
		// This return value should be unreachable because the direct
		// callback wins first and cancels the elicitation context. The
		// real-world client's elicitation request returns ctx.Err() in
		// that case; we mirror that with a decline so a bug that would
		// otherwise consume this reply path is obvious in tests.
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}
	transport, client := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	// Run the OAuth flow on a separate goroutine; the main test goroutine
	// will deliver the callback once the elicitation has been observed.
	type roundTripResult struct {
		resp *http.Response
		err  error
	}
	rtCh := make(chan roundTripResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
		if err != nil {
			rtCh <- roundTripResult{nil, err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		rtCh <- roundTripResult{resp, err}
	}()

	// Wait for the elicitation to be sent (so we know the registry has a
	// waiter), then deliver the callback out of band.
	var state string
	select {
	case state = <-stateSent:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for elicitation to be sent")
	}
	require.NotEmpty(t, state)
	require.NoError(t, DeliverPendingOAuthCallback(state, PendingOAuthCallback{Code: "abc"}))

	// The RoundTrip goroutine should now complete.
	var rt roundTripResult
	select {
	case rt = <-rtCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for OAuth flow to complete after direct callback")
	}
	require.NoError(t, rt.err)
	require.NotNil(t, rt.resp)
	assert.Equal(t, http.StatusOK, rt.resp.StatusCode)

	// Verify the same token-endpoint contract as the regular drive-flow.
	require.Equal(t, int32(1), srv.tokenCalls.Load(), "token endpoint must be hit exactly once")
	assert.Equal(t, "abc", srv.lastForm.Get("code"))
	assert.Equal(t, redirectURI, srv.lastForm.Get("redirect_uri"))
	assert.NotEmpty(t, srv.lastForm.Get("code_verifier"))

	tok, err := client.tokenStore.GetToken(srv.URL)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, "exchanged-at", tok.AccessToken)
}

// TestUnmanagedOAuthFlow_DriveFlow_DirectCallbackError relays an OAuth
// error (e.g. user denied consent) via the direct-callback path. The
// flow must abort cleanly without hitting the token endpoint.
func TestUnmanagedOAuthFlow_DriveFlow_DirectCallbackError(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	elicitationCanReturn := make(chan struct{})
	defer close(elicitationCanReturn)
	capture := &elicitCaptured{}
	stateSent := make(chan string, 1)
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		state, _ := req.Meta["docker-agent/state"].(string)
		stateSent <- state
		<-elicitationCanReturn
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	rtErrCh := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
		if err != nil {
			rtErrCh <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		rtErrCh <- err
	}()

	var state string
	select {
	case state = <-stateSent:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for elicitation to be sent")
	}
	require.NoError(t, DeliverPendingOAuthCallback(state, PendingOAuthCallback{
		Error:   "access_denied",
		ErrDesc: "user declined",
	}))

	var rtErr error
	select {
	case rtErr = <-rtErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for OAuth flow to abort after direct callback error")
	}
	require.Error(t, rtErr)
	assert.Contains(t, rtErr.Error(), "access_denied")
	assert.Contains(t, rtErr.Error(), "user declined")
	assert.Equal(t, int32(0), srv.tokenCalls.Load(),
		"token endpoint must NOT be hit when the callback carries an error")
}

// TestUnmanagedOAuthFlow_DriveFlow_AbortsOnParentCtxCancellation verifies
// that user-initiated cancellation of the in-progress agent run
// propagates through the OAuth select even though the local ctx has been
// detached from its parent by clientConnector.Connect's
// context.WithoutCancel.
//
// The select watches two cancellation signals: the local (detached) ctx
// and the parent ctx attached via withCancellableParent. Only the
// second one is expected to fire on user-initiated cancellation; this
// test asserts it does, and that the OAuth flow returns the parent's
// ctx error so the agent loop can complete cleanly and the
// per-session streaming lock can be released.
func TestUnmanagedOAuthFlow_DriveFlow_AbortsOnParentCtxCancellation(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	elicitationCanReturn := make(chan struct{})
	defer close(elicitationCanReturn)
	capture := &elicitCaptured{}
	elicitationSent := make(chan struct{}, 1)
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		elicitationSent <- struct{}{}
		<-elicitationCanReturn
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	// Mirror what clientConnector.Connect sets up: a cancellable parent
	// ctx, then a detached ctx that carries the parent as a value.
	parentCtx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	requestCtx := withCancellableParent(context.WithoutCancel(parentCtx), parentCtx)

	rtErrCh := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, srv.URL, strings.NewReader("{}"))
		if err != nil {
			rtErrCh <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		rtErrCh <- err
	}()

	// Wait for the OAuth flow to enter its blocking select (i.e. the
	// elicitation has been sent and the goroutine is waiting on a
	// reply). Cancel the parent ctx; the local ctx remains live, so
	// without the user-cancel branch in the select, this would hang.
	select {
	case <-elicitationSent:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for elicitation to be sent")
	}
	parentCancel()

	var rtErr error
	select {
	case rtErr = <-rtErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("OAuth flow did not abort after parent ctx cancellation")
	}
	require.Error(t, rtErr)
	require.ErrorIs(t, rtErr, context.Canceled,
		"OAuth flow must return the parent's ctx error so the agent loop sees a cancellable error")
	assert.Equal(t, int32(0), srv.tokenCalls.Load(),
		"token endpoint must NOT be hit when the parent ctx is cancelled before any callback")
}

// TestUnmanagedOAuthFlow_DriveFlow_TimesOutWhenNoReplyArrives ensures the
// per-flow deadline releases the streaming lock even when the MCP client
// disconnects silently and never produces an elicitation reply.
//
// Without unmanagedOAuthWaitTimeout, requestElicitation would block on a
// dead session indefinitely, the per-session lock at the SessionManager
// level would stay held, and every subsequent message from that session
// would return 409 / ErrSessionBusy until a process restart.
func TestUnmanagedOAuthFlow_DriveFlow_TimesOutWhenNoReplyArrives(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)
	defer srv.Close()

	const redirectURI = "https://example.test/oauth/cb"
	capture := &elicitCaptured{}
	// Block until the test's t.Context is cancelled (after the
	// roundtrip has returned). This emulates a silent client
	// disconnect: requestElicitation honors its ctx and returns when
	// elicCtx hits its deadline, surfacing the deadline-exceeded
	// error on elicCh.
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		<-t.Context().Done()
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)
	// Short per-transport wait so the timeout fires quickly; no global
	// mutation, so this test stays parallel-safe.
	transport.waitTimeout = 200 * time.Millisecond

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, rtErr := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, rtErr)
	assert.True(t,
		strings.Contains(rtErr.Error(), "timed out") ||
			strings.Contains(rtErr.Error(), "context deadline exceeded"),
		"expected timeout error, got: %v", rtErr,
	)
	assert.Equal(t, int32(0), srv.tokenCalls.Load(),
		"token endpoint must NOT be hit on timeout")
}

// TestRegisterClient_GrantTypesIncludeRefreshToken verifies that dynamic
// client registration (RFC 7591) advertises both grant types the client
// uses. Strict authorization servers like Miro reject registrations that
// omit refresh_token.
func TestRegisterClient_GrantTypesIncludeRefreshToken(t *testing.T) {
	t.Parallel()

	var registrationBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&registrationBody); err != nil {
			t.Fatalf("failed to decode registration request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"test-client","client_secret":"test-secret"}`))
	}))
	defer srv.Close()

	meta := &AuthorizationServerMetadata{
		RegistrationEndpoint: srv.URL,
	}
	clientID, clientSecret, err := RegisterClient(t.Context(), meta, "https://example.test/oauth/cb", nil)
	require.NoError(t, err)
	assert.Equal(t, "test-client", clientID)
	assert.Equal(t, "test-secret", clientSecret)

	grantTypes, _ := registrationBody["grant_types"].([]any)
	require.Len(t, grantTypes, 2, "grant_types must list both grants the client uses")
	assert.Contains(t, grantTypes, "authorization_code", "grant_types must include authorization_code")
	assert.Contains(t, grantTypes, "refresh_token", "grant_types must include refresh_token (RFC 7591; required by strict servers such as Miro)")

	responseTypes, _ := registrationBody["response_types"].([]any)
	assert.Contains(t, responseTypes, "code", "response_types must include code")
}

// TestUnmanagedRedirectURI_PerToolsetTakesPrecedence verifies the precedence
// order: per-toolset RemoteOAuthConfig.CallbackRedirectURL overrides the
// runtime-wide --mcp-oauth-redirect-uri.
func TestUnmanagedRedirectURI_PerToolsetTakesPrecedence(t *testing.T) {
	t.Parallel()

	transport := &oauthTransport{
		unmanagedOAuthRedirectURI: "https://global.example/cb",
		oauthConfig: &latest.RemoteOAuthConfig{
			CallbackRedirectURL: "https://per-toolset.example/cb",
		},
	}
	assert.Equal(t, "https://per-toolset.example/cb", transport.unmanagedRedirectURI())

	transport.oauthConfig = nil
	assert.Equal(t, "https://global.example/cb", transport.unmanagedRedirectURI())

	transport.unmanagedOAuthRedirectURI = ""
	assert.Empty(t, transport.unmanagedRedirectURI())
}

// --------- Server-side invalid_token eviction + refresh tests ---------

// newInvalidTokenTestServer creates an httptest mux emulating a server that:
//   - Returns 401 + WWW-Authenticate: Bearer error="invalid_token" for the
//     stale bearer token "old-at".
//   - Returns 200 for any request bearing a valid bearer token "fresh-at".
//   - Serves OAuth authorization-server metadata and a /token refresh endpoint.
func newInvalidTokenTestServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()

	// Use NewUnstartedServer so we can reference srv.URL in handler closures
	// before the server is started.
	srv := httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-at",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "fresh-rt",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fresh-at" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		// All other tokens (including the stale "old-at") → invalid_token.
		w.Header().Set("WWW-Authenticate", `Bearer realm="test", error="invalid_token"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token","error_description":"Invalid access token"}`))
	})

	return srv, &tokenCalls
}

// newTransportWithStaleToken builds an oauthTransport pre-populated with a
// non-expired stale token "old-at" that the server will reject with
// invalid_token. The token carries a refresh_token and client credentials so
// the silent refresh path can be exercised.
func newTransportWithStaleToken(t *testing.T, baseURL string) *oauthTransport {
	t.Helper()
	store := NewInMemoryTokenStore()
	err := store.StoreToken(baseURL, &OAuthToken{
		AccessToken:  "old-at",
		TokenType:    "Bearer",
		RefreshToken: "old-rt",
		// Non-expired so getValidToken returns it without local-expiry refresh.
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ClientID:     "cid",
		ClientSecret: "csec",
		AuthServer:   baseURL, // metadata discovery goes to the same server
	})
	require.NoError(t, err)
	return &oauthTransport{
		base:       newTestTransport(t),
		tokenStore: store,
		baseURL:    baseURL,
		// Allow private IPs so the httptest 127.0.0.1 server is reachable.
		oauthHTTPClient: oauthHTTPClientForAllowPrivateIPs(true),
	}
}

// TestRoundTrip_ServerInvalidTokenEvictsAndRefreshes is the primary regression
// test for #3198: when the server rejects a non-expired stored token with
// invalid_token, the transport must silently evict the stale token, call the
// token endpoint exactly once to refresh it, and replay the request with the
// new bearer so the caller sees a 200 response.
func TestRoundTrip_ServerInvalidTokenEvictsAndRefreshes(t *testing.T) {
	t.Parallel()

	srv, tokenCalls := newInvalidTokenTestServer(t)

	var oauthSuccessFired atomic.Bool
	transport := newTransportWithStaleToken(t, srv.URL)
	transport.onOAuthSuccess = func() { oauthSuccessFired.Store(true) }

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), tokenCalls.Load(), "token endpoint must be called exactly once")

	// The stored token must now be the freshly-obtained one.
	stored, err := transport.tokenStore.GetToken(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "fresh-at", stored.AccessToken)

	assert.True(t, oauthSuccessFired.Load(), "oauthSuccess must be called after silent refresh")
}

// TestRoundTrip_ServerInvalidToken_RefreshFails_DefersWhenNonInteractive
// verifies that when the server rejects the token with invalid_token but the
// token-endpoint refresh fails, a non-interactive context returns
// IsAuthorizationRequired and the stale token is evicted from the store.
func TestRoundTrip_ServerInvalidToken_RefreshFails_DefersWhenNonInteractive(t *testing.T) {
	t.Parallel()

	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="test", error="invalid_token"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})

	transport := newTransportWithStaleToken(t, srv.URL)

	ctx := WithoutInteractivePrompts(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, rtErr := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, rtErr)
	assert.True(t, IsAuthorizationRequired(rtErr), "must return AuthorizationRequiredError; got: %v", rtErr)

	// Stale token must have been evicted.
	_, storeErr := transport.tokenStore.GetToken(srv.URL)
	require.Error(t, storeErr, "stale token must be evicted from the store")

	// The token endpoint was hit at most once (backoff not yet active).
	assert.Equal(t, int32(1), tokenCalls.Load())

	// A second request within the backoff window must NOT hit the token
	// endpoint again (refreshFailedAt is set).
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("{}"))
	// Re-populate the store with a stale token so roundTrip tries to attach it again.
	_ = transport.tokenStore.StoreToken(srv.URL, &OAuthToken{
		AccessToken:  "old-at",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(time.Hour),
		ClientID:     "cid",
		ClientSecret: "csec",
		AuthServer:   srv.URL,
	})
	resp2, _ := transport.RoundTrip(req2)
	if resp2 != nil {
		resp2.Body.Close()
	}
	assert.Equal(t, int32(1), tokenCalls.Load(), "backoff must prevent a second token-endpoint hit")
}

// TestRoundTrip_ServerInvalidToken_NoRefreshToken_NonInteractive verifies
// that when the stored token has no refresh_token, a non-interactive context
// returns IsAuthorizationRequired without hitting the token endpoint.
func TestRoundTrip_ServerInvalidToken_NoRefreshToken_NonInteractive(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	var tokenCalls atomic.Int32

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="test", error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})

	store := NewInMemoryTokenStore()
	err := store.StoreToken(srv.URL, &OAuthToken{
		AccessToken: "old-at",
		TokenType:   "Bearer",
		// No RefreshToken → refresh path must not be attempted.
		ExpiresAt:  time.Now().Add(time.Hour),
		AuthServer: srv.URL,
	})
	require.NoError(t, err)

	transport := &oauthTransport{
		base:            newTestTransport(t),
		tokenStore:      store,
		baseURL:         srv.URL,
		oauthHTTPClient: oauthHTTPClientForAllowPrivateIPs(true),
	}

	ctx := WithoutInteractivePrompts(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("{}"))
	resp, rtErr := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, rtErr)
	assert.True(t, IsAuthorizationRequired(rtErr))
	assert.Equal(t, int32(0), tokenCalls.Load(), "no refresh token → token endpoint must never be hit")
}

// TestRoundTrip_FirstContact401Unchanged is a regression guard: when no
// token is stored (first-contact), the transport must take the existing
// interactive OAuth flow unchanged — it must NOT treat the first-contact
// 401 as an invalid_token rejection.
func TestRoundTrip_FirstContact401Unchanged(t *testing.T) {
	t.Parallel()

	srv := newUnmanagedOAuthTestServer(t)

	var elicitCalls atomic.Int32
	capture := &elicitCaptured{}
	capture.replyFn = func(_ *gomcp.ElicitParams) tools.ElicitationResult {
		elicitCalls.Add(1)
		return tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"access_token": "first-at",
				"token_type":   "Bearer",
			},
		}
	}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, "", capture)
	// No pre-stored token → first-contact path.

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), elicitCalls.Load(), "first-contact must go through the interactive OAuth flow")
	// Token endpoint was NOT hit for first-contact (client supplied the token directly).
	assert.Equal(t, int32(0), srv.tokenCalls.Load())
}

// TestRoundTrip_ConcurrentInvalidToken_RefreshesOnce verifies the
// coalescing behaviour: when N concurrent roundTrips all see the stale
// token and hit 401 + invalid_token simultaneously, exactly one refresh
// is issued and all N requests eventually succeed with the fresh token.
func TestRoundTrip_ConcurrentInvalidToken_RefreshesOnce(t *testing.T) {
	t.Parallel()

	srv, tokenCalls := newInvalidTokenTestServer(t)

	transport := newTransportWithStaleToken(t, srv.URL)
	transport.onOAuthSuccess = func() {}

	const n = 6
	results := make(chan error, n)

	for range n {
		go func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
			if err != nil {
				results <- err
				return
			}
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			results <- err
		}()
	}

	for range n {
		select {
		case err := <-results:
			require.NoError(t, err, "all concurrent requests must eventually succeed")
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for concurrent round-trips")
		}
	}

	assert.Equal(t, int32(1), tokenCalls.Load(),
		"oauthFlowMu coalescing must ensure the token endpoint is hit exactly once")
}

// TestRoundTrip_Bare401WithAttachedTokenUnchanged is the regression guard for
// blocking issue 1: when a token WAS attached to the request and the server
// returns a plain 401 (no error="invalid_token" in the WWW-Authenticate
// header and no invalid_token in the body), the transport MUST return the 401
// response as-is — no eviction, no /token call, stored credential untouched.
//
// This covers the "app-level authorization failure" case (wrong permissions,
// revoked app access, etc.) where the server rejects the bearer for a reason
// that isn't a stale/revoked token we can silently refresh.
func TestRoundTrip_Bare401WithAttachedTokenUnchanged(t *testing.T) {
	t.Parallel()

	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Main endpoint: 401 with Bearer realm but NO error= code.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})

	store := NewInMemoryTokenStore()
	require.NoError(t, store.StoreToken(srv.URL, &OAuthToken{
		AccessToken:  "good-at",
		TokenType:    "Bearer",
		RefreshToken: "good-rt", // has a refresh token — must NOT be used
		ExpiresAt:    time.Now().Add(time.Hour),
		AuthServer:   srv.URL,
		ClientID:     "cid",
		ClientSecret: "csec",
	}))

	transport := &oauthTransport{
		base:            newTestTransport(t),
		tokenStore:      store,
		baseURL:         srv.URL,
		oauthHTTPClient: oauthHTTPClientForAllowPrivateIPs(true),
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)

	resp, rtErr := transport.RoundTrip(req)
	require.NoError(t, rtErr, "bare 401 must be returned as a normal response, not an error")
	if resp != nil {
		resp.Body.Close()
	}

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"bare 401 without invalid_token must be returned unchanged")
	assert.Equal(t, int32(0), tokenCalls.Load(),
		"token endpoint must NOT be called for a bare 401 without invalid_token")

	// Stored token must be untouched.
	stored, storeErr := transport.tokenStore.GetToken(srv.URL)
	require.NoError(t, storeErr, "token must still be in the store")
	assert.Equal(t, "good-at", stored.AccessToken,
		"stored token must not be evicted or replaced on a bare 401")
}

// TestRoundTrip_ConcurrentInvalidToken_NoRefresh_StickyDecline is the
// regression guard for blocking issue 2: when concurrent requests all hit a
// 401 + error="invalid_token" for a token that has NO refresh_token, the
// transport must fall back to interactive OAuth. The first goroutine runs the
// OAuth flow; the user declines. The sticky-decline latch (lastOAuthDeclined)
// must be observed by all subsequently-queued goroutines so that exactly ONE
// elicitation dialog is popped and all callers surface OAuthDeclinedError.
func TestRoundTrip_ConcurrentInvalidToken_NoRefresh_StickyDecline(t *testing.T) {
	t.Parallel()

	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	// OAuth discovery: needed by handleUnmanagedOAuthFlow before it sends the
	// elicitation. Both endpoints must succeed for the flow to reach elicitation.
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              srv.URL,
			"authorization_servers": []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="test", error="invalid_token"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})

	// Token without a refresh_token: no silent refresh, must fall back to
	// interactive OAuth.
	store := NewInMemoryTokenStore()
	require.NoError(t, store.StoreToken(srv.URL, &OAuthToken{
		AccessToken: "old-at",
		TokenType:   "Bearer",
		// No RefreshToken.
		ExpiresAt:  time.Now().Add(time.Hour),
		AuthServer: srv.URL,
	}))

	var elicitCount atomic.Int32
	capture := &elicitCaptured{}
	capture.replyFn = func(_ *gomcp.ElicitParams) tools.ElicitationResult {
		elicitCount.Add(1)
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}

	client := newRemoteClient(srv.URL, "streamable", nil, store, nil, false, nil)
	client.SetElicitationHandler(capture.handler)
	client.allowPrivateIPs = true

	transport := &oauthTransport{
		base:               newTestTransport(t),
		requestElicitation: client.requestElicitation,
		tokenStore:         store,
		baseURL:            srv.URL,
		managed:            false,
		oauthHTTPClient:    oauthHTTPClientForAllowPrivateIPs(true),
	}

	const n = 5
	results := make(chan error, n)
	for range n {
		go func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
			if err != nil {
				results <- err
				return
			}
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			results <- err
		}()
	}

	for range n {
		select {
		case err := <-results:
			require.Error(t, err)
			assert.True(t, IsOAuthDeclined(err),
				"all concurrent requests must surface OAuthDeclinedError after the user declined; got: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for concurrent round-trips to complete")
		}
	}

	assert.Equal(t, int32(1), elicitCount.Load(),
		"sticky-decline must ensure exactly one elicitation dialog is shown")
	assert.Equal(t, int32(0), tokenCalls.Load(),
		"token endpoint must NOT be hit: no refresh token exists")
}

// TestBuildAuthorizationURL verifies that BuildAuthorizationURL always
// produces a well-formed URL with exactly one '?' regardless of whether the
// auth endpoint already contains a query string. Regression test for #3229.
func TestBuildAuthorizationURL(t *testing.T) {
	t.Parallel()

	const (
		clientID      = "test-client"
		redirectURI   = "https://example.com/callback"
		state         = "test-state"
		codeChallenge = "test-challenge"
		resourceURL   = "https://mcp.example.com"
	)

	tests := []struct {
		name               string
		authEndpoint       string
		scopes             []string
		wantPreservedParam map[string]string // params the endpoint already carried
		wantNoScope        bool
	}{
		{
			name:         "plain endpoint without existing query string",
			authEndpoint: "https://auth.example.com/authorize",
			scopes:       []string{"read", "write"},
		},
		{
			// Regression case: Grafana Cloud bakes grafana_url= into the endpoint.
			name:         "endpoint with pre-existing query string (Grafana Cloud)",
			authEndpoint: "https://mcp.grafana.com/mcp/oauth/authorize?grafana_url=https%3A%2F%2Fdockerinc.grafana.net",
			scopes:       []string{"metrics:read"},
			wantPreservedParam: map[string]string{
				"grafana_url": "https://dockerinc.grafana.net",
			},
		},
		{
			name:         "empty scopes omits scope param",
			authEndpoint: "https://auth.example.com/authorize",
			scopes:       nil,
			wantNoScope:  true,
		},
		{
			name:         "multiple scopes are space-joined",
			authEndpoint: "https://auth.example.com/authorize",
			scopes:       []string{"openid", "profile", "email"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := BuildAuthorizationURL(tc.authEndpoint, clientID, redirectURI, state, codeChallenge, resourceURL, tc.scopes)

			// Exactly one '?' in the result.
			assert.Equal(t, 1, strings.Count(result, "?"),
				"result must contain exactly one '?': %s", result)

			// Parse and inspect individual query parameters.
			u, err := url.Parse(result)
			require.NoError(t, err, "result must be a valid URL")
			q := u.Query()

			assert.Equal(t, "code", q.Get("response_type"))
			assert.Equal(t, clientID, q.Get("client_id"))
			assert.Equal(t, redirectURI, q.Get("redirect_uri"))
			assert.Equal(t, state, q.Get("state"))
			assert.Equal(t, codeChallenge, q.Get("code_challenge"))
			assert.Equal(t, "S256", q.Get("code_challenge_method"))
			assert.Equal(t, resourceURL, q.Get("resource"))

			// scope handling
			if tc.wantNoScope {
				assert.False(t, q.Has("scope"), "scope param must be absent when scopes is empty")
			} else {
				assert.Equal(t, strings.Join(tc.scopes, " "), q.Get("scope"))
			}

			// pre-existing params must survive intact
			for k, want := range tc.wantPreservedParam {
				assert.Equal(t, want, q.Get(k),
					"pre-existing param %q must be preserved unchanged", k)
			}
		})
	}
}

// --------- Client-credential elicitation fallback ---------

// clientCredsElicit returns an elicitation callback that captures the
// oauth_client_credentials request and answers with the given result.
func clientCredsElicit(captured **gomcp.ElicitParams, result tools.ElicitationResult) func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
	return func(_ context.Context, params *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		*captured = params
		return result, nil
	}
}

// TestResolveClientCredentials_NoDCR_PromptsUser verifies the fallback when
// the authorization server does not advertise a registration endpoint: the
// user is asked for pre-registered client credentials via a form
// elicitation, and the configured scopes are preserved on accept. Non-empty
// challenge and PRM scopes are supplied to prove this prompt-fallback
// branch stays configured-only and never widens via discovery.
func TestResolveClientCredentials_NoDCR_PromptsUser(t *testing.T) {
	t.Parallel()

	var captured *gomcp.ElicitParams
	transport := &oauthTransport{
		baseURL: "https://mcp.example.test/mcp",
		requestElicitation: clientCredsElicit(&captured, tools.ElicitationResult{
			Action: tools.ElicitationActionAccept,
			Content: map[string]any{
				"client_id":     "user-client-id",
				"client_secret": "user-secret",
			},
		}),
		oauthConfig: &latest.RemoteOAuthConfig{Scopes: []string{"read", "write"}},
	}

	clientID, clientSecret, scopes, err := transport.resolveClientCredentials(
		t.Context(), &AuthorizationServerMetadata{}, "https://example.test/cb",
		[]string{"challenge-scope"}, []string{"prm-scope"},
	)
	require.NoError(t, err)
	assert.Equal(t, "user-client-id", clientID)
	assert.Equal(t, "user-secret", clientSecret)
	assert.Equal(t, []string{"read", "write"}, scopes,
		"configured scopes must be preserved, never widened by the challenge scope or PRM scopes_supported, when the user supplies the client credentials")

	require.NotNil(t, captured, "a client-credentials elicitation must have been sent")
	assert.Equal(t, "oauth_client_credentials", captured.Meta["docker-agent/type"])
	assert.Equal(t, transport.baseURL, captured.Meta["docker-agent/server_url"])
	schema, ok := captured.RequestedSchema.(map[string]any)
	require.True(t, ok, "the elicitation must carry a form schema")
	assert.Contains(t, schema["required"], "client_id", "client_id must be a required form field")
}

// TestResolveClientCredentials_DCRFails_PromptsUser verifies that a failing
// dynamic client registration falls back to asking the user, instead of
// aborting the flow outright. Non-empty challenge and PRM scopes are
// supplied, with no scopes configured, to prove this branch omits the
// scope entirely rather than falling back to what discovery advertised.
func TestResolveClientCredentials_DCRFails_PromptsUser(t *testing.T) {
	t.Parallel()

	var registerCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registerCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var captured *gomcp.ElicitParams
	transport := &oauthTransport{
		baseURL: "https://mcp.example.test/mcp",
		requestElicitation: clientCredsElicit(&captured, tools.ElicitationResult{
			Action:  tools.ElicitationActionAccept,
			Content: map[string]any{"client_id": "user-client-id"},
		}),
		oauthHTTPClient: oauthHTTPClientForAllowPrivateIPs(true),
	}

	clientID, clientSecret, scopes, err := transport.resolveClientCredentials(
		t.Context(),
		&AuthorizationServerMetadata{RegistrationEndpoint: srv.URL},
		"https://example.test/cb",
		[]string{"challenge-scope"}, []string{"prm-scope"},
	)
	require.NoError(t, err)
	assert.Equal(t, int32(1), registerCalls.Load(), "dynamic registration must be attempted first")
	assert.Equal(t, "user-client-id", clientID)
	assert.Empty(t, clientSecret, "client_secret is optional")
	assert.Nil(t, scopes, "no scopes configured: the scope must be omitted, not filled in from the challenge or PRM scopes_supported")
	require.NotNil(t, captured)
	assert.Equal(t, "oauth_client_credentials", captured.Meta["docker-agent/type"])
}

// TestResolveClientCredentials_ExplicitClientID_ConfiguredOnly verifies the
// explicit-credentials branch (t.oauthConfig.ClientID set): it must return
// exactly the configured client_id/client_secret/scopes and never attempt
// dynamic client registration, even when the authorization server
// advertises a registration endpoint and non-empty challenge/PRM scopes are
// available. Absent configured scopes must come back as nil (omitted),
// never auto-filled from discovery.
func TestResolveClientCredentials_ExplicitClientID_ConfiguredOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured []string
		wantScopes []string
	}{
		{name: "configured scopes are returned exactly", configured: []string{"read", "write"}, wantScopes: []string{"read", "write"}},
		{name: "absent configured scopes are omitted", configured: nil, wantScopes: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var registerCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				registerCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"client_id":"should-not-be-used"}`))
			}))
			defer srv.Close()

			transport := &oauthTransport{
				baseURL: "https://mcp.example.test/mcp",
				oauthConfig: &latest.RemoteOAuthConfig{
					ClientID:     "explicit-client-id",
					ClientSecret: "explicit-secret",
					Scopes:       tt.configured,
				},
				oauthHTTPClient: oauthHTTPClientForAllowPrivateIPs(true),
			}

			clientID, clientSecret, scopes, err := transport.resolveClientCredentials(
				t.Context(),
				&AuthorizationServerMetadata{RegistrationEndpoint: srv.URL},
				"https://example.test/cb",
				[]string{"challenge-scope"}, []string{"prm-scope"},
			)
			require.NoError(t, err)
			assert.Equal(t, "explicit-client-id", clientID)
			assert.Equal(t, "explicit-secret", clientSecret, "the explicit client secret must be forwarded unchanged")
			assert.Equal(t, tt.wantScopes, scopes)
			assert.Equal(t, int32(0), registerCalls.Load(),
				"an explicit client_id must never trigger a dynamic client registration request")
		})
	}
}

// TestResolveClientCredentials_PromptDeclined verifies decline and cancel
// both surface the OAuthDeclinedError sentinel (wrapped, but reachable via
// errors.As) so the sticky-decline latch in startInteractiveFlowLocked keeps
// working for this dialog too.
func TestResolveClientCredentials_PromptDeclined(t *testing.T) {
	t.Parallel()

	for _, action := range []tools.ElicitationAction{tools.ElicitationActionDecline, tools.ElicitationActionCancel} {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()

			var captured *gomcp.ElicitParams
			transport := &oauthTransport{
				baseURL:            "https://mcp.example.test/mcp",
				requestElicitation: clientCredsElicit(&captured, tools.ElicitationResult{Action: action}),
			}

			clientID, clientSecret, _, err := transport.resolveClientCredentials(
				t.Context(), &AuthorizationServerMetadata{}, "https://example.test/cb", nil, nil,
			)
			require.Error(t, err)
			assert.Empty(t, clientID)
			assert.Empty(t, clientSecret)
			assert.True(t, IsOAuthDeclined(err),
				"a declined/cancelled credentials prompt must surface OAuthDeclinedError; got: %v", err)
			assert.Contains(t, err.Error(), "dynamic client registration",
				"the error must explain why the credentials were needed")
		})
	}
}

// TestResolveClientCredentials_PromptMissingClientID verifies that an
// accepted reply without a usable client_id is rejected.
func TestResolveClientCredentials_PromptMissingClientID(t *testing.T) {
	t.Parallel()

	for name, content := range map[string]map[string]any{
		"absent":     {},
		"empty":      {"client_id": ""},
		"whitespace": {"client_id": "   "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var captured *gomcp.ElicitParams
			transport := &oauthTransport{
				baseURL: "https://mcp.example.test/mcp",
				requestElicitation: clientCredsElicit(&captured, tools.ElicitationResult{
					Action:  tools.ElicitationActionAccept,
					Content: content,
				}),
			}

			clientID, clientSecret, _, err := transport.resolveClientCredentials(
				t.Context(), &AuthorizationServerMetadata{}, "https://example.test/cb", nil, nil,
			)
			require.Error(t, err)
			assert.Empty(t, clientID)
			assert.Empty(t, clientSecret)
			assert.Contains(t, err.Error(), "client_id", "the error must mention the missing field")
		})
	}
}

// TestResolveClientCredentials_NoElicitationBridgeDefersAuth verifies the
// no-handler behaviour: with no elicitation callback wired up the fallback
// defers with the recognisable AuthorizationRequiredError (same contract as
// requestElicitation), so a non-interactive startup probe doesn't hard-fail.
func TestResolveClientCredentials_NoElicitationBridgeDefersAuth(t *testing.T) {
	t.Parallel()

	transport := &oauthTransport{baseURL: "https://mcp.example.test/mcp"}

	clientID, clientSecret, _, err := transport.resolveClientCredentials(
		t.Context(), &AuthorizationServerMetadata{}, "https://example.test/cb", nil, nil,
	)
	require.Error(t, err)
	assert.Empty(t, clientID)
	assert.Empty(t, clientSecret)
	assert.True(t, IsAuthorizationRequired(err),
		"a missing elicitation bridge must defer with AuthorizationRequiredError; got: %v", err)
}

// TestUnmanagedOAuthFlow_DriveFlow_NoDCR_UsesPromptedCredentials proves the
// fallback end-to-end on the docker-agent-driven unmanaged flow: with no
// registration endpoint advertised, the flow prompts for client
// credentials, builds the authorize URL with them, and uses them for the
// token exchange.
func TestUnmanagedOAuthFlow_DriveFlow_NoDCR_UsesPromptedCredentials(t *testing.T) {
	t.Parallel()

	var tokenForm url.Values
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL},
		})
	})
	// Metadata WITHOUT a registration endpoint: DCR is unsupported.
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource="`+srv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	const redirectURI = "https://example.test/oauth/cb"
	capture := &elicitCaptured{}
	var credsPrompted atomic.Int32
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		// First elicitation asks for client credentials, second runs the
		// OAuth flow itself.
		if req.Meta["docker-agent/type"] == "oauth_client_credentials" {
			credsPrompted.Add(1)
			return tools.ElicitationResult{
				Action: tools.ElicitationActionAccept,
				Content: map[string]any{
					"client_id":     "user-client-id",
					"client_secret": "user-secret",
				},
			}
		}
		state, _ := req.Meta["docker-agent/state"].(string)
		authorizeURL, _ := req.Meta["docker-agent/authorize_url"].(string)
		assert.Contains(t, authorizeURL, "client_id=user-client-id",
			"the authorize URL must be built with the prompted client_id")
		return tools.ElicitationResult{
			Action:  tools.ElicitationActionAccept,
			Content: map[string]any{"code": "abc", "state": state},
		}
	}
	transport, client := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, int32(1), credsPrompted.Load(), "the client-credentials form must be shown exactly once")
	require.NotNil(t, tokenForm, "the token endpoint must have been hit")
	assert.Equal(t, "user-client-id", tokenForm.Get("client_id"),
		"the token exchange must use the prompted client_id")
	assert.Equal(t, "user-secret", tokenForm.Get("client_secret"),
		"the token exchange must use the prompted client_secret")

	tok, err := client.tokenStore.GetToken(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user-client-id", tok.ClientID, "credentials must be stamped on the token for later refresh")
	assert.Equal(t, "user-secret", tok.ClientSecret)
}

// TestManagedOAuthFlow_NoDCR_PromptsForCredentialsThenHonorsConsentDecline
// drives handleManagedOAuthFlow directly against an httptest authorization
// server whose metadata advertises no registration endpoint. The flow must
// reach the client-credentials prompt fallback, then stop with the user's
// decline of the subsequent authorization consent. The decline lands before
// RequestAuthorizationCode, so no browser is ever launched, and the callback
// server binds an ephemeral 127.0.0.1 port (oauthConfig is nil → port 0), so
// the test needs no port coordination.
func TestManagedOAuthFlow_NoDCR_PromptsForCredentialsThenHonorsConsentDecline(t *testing.T) {
	t.Parallel()

	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL},
		})
	})
	// Metadata WITHOUT a registration endpoint: DCR is unsupported, so the
	// flow must fall back to prompting for client credentials.
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})

	// The flow is single-goroutine, so plain slice appends are safe here.
	var elicitTypes []string
	capture := &elicitCaptured{}
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		typ, _ := req.Meta["docker-agent/type"].(string)
		elicitTypes = append(elicitTypes, typ)
		if typ == "oauth_client_credentials" {
			return tools.ElicitationResult{
				Action: tools.ElicitationActionAccept,
				Content: map[string]any{
					"client_id":     "user-client-id",
					"client_secret": "user-secret",
				},
			}
		}
		// The authorization consent that would otherwise open the browser.
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}
	}

	store := NewInMemoryTokenStore()
	transport := &oauthTransport{
		base:               newTestTransport(t),
		requestElicitation: capture.handler,
		tokenStore:         store,
		baseURL:            srv.URL,
		managed:            true,
		oauthHTTPClient:    oauthHTTPClientForAllowPrivateIPs(true),
	}

	err := transport.handleManagedOAuthFlow(t.Context(), srv.URL, "")
	require.EqualError(t, err, "user declined OAuth authorization")

	assert.Equal(t, []string{"oauth_client_credentials", "oauth_flow"}, elicitTypes,
		"the credentials fallback must run first, then the authorization consent")
	assert.Equal(t, int32(0), tokenCalls.Load(),
		"token endpoint must NOT be hit after the consent decline")
	_, getErr := store.GetToken(srv.URL)
	require.Error(t, getErr, "no token must be stored after a declined consent")
}

// --------- DCR scope selection & propagation (issue #3879) ---------
//
// A successful dynamic client registration previously always registered
// with no scope at all, regardless of what was configured, challenged, or
// advertised by the server. These tests lock in the deterministic
// selection contract (configured > challenge > PRM scopes_supported > none)
// and prove the same selected scopes reach every downstream consumer: the
// DCR request itself, the /authorize URL, and the stored token's
// RequestedScopes.

func TestSelectDCRScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		configured      []string
		challengeScopes []string
		prmScopes       []string
		want            []string
	}{
		{
			name:            "configured wins over challenge and PRM",
			configured:      []string{"read", "write"},
			challengeScopes: []string{"admin"},
			prmScopes:       []string{"everything"},
			want:            []string{"read", "write"},
		},
		{
			name:            "challenge scope used when nothing is configured",
			challengeScopes: []string{"repo"},
			prmScopes:       []string{"everything"},
			want:            []string{"repo"},
		},
		{
			name:      "PRM scopes_supported used when configured and challenge are both empty",
			prmScopes: []string{"read", "write"},
			want:      []string{"read", "write"},
		},
		{
			name: "omitted entirely (nil) when none of the three are available",
			want: nil,
		},
		{
			name:            "a challenge scope of only whitespace falls through to PRM",
			challengeScopes: []string{"  "},
			prmScopes:       []string{"read"},
			want:            []string{"read"},
		},
		{
			name:       "duplicates and surrounding whitespace are normalized, order preserved",
			configured: []string{" read ", "write", "read"},
			want:       []string{"read", "write"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, selectDCRScopes(tt.configured, tt.challengeScopes, tt.prmScopes))
		})
	}
}

func TestChallengeScopesFromWWWAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wwwAuth string
		want    []string
	}{
		{
			name:    "quoted multi-value scope",
			wwwAuth: `Bearer resource="https://x", scope="read write"`,
			want:    []string{"read", "write"},
		},
		{name: "unquoted single scope", wwwAuth: `Bearer scope=read`, want: []string{"read"}},
		{name: "no scope parameter", wwwAuth: `Bearer resource="https://x"`, want: nil},
		{name: "empty header", wwwAuth: "", want: nil},
		{
			// The literal text "scope=read" is entirely inside the quoted
			// value of error_description, not a top-level auth-param: a
			// naive regex scan of the raw header would still match it and
			// inject a bogus scope into DCR selection.
			name:    "scope= text inside another parameter's quoted value is ignored",
			wwwAuth: `Bearer error="insufficient_scope", error_description="Missing scope=read write; please retry"`,
			want:    nil,
		},
		{
			name:    "a genuine top-level scope param survives alongside a decoy quoted one",
			wwwAuth: `Bearer error_description="try scope=bogus", scope="read write"`,
			want:    []string{"read", "write"},
		},
		{
			name:    "parameter name is case-insensitive",
			wwwAuth: `Bearer SCOPE="read write"`,
			want:    []string{"read", "write"},
		},
		{
			name:    "parameter ordering does not matter",
			wwwAuth: `Bearer scope="read write", resource="https://x", resource_metadata="https://y"`,
			want:    []string{"read", "write"},
		},
		{
			// The scanner must not stop at Basic's bare-token-less realm
			// challenge boundary and miss the Bearer challenge that follows.
			name:    "multi-challenge: scope on a later Bearer challenge survives",
			wwwAuth: `Basic realm="corp", Bearer scope="read write"`,
			want:    []string{"read", "write"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, challengeScopesFromWWWAuth(tt.wwwAuth))
		})
	}
}

// TestResourceMetadataFromWWWAuth_IgnoresQuotedValueFalsePositive proves
// resourceMetadataFromWWWAuth is quote-aware the same way
// challengeScopesFromWWWAuth is: "resource_metadata=" (or "resource=") text
// that appears inside another parameter's quoted value must never be read
// as a top-level auth-param, and a genuine top-level param must still be
// found alongside such a decoy.
func TestResourceMetadataFromWWWAuth_IgnoresQuotedValueFalsePositive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wwwAuth string
		want    string
	}{
		{
			name:    "resource_metadata= text inside a quoted value is ignored",
			wwwAuth: `Bearer error="invalid_request", error_description="see resource_metadata=\"https://evil.test\""`,
			want:    "",
		},
		{
			name:    "resource= text inside a quoted value is ignored",
			wwwAuth: `Bearer error_description="try resource=https://evil.test instead"`,
			want:    "",
		},
		{
			name:    "a genuine top-level resource_metadata param survives alongside a decoy quoted one",
			wwwAuth: `Bearer error_description="see resource_metadata=https://evil.test", resource_metadata="https://real.test/prm"`,
			want:    "https://real.test/prm",
		},
		{
			// Live Atlassian-shaped example: a Basic
			// challenge precedes the Bearer one carrying resource_metadata.
			name:    "multi-challenge: resource_metadata on a later Bearer challenge survives",
			wwwAuth: `Basic realm="corp", Bearer resource_metadata="https://real.test/prm"`,
			want:    "https://real.test/prm",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resourceMetadataFromWWWAuth(tt.wwwAuth))
		})
	}
}

// TestParseAuthParams_MultiChallenge proves the quote-aware auth-param
// scanner continues scanning after a bare auth-scheme token instead of
// stopping there, so a valid RFC 7235 multi-challenge WWW-Authenticate
// header does not lose the params carried by a later challenge.
func TestParseAuthParams_MultiChallenge(t *testing.T) {
	t.Parallel()

	t.Run("Basic then Bearer: resource_metadata on the later challenge survives", func(t *testing.T) {
		t.Parallel()
		wwwAuth := `Basic realm="corp", Bearer resource_metadata="https://real.test/prm"`
		assert.Equal(t, map[string]string{
			"realm":             "corp",
			"resource_metadata": "https://real.test/prm",
		}, parseAuthParams(wwwAuth))
		assert.Equal(t, "https://real.test/prm", resourceMetadataFromWWWAuth(wwwAuth))
	})

	t.Run("Basic then Bearer: scope and error on the later challenge both survive", func(t *testing.T) {
		t.Parallel()
		wwwAuth := `Basic realm="corp", Bearer error="invalid_token", scope="read write"`
		assert.Equal(t, []string{"read", "write"}, challengeScopesFromWWWAuth(wwwAuth))
		assert.Equal(t, "invalid_token", errorCodeFromWWWAuth(wwwAuth))
	})

	t.Run("quoted commas and name= text spanning the challenge boundary do not confuse the scan", func(t *testing.T) {
		t.Parallel()
		// realm's quoted value contains a comma, and error_description's
		// quoted value contains decoy "scope=" and "resource_metadata="
		// text; only the genuine top-level params after the Bearer scheme
		// token must be extracted.
		wwwAuth := `Basic realm="corp, still corp", Bearer error_description="try scope=bogus, resource_metadata=https://evil.test", scope="read write", resource_metadata="https://real.test/prm"`
		assert.Equal(t, []string{"read", "write"}, challengeScopesFromWWWAuth(wwwAuth))
		assert.Equal(t, "https://real.test/prm", resourceMetadataFromWWWAuth(wwwAuth))
	})

	t.Run("single-challenge behavior is unchanged", func(t *testing.T) {
		t.Parallel()
		wwwAuth := `Bearer resource_metadata="https://real.test/prm", scope="read write", error="invalid_token"`
		assert.Equal(t, map[string]string{
			"resource_metadata": "https://real.test/prm",
			"scope":             "read write",
			"error":             "invalid_token",
		}, parseAuthParams(wwwAuth))
	})

	t.Run("token68 credential with no '=' padding: later Bearer scope still found", func(t *testing.T) {
		t.Parallel()
		// The token68 credential has no auth-params of its own, so skipping
		// it as a bare token and continuing is safe: it never appears as a
		// map key (the leading auth-scheme, "Negotiate", is stripped once
		// up front, same as for a single-challenge header).
		wwwAuth := `Negotiate YWxhZGRpbjpvcGVuc2VzYW1l, Bearer scope="read"`
		assert.Equal(t, map[string]string{"scope": "read"}, parseAuthParams(wwwAuth))
	})

	t.Run("token68 credential with '=' padding: accepted limitation, pinned", func(t *testing.T) {
		t.Parallel()
		// RFC 7235 token68 syntax allows trailing "=" padding, which this
		// scanner cannot distinguish from a bare name followed by "=value"
		// once it has skipped past the credential's start. The padded
		// token68 is therefore misread as its own (bogus, harmless) param
		// instead of being recognized as one opaque credential. This is a
		// documented, deterministic limitation — not a crash or data loss
		// — and it does not prevent the later Bearer challenge's genuine
		// scope from being found.
		wwwAuth := `Negotiate YWxhZGRpbjpvcGVuc2VzYW1l=, Bearer scope="read"`
		got := parseAuthParams(wwwAuth)
		assert.Equal(t, "read", got["scope"])
		assert.Empty(t, got["ywxhzgrpbjpvcgvuc2vzyw1l"])
		assert.Len(t, got, 2)
	})
}

// TestResolveClientCredentials_DCRSuccess_ScopeSelection drives the real
// RFC 7591 registration request against a local httptest server and
// verifies both that the deterministic priority order is respected and
// that resolveClientCredentials returns exactly the scopes it sent to the
// registration endpoint, across the "nothing available" case too (the
// scope field must be omitted entirely, not sent as empty).
func TestResolveClientCredentials_DCRSuccess_ScopeSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		configured      []string
		challengeScopes []string
		prmScopes       []string
		wantScope       string // exact "scope" field the DCR request must carry; empty means the field must be absent
	}{
		{
			name:            "configured scopes take priority",
			configured:      []string{"read", "write"},
			challengeScopes: []string{"admin"},
			prmScopes:       []string{"everything"},
			wantScope:       "read write",
		},
		{
			name:            "challenge scope used when nothing configured",
			challengeScopes: []string{"repo"},
			prmScopes:       []string{"everything"},
			wantScope:       "repo",
		},
		{
			name:      "PRM scopes_supported used when configured and challenge are both empty",
			prmScopes: []string{"read", "write"},
			wantScope: "read write",
		},
		{
			name: "scope omitted entirely when none of the three are available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"client_id":"registered-client-id"}`))
			}))
			defer srv.Close()

			var oauthConfig *latest.RemoteOAuthConfig
			if len(tt.configured) > 0 {
				oauthConfig = &latest.RemoteOAuthConfig{Scopes: tt.configured}
			}
			transport := &oauthTransport{
				baseURL:         "https://mcp.example.test/mcp",
				oauthConfig:     oauthConfig,
				oauthHTTPClient: oauthHTTPClientForAllowPrivateIPs(true),
			}

			clientID, _, scopes, err := transport.resolveClientCredentials(
				t.Context(),
				&AuthorizationServerMetadata{RegistrationEndpoint: srv.URL},
				"https://example.test/cb",
				tt.challengeScopes,
				tt.prmScopes,
			)
			require.NoError(t, err)
			assert.Equal(t, "registered-client-id", clientID)

			gotScope, hadScope := gotBody["scope"]
			if tt.wantScope == "" {
				assert.False(t, hadScope, "DCR request must omit the scope field entirely, not send it empty")
				assert.Nil(t, scopes)
			} else {
				assert.Equal(t, tt.wantScope, gotScope, "DCR request must carry the selected scopes")
				assert.Equal(t, strings.Fields(tt.wantScope), scopes,
					"resolveClientCredentials must return the exact scopes sent to the DCR request")
			}
		})
	}
}

// TestUnmanagedOAuthFlow_DriveFlow_DCRSuccess_ScopesPropagate exercises the
// full docker-agent-driven unmanaged flow end to end with an empty
// challenge scope, proving the PRM scopes_supported fallback reaches every
// downstream consumer: the DCR request, the /authorize URL sent to the
// client, and the stored token's RequestedScopes used later for refresh and
// scope-coverage checks.
func TestUnmanagedOAuthFlow_DriveFlow_DCRSuccess_ScopesPropagate(t *testing.T) {
	t.Parallel()

	var registerScope string
	var haveRegisterScope bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL},
			ScopesSupported:      []string{"repo", "profile"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
			RegistrationEndpoint:  srv.URL + "/register",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["scope"].(string); ok {
			registerScope, haveRegisterScope = s, true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id","client_secret":"registered-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})
	// The 401 challenge carries no scope parameter: the flow must fall
	// through to PRM scopes_supported rather than skip scope selection.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource="`+srv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	const redirectURI = "https://example.test/oauth/cb"
	var authorizeURL string
	capture := &elicitCaptured{}
	capture.replyFn = func(req *gomcp.ElicitParams) tools.ElicitationResult {
		authorizeURL, _ = req.Meta["docker-agent/authorize_url"].(string)
		state, _ := req.Meta["docker-agent/state"].(string)
		return tools.ElicitationResult{
			Action:  tools.ElicitationActionAccept,
			Content: map[string]any{"code": "abc", "state": state},
		}
	}
	transport, client := newUnmanagedTestTransport(t, srv.URL, redirectURI, capture)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.True(t, haveRegisterScope, "DCR request must carry a scope field")
	assert.Equal(t, "repo profile", registerScope, "DCR must request the PRM scopes_supported fallback")

	authURL, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	assert.Equal(t, "repo profile", authURL.Query().Get("scope"),
		"the /authorize request must carry the exact same scopes sent to DCR")

	tok, err := client.tokenStore.GetToken(srv.URL)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, []string{"repo", "profile"}, tok.RequestedScopes,
		"the stored token's RequestedScopes must match the scopes sent to DCR and /authorize")
}

// TestHandleManagedOAuthFlow_DCRSuccess_ConfiguredScopesWinOverChallengeAndPRM
// proves the browser-driven managed flow (the other resolveClientCredentials
// call site) wires the challenge scope and PRM scopes_supported through
// correctly too, and that configured scopes still win the priority order
// there. The consent dialog is declined so the flow stops right after DCR,
// before it would open a real browser tab via RequestAuthorizationCode.
func TestHandleManagedOAuthFlow_DCRSuccess_ConfiguredScopesWinOverChallengeAndPRM(t *testing.T) {
	t.Parallel()

	var registerScope string
	var haveRegisterScope bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL},
			ScopesSupported:      []string{"prm-scope"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
			RegistrationEndpoint:  srv.URL + "/register",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["scope"].(string); ok {
			registerScope, haveRegisterScope = s, true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id"}`))
	})

	capture := &elicitCaptured{reply: tools.ElicitationResult{Action: tools.ElicitationActionDecline}}
	transport := &oauthTransport{
		base:               newTestTransport(t),
		requestElicitation: capture.handler,
		tokenStore:         NewInMemoryTokenStore(),
		baseURL:            srv.URL,
		managed:            true,
		oauthConfig:        &latest.RemoteOAuthConfig{Scopes: []string{"configured-scope"}},
		oauthHTTPClient:    oauthHTTPClientForAllowPrivateIPs(true),
	}

	// The challenge advertises a different scope than configured; it must
	// lose to the configured scope per the deterministic selection order.
	err := transport.handleManagedOAuthFlow(t.Context(), srv.URL, `Bearer scope="challenge-scope"`)
	require.EqualError(t, err, "user declined OAuth authorization")

	require.True(t, haveRegisterScope, "DCR request must carry a scope field")
	assert.Equal(t, "configured-scope", registerScope,
		"configured scopes must win over both the challenge scope and PRM scopes_supported")
}

// fakeBrowserOpener swaps the process-wide browser opener for one that
// captures the authorization URL into a buffered channel instead of launching
// a real browser. This makes browser-fixture tests host-independent (no POSIX
// PATH shim, no platform-specific launcher). Restores the original on cleanup.
// Mutating the process-wide opener is incompatible with t.Parallel.
func fakeBrowserOpener(t *testing.T) <-chan string {
	t.Helper()
	urlCh := make(chan string, 1)
	restore := oauthflow.SetBrowserOpenerForTesting(func(_ context.Context, rawURL string) error {
		select {
		case urlCh <- rawURL:
		default:
		}
		return nil
	})
	t.Cleanup(restore)
	return urlCh
}

// requireCapturedAuthorizeURL waits for the fake browser opener to deliver
// the authorization URL, timing out after 15 seconds.
func requireCapturedAuthorizeURL(t *testing.T, urlCh <-chan string) string {
	t.Helper()
	select {
	case u := <-urlCh:
		return u
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the fake browser opener to receive the authorize URL")
		return ""
	}
}

// deliverFakeCallback simulates the browser completing the authorization
// consent screen and being redirected back to the local loopback callback
// server: it extracts redirect_uri and state from authURL and issues the
// callback request itself, standing in for the human + IdP round trip.
func deliverFakeCallback(t *testing.T, authURL, code string) {
	t.Helper()

	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	redirectURI := parsed.Query().Get("redirect_uri")
	require.NotEmpty(t, redirectURI, "authorize URL must carry redirect_uri")
	state := parsed.Query().Get("state")
	require.NotEmpty(t, state, "authorize URL must carry state")

	cbURL, err := url.Parse(redirectURI)
	require.NoError(t, err)
	cbQuery := cbURL.Query()
	cbQuery.Set("code", code)
	cbQuery.Set("state", state)
	cbURL.RawQuery = cbQuery.Encode()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cbURL.String(), http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "fake OAuth callback must be accepted")
}

// TestHandleManagedOAuthFlow_DCRSuccess_EndToEndScopeEquality drives the
// managed flow all the way to completion — consent accept, the (faked)
// browser hand-off, the loopback callback, code exchange, and token
// storage — and proves the exact same selected scope set reaches every
// place that must agree on it: the DCR registration request body, the
// /authorize URL's scope parameter, and the stored token's RequestedScopes.
func TestHandleManagedOAuthFlow_DCRSuccess_EndToEndScopeEquality(t *testing.T) {
	urlCh := fakeBrowserOpener(t)

	var registerScope string
	var haveRegisterScope bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL},
			ScopesSupported:      []string{"prm-scope"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
			RegistrationEndpoint:  srv.URL + "/register",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["scope"].(string); ok {
			registerScope, haveRegisterScope = s, true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id","client_secret":"registered-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	capture := &elicitCaptured{reply: tools.ElicitationResult{Action: tools.ElicitationActionAccept}}
	store := NewInMemoryTokenStore()
	transport := &oauthTransport{
		base:               newTestTransport(t),
		requestElicitation: capture.handler,
		tokenStore:         store,
		baseURL:            srv.URL,
		managed:            true,
		oauthConfig:        &latest.RemoteOAuthConfig{Scopes: []string{"configured-scope"}},
		oauthHTTPClient:    oauthHTTPClientForAllowPrivateIPs(true),
	}

	errCh := make(chan error, 1)
	go func() {
		// The challenge advertises a different scope than configured, and
		// PRM scopes_supported advertises a third: configured scopes must
		// still win the priority order all the way through to a stored token.
		errCh <- transport.handleManagedOAuthFlow(t.Context(), srv.URL, `Bearer scope="challenge-scope"`)
	}()

	authURL := requireCapturedAuthorizeURL(t, urlCh)
	deliverFakeCallback(t, authURL, "test-authorization-code")

	require.NoError(t, <-errCh)

	require.True(t, haveRegisterScope, "DCR request must carry a scope field")
	assert.Equal(t, "configured-scope", registerScope, "DCR registration request must carry the selected scope")

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "configured-scope", parsedAuthURL.Query().Get("scope"),
		"the /authorize URL must carry the exact same scope sent to DCR")

	tok, err := store.GetToken(srv.URL)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, []string{"configured-scope"}, tok.RequestedScopes,
		"the stored token's RequestedScopes must match the scope sent to DCR and /authorize")
}
