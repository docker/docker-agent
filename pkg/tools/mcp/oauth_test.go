package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestExchangeCodeForToken_PreservesClientCredentials verifies that
// ExchangeCodeForToken stores the client_id and client_secret on the
// returned OAuthToken so they are available for subsequent refresh calls.
func TestExchangeCodeForToken_PreservesClientCredentials(t *testing.T) {
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
		AccessToken:  "old-at",
		TokenType:    "Bearer",
		RefreshToken: "old-rt",
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // expired
		ClientID:     "stored-cid",
		ClientSecret: "stored-csec",
	}
	if err := store.StoreToken(srv.URL, expiredToken); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       http.DefaultTransport,
		tokenStore: store,
		baseURL:    srv.URL,
	}

	got := transport.getValidToken(t.Context())
	if got == nil {
		t.Fatal("getValidToken returned nil, expected refreshed token")
	}
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
}

// TestGetValidToken_UsesStoredAuthServerForRefresh verifies that silent
// refresh uses the discovered auth server rather than assuming the MCP
// server URL also hosts the OAuth metadata.
func TestGetValidToken_UsesStoredAuthServerForRefresh(t *testing.T) {
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
		base:       http.DefaultTransport,
		tokenStore: store,
		baseURL:    mcpSrv.URL,
	}

	got := transport.getValidToken(t.Context())
	if got == nil {
		t.Fatal("getValidToken returned nil, expected refreshed token")
	}
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
	cs, err := NewCallbackServer()
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
	resp, err := http.DefaultClient.Do(req)
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

// TestOAuthTransport_RetryFailureExposesResponseBody verifies that when
// the authenticated retry after a successful OAuth flow still fails with
// a non-2xx status, the response body is logged and preserved for the
// caller. Without this, diagnosing post-OAuth server errors is limited
// to the generic HTTP status text, which hides useful provider-specific
// detail such as scope mismatches or payload complaints.
func TestOAuthTransport_RetryFailureExposesResponseBody(t *testing.T) {
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
		base:       http.DefaultTransport,
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
		base:       http.DefaultTransport,
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
	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken:     "stale-at",
		TokenType:       "Bearer",
		RequestedScopes: []string{"users:read"},
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       http.DefaultTransport,
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
	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken:     "good-at",
		TokenType:       "Bearer",
		RequestedScopes: []string{"users:read", "channels:history", "extra:scope"},
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       http.DefaultTransport,
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

// TestGetValidToken_LeavesLegacyTokenAlone verifies that stored tokens
// that predate the RequestedScopes field (empty slice) are treated as
// sufficient, so an upgrade doesn't forcibly invalidate every existing
// user's session.
func TestGetValidToken_LeavesLegacyTokenAlone(t *testing.T) {
	store := NewInMemoryTokenStore()
	if err := store.StoreToken("https://mcp.example.com", &OAuthToken{
		AccessToken: "legacy-at",
		TokenType:   "Bearer",
		// RequestedScopes intentionally nil (legacy).
	}); err != nil {
		t.Fatal(err)
	}

	transport := &oauthTransport{
		base:       http.DefaultTransport,
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource="https://example.test/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	transport := &oauthTransport{
		base:       http.DefaultTransport,
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
