package mcp

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestProbeUnauthenticated characterizes probeUnauthenticated: it makes one
// GET request and reports the WWW-Authenticate header verbatim, or "" when
// the server answers without an OAuth challenge.
func TestProbeUnauthenticated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		wwwAuth    string
		wantHeader string
	}{
		{
			name:       "401 with Bearer challenge is captured verbatim",
			status:     http.StatusUnauthorized,
			wwwAuth:    `Bearer resource_metadata="https://res.example.test/.well-known/oauth-protected-resource"`,
			wantHeader: `Bearer resource_metadata="https://res.example.test/.well-known/oauth-protected-resource"`,
		},
		{
			name:       "200 with no auth required yields an empty challenge",
			status:     http.StatusOK,
			wantHeader: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotMethod, gotAccept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotAccept = r.Header.Get("Accept")
				if tt.wwwAuth != "" {
					w.Header().Set("WWW-Authenticate", tt.wwwAuth)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			got, err := probeUnauthenticated(t.Context(), srv.Client(), srv.URL)
			require.NoError(t, err)
			assert.Equal(t, tt.wantHeader, got)
			assert.Equal(t, http.MethodGet, gotMethod, "probe must use GET")
			assert.NotEmpty(t, gotAccept, "probe must advertise an Accept header")
		})
	}
}

// TestProtectedResourceMetadataCandidates covers the discovery selector's
// branches: an exact challenged resource_metadata URL is authoritative (no
// fallback, and a 404 on it is a hard error via NotFoundIsHardError); a
// challenge carrying only a bare resource auth-param is NOT treated as a
// metadata URL and falls back to the RFC 9728 §3.1 path-insertion URL
// first, with the origin-root well-known URL as the only opt-in fallback
// (NotFoundIsHardError unset, so a 404 there is tolerated as usual).
func TestProtectedResourceMetadataCandidates(t *testing.T) {
	t.Parallel()

	serverURL, err := url.Parse("https://mcp.example.test/v1/mcp/authv2")
	require.NoError(t, err)
	const authOrigin = "https://mcp.example.test"

	tests := []struct {
		name                    string
		wwwAuth                 string
		wantPrimary             string
		wantFallbackURL         []string
		wantNotFoundIsHardError bool
	}{
		{
			name:                    "exact challenged resource_metadata is authoritative with no fallback and hard-errors on 404",
			wwwAuth:                 `Bearer resource_metadata="https://mcp.example.test/custom-prm"`,
			wantPrimary:             "https://mcp.example.test/custom-prm",
			wantNotFoundIsHardError: true,
		},
		{
			name:            "no resource_metadata falls back to path-insertion then origin-root",
			wwwAuth:         `Bearer error="insufficient_scope"`,
			wantPrimary:     "https://mcp.example.test/.well-known/oauth-protected-resource/v1/mcp/authv2",
			wantFallbackURL: []string{"https://mcp.example.test/.well-known/oauth-protected-resource"},
		},
		{
			name:            "a bare resource auth-param is not a metadata URL: falls back to path-insertion then origin-root",
			wwwAuth:         `Bearer resource="https://mcp.example.test/v1/mcp/authv2"`,
			wantPrimary:     "https://mcp.example.test/.well-known/oauth-protected-resource/v1/mcp/authv2",
			wantFallbackURL: []string{"https://mcp.example.test/.well-known/oauth-protected-resource"},
		},
		{
			name:            "empty challenge (no 401) also falls back to path-insertion then origin-root",
			wwwAuth:         "",
			wantPrimary:     "https://mcp.example.test/.well-known/oauth-protected-resource/v1/mcp/authv2",
			wantFallbackURL: []string{"https://mcp.example.test/.well-known/oauth-protected-resource"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			primary, opts := protectedResourceMetadataCandidates(serverURL, authOrigin, tt.wwwAuth)
			assert.Equal(t, tt.wantPrimary, primary)
			assert.Equal(t, tt.wantFallbackURL, opts.FallbackCandidateURLs)
			assert.Equal(t, tt.wantNotFoundIsHardError, opts.NotFoundIsHardError)
		})
	}
}

// TestProtectedResourceMetadataPathInsertionURL covers RFC 9728 §3.1 path
// insertion for both a path-bearing and a root resource URL.
func TestProtectedResourceMetadataPathInsertionURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "path is inserted between origin and path",
			in:   "https://mcp.atlassian.com/v1/mcp/authv2",
			want: "https://mcp.atlassian.com/.well-known/oauth-protected-resource/v1/mcp/authv2",
		},
		{
			name: "trailing slash is trimmed before insertion",
			in:   "https://mcp.example.test/mcp/",
			want: "https://mcp.example.test/.well-known/oauth-protected-resource/mcp",
		},
		{
			name: "no path collapses to the plain well-known URL",
			in:   "https://mcp.example.test",
			want: "https://mcp.example.test/.well-known/oauth-protected-resource",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := url.Parse(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, protectedResourceMetadataPathInsertionURL(parsed))
		})
	}
}

// TestResolveStandaloneClientCredentials characterizes the standalone CLI's
// client-credential selector against the full decision table: explicit
// config wins with its scopes carried exactly (nil when omitted); absent
// explicit credentials require DCR, using configured > challenge > PRM >
// omit scope selection; and a missing or failing DCR is a hard error with
// no interactive prompt (the selector never elicits).
func TestResolveStandaloneClientCredentials(t *testing.T) {
	t.Parallel()

	t.Run("explicit client ID skips DCR and carries configured scopes exactly", func(t *testing.T) {
		t.Parallel()

		var registerCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			registerCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		oauthConfig := &latest.RemoteOAuthConfig{
			ClientID:     "explicit-client",
			ClientSecret: "explicit-secret",
			Scopes:       []string{"explicit-scope"},
		}
		authMeta := &AuthorizationServerMetadata{RegistrationEndpoint: srv.URL}

		clientID, clientSecret, scopes, err := resolveStandaloneClientCredentials(
			t.Context(), srv.Client(), oauthConfig, authMeta, "http://127.0.0.1/callback",
			[]string{"challenge-scope"}, []string{"prm-scope"},
		)
		require.NoError(t, err)
		assert.Equal(t, "explicit-client", clientID)
		assert.Equal(t, "explicit-secret", clientSecret)
		assert.Equal(t, []string{"explicit-scope"}, scopes)
		assert.Equal(t, int32(0), registerCalls.Load(), "explicit credentials must never trigger DCR")
	})

	t.Run("explicit client ID with no configured scopes omits the scope parameter", func(t *testing.T) {
		t.Parallel()

		oauthConfig := &latest.RemoteOAuthConfig{ClientID: "explicit-client"}
		authMeta := &AuthorizationServerMetadata{RegistrationEndpoint: "http://unused.invalid"}

		clientID, clientSecret, scopes, err := resolveStandaloneClientCredentials(
			t.Context(), http.DefaultClient, oauthConfig, authMeta, "http://127.0.0.1/callback",
			[]string{"challenge-scope"}, []string{"prm-scope"},
		)
		require.NoError(t, err)
		assert.Equal(t, "explicit-client", clientID)
		assert.Empty(t, clientSecret)
		assert.Nil(t, scopes, "no configured scopes must not be backfilled from challenge/PRM when credentials are explicit")
	})

	t.Run("no explicit client ID and no registration endpoint hard-errors without prompting", func(t *testing.T) {
		t.Parallel()

		authMeta := &AuthorizationServerMetadata{}

		_, _, _, err := resolveStandaloneClientCredentials(
			t.Context(), http.DefaultClient, nil, authMeta, "http://127.0.0.1/callback", nil, nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not support dynamic client registration")
	})

	t.Run("DCR failure hard-errors without prompting", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		authMeta := &AuthorizationServerMetadata{RegistrationEndpoint: srv.URL}

		_, _, _, err := resolveStandaloneClientCredentials(
			t.Context(), srv.Client(), nil, authMeta, "http://127.0.0.1/callback", nil, nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dynamic client registration failed")
	})

	t.Run("successful DCR uses configured over challenge over PRM scopes and reuses them", func(t *testing.T) {
		t.Parallel()

		var gotScope string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Scope string `json:"scope"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotScope = body.Scope
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"client_id":"registered-id","client_secret":"registered-secret"}`))
		}))
		defer srv.Close()

		authMeta := &AuthorizationServerMetadata{RegistrationEndpoint: srv.URL}
		oauthConfig := &latest.RemoteOAuthConfig{Scopes: []string{"configured-scope"}}

		clientID, clientSecret, scopes, err := resolveStandaloneClientCredentials(
			t.Context(), srv.Client(), oauthConfig, authMeta, "http://127.0.0.1/callback",
			[]string{"challenge-scope"}, []string{"prm-scope"},
		)
		require.NoError(t, err)
		assert.Equal(t, "registered-id", clientID)
		assert.Equal(t, "registered-secret", clientSecret)
		assert.Equal(t, []string{"configured-scope"}, scopes)
		assert.Equal(t, "configured-scope", gotScope, "the DCR request must carry the exact selected scope")
	})

	t.Run("successful DCR falls back to challenge then PRM scopes when unconfigured", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"client_id":"registered-id"}`))
		}))
		defer srv.Close()
		authMeta := &AuthorizationServerMetadata{RegistrationEndpoint: srv.URL}

		_, _, scopes, err := resolveStandaloneClientCredentials(
			t.Context(), srv.Client(), nil, authMeta, "http://127.0.0.1/callback",
			nil, []string{"prm-scope"},
		)
		require.NoError(t, err)
		assert.Equal(t, []string{"prm-scope"}, scopes, "PRM scopes_supported must be used when no configured or challenge scope is available")
	})
}

// TestCallbackConfigAccessors pins the nil-safe accessors PerformOAuthLogin
// uses to read RemoteOAuthConfig.CallbackPort/CallbackRedirectURL — the same
// accessors the runtime's managed OAuth flow uses — so a nil Remote.OAuth
// (the common case) behaves identically to an explicit zero-value config.
func TestCallbackConfigAccessors(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, callbackPortFrom(nil))
	assert.Empty(t, callbackRedirectURLFrom(nil))

	assert.Equal(t, 0, callbackPortFrom(&latest.RemoteOAuthConfig{}))
	assert.Empty(t, callbackRedirectURLFrom(&latest.RemoteOAuthConfig{}))

	configured := &latest.RemoteOAuthConfig{CallbackPort: 8765, CallbackRedirectURL: "https://proxy.example.test/cb"}
	assert.Equal(t, 8765, callbackPortFrom(configured))
	assert.Equal(t, "https://proxy.example.test/cb", callbackRedirectURLFrom(configured))
}

// reserveFreeLoopbackPort returns a TCP port free on 127.0.0.1 at the time
// of the call, for tests that need to pin RemoteOAuthConfig.CallbackPort to
// a specific, real, bindable port. The listener is closed immediately so
// the test's own call to NewCallbackServerOnPort can bind it.
func reserveFreeLoopbackPort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// TestPerformOAuthLogin_DefaultCallback_UsesLoopbackRedirect_EndToEnd drives
// PerformOAuthLogin end to end with no OAuth config at all (the common
// case): the redirect URI used for /authorize and the token exchange must
// be the local callback server's own loopback address, unchanged — the
// behavior preserved from before this package honored
// CallbackPort/CallbackRedirectURL.
func TestPerformOAuthLogin_DefaultCallback_UsesLoopbackRedirect_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	var tokenRedirectURI string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-id"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenRedirectURI = r.FormValue("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-default-callback")

	require.NoError(t, <-errCh)

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	authorizeRedirectURI := parsedAuthURL.Query().Get("redirect_uri")
	assert.Regexp(t, `^http://127\.0\.0\.1:\d+/callback$`, authorizeRedirectURI,
		"with no OAuth config, the redirect URI must be the local loopback callback server's own address")
	assert.Equal(t, authorizeRedirectURI, tokenRedirectURI,
		"the token exchange must reuse the exact same redirect_uri as /authorize")
}

// TestPerformOAuthLogin_CallbackPort_UsesConfiguredPort_EndToEnd drives
// PerformOAuthLogin end to end with RemoteOAuthConfig.CallbackPort set: the
// local callback server must bind that exact port, and the redirect URI
// used for /authorize and the token exchange must carry it — the same
// contract NewCallbackServerOnPort gives the runtime's managed OAuth flow.
func TestPerformOAuthLogin_CallbackPort_UsesConfiguredPort_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)
	port := reserveFreeLoopbackPort(t)

	const mcpPath = "/mcp"
	var tokenRedirectURI string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-id"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenRedirectURI = r.FormValue("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{
		URL:   srv.URL + mcpPath,
		OAuth: &latest.RemoteOAuthConfig{CallbackPort: port},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-configured-port")

	require.NoError(t, <-errCh)

	wantRedirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, wantRedirectURI, parsedAuthURL.Query().Get("redirect_uri"),
		"the /authorize redirect_uri must carry the configured CallbackPort")
	assert.Equal(t, wantRedirectURI, tokenRedirectURI,
		"the token exchange must reuse the exact same configured-port redirect_uri as /authorize")
}

// TestPerformOAuthLogin_CallbackPortAlreadyInUse_HardErrors proves that a
// configured CallbackPort docker-agent cannot bind is a hard error (no
// fallback to a random port, no browser, no token stored) — the same
// failure mode NewCallbackServerOnPort gives the runtime's managed flow.
func TestPerformOAuthLogin_CallbackPortAlreadyInUse_HardErrors(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	const mcpPath = "/mcp"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{
		URL:   srv.URL + mcpPath,
		OAuth: &latest.RemoteOAuthConfig{CallbackPort: port},
	}

	err = PerformOAuthLogin(t.Context(), remote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create callback server")

	_, err = os.ReadFile(captureFile)
	require.Error(t, err, "no browser must be opened when the configured callback port cannot be bound")
	_, err = store.GetToken(remote.URL)
	require.Error(t, err, "no token must be stored when the configured callback port cannot be bound")
}

// TestPerformOAuthLogin_CallbackRedirectURL_SubstitutedPlaceholderReachesEverySurface_EndToEnd
// drives PerformOAuthLogin end to end with RemoteOAuthConfig.CallbackRedirectURL
// set to an override carrying the ${callbackPort} placeholder: the resolved
// redirect URI (override verbatim, with the placeholder substituted for the
// local callback server's actual port) must be what reaches /authorize and
// the token exchange, proving the override path — not the default loopback
// address — was used.
func TestPerformOAuthLogin_CallbackRedirectURL_SubstitutedPlaceholderReachesEverySurface_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	var registerRedirectURIs []string
	var tokenRedirectURI string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		registerRedirectURIs = body.RedirectURIs
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-id"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenRedirectURI = r.FormValue("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{
		URL: srv.URL + mcpPath,
		OAuth: &latest.RemoteOAuthConfig{
			CallbackRedirectURL: "http://127.0.0.1:${callbackPort}/callback?viaProxy=1",
		},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-redirect-override")

	require.NoError(t, <-errCh)

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	authorizeRedirectURI := parsedAuthURL.Query().Get("redirect_uri")
	assert.Regexp(t, `^http://127\.0\.0\.1:\d+/callback\?viaProxy=1$`, authorizeRedirectURI,
		"the /authorize redirect_uri must be the override with ${callbackPort} substituted, not the default loopback address")
	require.Len(t, registerRedirectURIs, 1)
	assert.Equal(t, authorizeRedirectURI, registerRedirectURIs[0],
		"the DCR request's redirect_uris must carry the exact same resolved redirect URI as /authorize")
	assert.Equal(t, authorizeRedirectURI, tokenRedirectURI,
		"the token exchange must reuse the exact same resolved redirect URI as /authorize")
}

// TestPerformOAuthLogin_CallbackPortWithExplicitClient_EndToEnd proves
// CallbackPort is honored on the explicit-client-credentials path too (no
// DCR involved): the configured port must still reach /authorize and the
// token exchange.
func TestPerformOAuthLogin_CallbackPortWithExplicitClient_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)
	port := reserveFreeLoopbackPort(t)

	const mcpPath = "/mcp"
	var registerCalls int
	var tokenRedirectURI string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		registerCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenRedirectURI = r.FormValue("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{
		URL: srv.URL + mcpPath,
		OAuth: &latest.RemoteOAuthConfig{
			ClientID:     "explicit-client",
			ClientSecret: "explicit-secret",
			CallbackPort: port,
		},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-explicit-client-port")

	require.NoError(t, <-errCh)

	assert.Equal(t, 0, registerCalls, "explicit client credentials must never trigger DCR")

	wantRedirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, wantRedirectURI, parsedAuthURL.Query().Get("redirect_uri"))
	assert.Equal(t, wantRedirectURI, tokenRedirectURI)
}

// TestPerformOAuthLogin_ChallengeResourceMetadataAuthoritative_EndToEnd drives
// PerformOAuthLogin end to end when the unauthenticated probe's challenge
// names an exact resource_metadata URL: that URL is used verbatim (never
// recomputed via path-insertion/origin-root), and Remote.URL — including a
// query string, to prove it is never rewritten — is used verbatim as the
// probe target and, byte-for-byte, as the token-store key.
func TestPerformOAuthLogin_ChallengeResourceMetadataAuthoritative_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/v1/mcp/authv2"
	const mcpQuery = "tenant=42"

	var probeRequestURI string
	var pathInsertionCalls, originRootCalls, registerCalls atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, r *http.Request) {
		probeRequestURI = r.URL.RequestURI()
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/custom-prm"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/custom-prm", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             srv.URL + mcpPath,
			AuthorizationServers: []string{srv.URL},
			ScopesSupported:      []string{"prm-scope"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		pathInsertionCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		originRootCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		registerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-id","client_secret":"registered-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath + "?" + mcpQuery}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-challenge-exact-prm")

	require.NoError(t, <-errCh)

	assert.Equal(t, mcpPath+"?"+mcpQuery, probeRequestURI, "the probe must hit Remote.URL verbatim, including its query string")
	assert.Equal(t, int32(0), pathInsertionCalls.Load(), "the exact challenged resource_metadata URL must not fall back to path-insertion")
	assert.Equal(t, int32(0), originRootCalls.Load(), "the exact challenged resource_metadata URL must not fall back to origin-root")
	assert.Equal(t, int32(1), registerCalls.Load())

	tok, err := store.GetToken(remote.URL)
	require.NoError(t, err, "the stored token must be retrievable by Remote.URL verbatim, byte-for-byte")
	assert.Equal(t, "exchanged-at", tok.AccessToken)
	assert.Equal(t, []string{"prm-scope"}, tok.RequestedScopes, "no configured/challenge scope: PRM scopes_supported must be selected and carried to the token")
}

// TestPerformOAuthLogin_PathInsertionThenOriginRootFallback_EndToEnd drives
// PerformOAuthLogin end to end when the unauthenticated probe's challenge
// carries no resource_metadata: the RFC 9728 §3.1 path-insertion URL must be
// tried first, and the origin-root well-known URL only after that 404s.
func TestPerformOAuthLogin_PathInsertionThenOriginRootFallback_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/v1/mcp/authv2"

	var orderMu sync.Mutex
	var order []string
	recordOrder := func(step string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, step)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		recordOrder("path-insertion")
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		recordOrder("origin-root")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-id"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-path-insertion-fallback")

	require.NoError(t, <-errCh)
	orderMu.Lock()
	gotOrder := order
	orderMu.Unlock()
	assert.Equal(t, []string{"path-insertion", "origin-root"}, gotOrder,
		"path-insertion must be tried before the origin-root fallback, and only after a 404")

	_, err := store.GetToken(remote.URL)
	require.NoError(t, err)
}

// TestPerformOAuthLogin_ProtectedResourceMetadataHardError_StopsFlow proves
// that a hard protected-resource-metadata error (non-404/non-200, or a
// decode failure) stops the flow immediately in both discovery branches:
// no authorization-server metadata, DCR, browser, or token request follows.
func TestPerformOAuthLogin_ProtectedResourceMetadataHardError_StopsFlow(t *testing.T) {
	tests := []struct {
		name    string
		wwwAuth string
	}{
		{name: "exact challenged resource_metadata URL returns a hard error", wwwAuth: `Bearer resource_metadata="__PRM__"`},
		{name: "path-insertion candidate returns a hard error (no origin-root fallback)", wwwAuth: `Bearer error="insufficient_scope"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDefaultStore(t)
			store := NewInMemoryTokenStore()
			SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

			captureFile := setupFakeBrowserOpener(t)

			const mcpPath = "/v1/mcp/authv2"
			var authServerCalls, originRootCalls atomic.Int32
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			defer srv.Close()

			wwwAuth := strings.ReplaceAll(tt.wwwAuth, "__PRM__", srv.URL+"/prm-error")
			mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", wwwAuth)
				w.WriteHeader(http.StatusUnauthorized)
			})
			// The exact-challenge branch's PRM candidate is /prm-error; the
			// no-challenge branch's primary candidate is the path-insertion URL.
			// Both are wired to the same hard-error handler; only the one the
			// active sub-test's discovery branch actually reaches gets hit.
			hardErrorHandler := func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			}
			mux.HandleFunc("/prm-error", hardErrorHandler)
			mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, hardErrorHandler)
			mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
				originRootCalls.Add(1)
				w.WriteHeader(http.StatusNotFound)
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
				authServerCalls.Add(1)
				w.WriteHeader(http.StatusNotFound)
			})

			defer setOAuthLoginHTTPClientForTesting(srv.Client())()

			remote := latest.Remote{URL: srv.URL + mcpPath}
			err := PerformOAuthLogin(t.Context(), remote)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to fetch protected resource metadata")

			assert.Equal(t, int32(0), authServerCalls.Load(), "a hard PRM error must stop before authorization-server discovery")
			assert.Equal(t, int32(0), originRootCalls.Load(), "a hard error on an earlier candidate must not try a later one")
			_, err = os.ReadFile(captureFile)
			require.Error(t, err, "no browser must be opened after a hard PRM error")
			_, err = store.GetToken(remote.URL)
			require.Error(t, err, "no token must be stored after a hard PRM error")
		})
	}
}

// TestPerformOAuthLogin_ExplicitClientCredentials_NoDCR proves that
// configuring Remote.OAuth.ClientID skips Dynamic Client Registration
// entirely and carries the configured scopes (nil when omitted) to
// /authorize and the stored token.
func TestPerformOAuthLogin_ExplicitClientCredentials_NoDCR(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	var registerCalls atomic.Int32
	var gotAuthorizeScope string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		registerCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{
		URL:   srv.URL + mcpPath,
		OAuth: &latest.RemoteOAuthConfig{ClientID: "pre-registered-client", ClientSecret: "pre-registered-secret"},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	gotAuthorizeScope = parsedAuthURL.Query().Get("scope")
	assert.Equal(t, "pre-registered-client", parsedAuthURL.Query().Get("client_id"))

	deliverFakeCallback(t, authURL, "code-explicit-client-credentials")
	require.NoError(t, <-errCh)

	assert.Equal(t, int32(0), registerCalls.Load(), "explicit credentials must never trigger DCR")
	assert.Empty(t, gotAuthorizeScope, "no configured scopes: the scope parameter must be omitted, not backfilled from discovery")

	tok, err := store.GetToken(remote.URL)
	require.NoError(t, err)
	assert.Equal(t, "pre-registered-client", tok.ClientID)
	assert.Nil(t, tok.RequestedScopes)
}

// TestPerformOAuthLogin_NoDCR_NoExplicitCredentials_HardErrorsWithoutPrompting
// proves that when the authorization server does not support Dynamic Client
// Registration and no explicit clientId is configured, PerformOAuthLogin
// hard-errors (it never prompts, since it is a non-interactive command) and
// never opens the browser or stores a token.
func TestPerformOAuthLogin_NoDCR_NoExplicitCredentials_HardErrorsWithoutPrompting(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		// No registration_endpoint: DCR is unsupported.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
		})
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath}
	err := PerformOAuthLogin(t.Context(), remote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support dynamic client registration")

	_, err = os.ReadFile(captureFile)
	require.Error(t, err, "no browser must be opened when DCR is unavailable and no explicit credentials are configured")
	_, err = store.GetToken(remote.URL)
	require.Error(t, err, "no token must be stored when DCR is unavailable and no explicit credentials are configured")
}

// TestPerformOAuthLogin_DCRFails_HardErrorsWithoutPrompting proves that a
// failing Dynamic Client Registration call is a hard error, not a fallback
// to an interactive prompt: PerformOAuthLogin is non-interactive.
func TestPerformOAuthLogin_DCRFails_HardErrorsWithoutPrompting(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("registration unavailable"))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath}
	err := PerformOAuthLogin(t.Context(), remote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dynamic client registration failed")

	_, err = os.ReadFile(captureFile)
	require.Error(t, err, "no browser must be opened after a DCR failure")
	_, err = store.GetToken(remote.URL)
	require.Error(t, err, "no token must be stored after a DCR failure")
}

// TestPerformOAuthLogin_ChallengedResourceMetadata404_HardStopsFlow proves
// that a 404 on the exact challenged resource_metadata URL is a hard error
// (via fetchProtectedResourceMetadata's NotFoundIsHardError opt-in): it must
// not silently fall through to a guessed authorization server. No
// path-insertion/origin-root fallback, authorization-server metadata, DCR,
// browser, or token request follows, and the error string carries the
// underlying helper message exactly once, plus safe (query-free) URL
// context.
func TestPerformOAuthLogin_ChallengedResourceMetadata404_HardStopsFlow(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/v1/mcp/authv2"
	var pathInsertionCalls, originRootCalls, authServerCalls atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/custom-prm"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/custom-prm", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		pathInsertionCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		originRootCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		authServerCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath + "?secret=shhh"}
	err := PerformOAuthLogin(t.Context(), remote)
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), "failed to fetch protected resource metadata"),
		"the underlying helper message must appear exactly once, not doubled by the CLI wrap")
	assert.NotContains(t, err.Error(), "secret=shhh", "the error must never leak the query string")

	assert.Equal(t, int32(0), pathInsertionCalls.Load(), "a 404 on the exact challenged URL must not fall back to path-insertion")
	assert.Equal(t, int32(0), originRootCalls.Load(), "a 404 on the exact challenged URL must not fall back to origin-root")
	assert.Equal(t, int32(0), authServerCalls.Load(), "a hard PRM error must stop before authorization-server discovery")

	_, err = os.ReadFile(captureFile)
	require.Error(t, err, "no browser must be opened after a challenged-404 hard stop")
	_, err = store.GetToken(remote.URL)
	require.Error(t, err, "no token must be stored after a challenged-404 hard stop")
}

// TestPerformOAuthLogin_ResourceOnlyChallenge_FallsBackToPathInsertion_EndToEnd
// proves that a challenge carrying only a bare resource auth-param (naming
// the MCP endpoint itself, with no resource_metadata) is never GET/decoded
// as protected-resource metadata: discovery instead proceeds via the RFC
// 9728 §3.1 path-insertion candidate, exactly like an unchallenged probe.
func TestPerformOAuthLogin_ResourceOnlyChallenge_FallsBackToPathInsertion_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/v1/mcp/authv2"
	var mcpEndpointHitsAfterProbe atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		mcpEndpointHitsAfterProbe.Add(1)
		w.Header().Set("WWW-Authenticate", `Bearer resource="`+srv.URL+mcpPath+`"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{AuthorizationServers: []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-id"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-resource-only-challenge")

	require.NoError(t, <-errCh)
	assert.Equal(t, int32(1), mcpEndpointHitsAfterProbe.Load(),
		"the MCP endpoint must be fetched exactly once (the probe itself), never again as protected-resource metadata")

	_, err := store.GetToken(remote.URL)
	require.NoError(t, err)
}

// TestPerformOAuthLogin_DCRSuccess_EndToEndScopeEquality is the standalone
// counterpart of TestHandleManagedOAuthFlow_DCRSuccess_EndToEndScopeEquality:
// it drives PerformOAuthLogin all the way to a stored token and proves the
// exact same selected scope set reaches every place that must agree on it —
// the DCR registration request body, the /authorize URL's scope parameter,
// and the stored token's RequestedScopes.
func TestPerformOAuthLogin_DCRSuccess_EndToEndScopeEquality(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	var registerScope string
	var haveRegisterScope bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		// No challenge scope: the config's scope must still win over PRM.
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource"+mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		registerScope, haveRegisterScope = body.Scope, true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id","client_secret":"registered-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{
		URL:   srv.URL + mcpPath,
		OAuth: &latest.RemoteOAuthConfig{Scopes: []string{"configured-scope"}},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-dcr-scope-equality")

	require.NoError(t, <-errCh)

	require.True(t, haveRegisterScope, "DCR request must carry a scope field")
	assert.Equal(t, "configured-scope", registerScope, "DCR registration request must carry the selected scope")

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "configured-scope", parsedAuthURL.Query().Get("scope"),
		"the /authorize URL must carry the exact same scope sent to DCR")

	tok, err := store.GetToken(remote.URL)
	require.NoError(t, err)
	assert.Equal(t, []string{"configured-scope"}, tok.RequestedScopes,
		"the stored token's RequestedScopes must match the scope sent to DCR and /authorize")
}

// TestPerformOAuthLogin_ChallengeScopePrecedenceOverDistinctPRMScopes_EndToEnd
// drives PerformOAuthLogin end to end with a challenge that carries a scope
// AND protected-resource metadata whose scopes_supported is a distinct,
// non-empty set. It proves the challenge scope wins on all three surfaces
// (DCR registration body, /authorize scope parameter, stored
// RequestedScopes) and that no PRM-only scope leaks into any of them.
func TestPerformOAuthLogin_ChallengeScopePrecedenceOverDistinctPRMScopes_EndToEnd(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/mcp"
	var registerScope string
	var haveRegisterScope bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer scope="challenge-scope" resource_metadata="`+srv.URL+"/custom-prm"+`"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/custom-prm", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			AuthorizationServers: []string{srv.URL},
			ScopesSupported:      []string{"prm-only-scope"},
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		registerScope, haveRegisterScope = body.Scope, true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id","client_secret":"registered-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-challenge-over-prm")

	require.NoError(t, <-errCh)

	require.True(t, haveRegisterScope, "DCR request must carry a scope field")
	assert.Equal(t, "challenge-scope", registerScope, "the challenge scope, not the distinct PRM scope, must reach DCR")

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "challenge-scope", parsedAuthURL.Query().Get("scope"),
		"the /authorize URL must carry the challenge scope, not the distinct PRM scope")

	tok, err := store.GetToken(remote.URL)
	require.NoError(t, err)
	assert.Equal(t, []string{"challenge-scope"}, tok.RequestedScopes,
		"the stored token's RequestedScopes must be the challenge scope, with no PRM-only scope leaking in")
}

// TestPerformOAuthLogin_PRMResourceDiffersFromRemoteURL_ResourceIsRemoteURLVerbatim
// is the regression pin for R2: protected-resource metadata reports a
// resource deliberately DIFFERENT from the configured Remote.URL (which
// itself carries a query string), and the test proves Remote.URL — byte for
// byte, including the query — is what actually reaches every surface: the
// /authorize resource parameter, the token-exchange resource form value, and
// the token-store key. It also proves the PRM-reported resource leaks into
// none of the three; reintroducing `cmp.Or(resourceMetadata.Resource,
// serverURL)` would fail this test even though every other end-to-end
// fixture (whose PRM resource equals the MCP URL) would still pass.
func TestPerformOAuthLogin_PRMResourceDiffersFromRemoteURL_ResourceIsRemoteURLVerbatim(t *testing.T) {
	resetDefaultStore(t)
	store := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return store })

	captureFile := setupFakeBrowserOpener(t)

	const mcpPath = "/v1/mcp/authv2"
	const mcpQuery = "tenant=42"
	const prmResource = "https://prm-reports-a-different-resource.example.test/mcp"

	var tokenResource string
	var haveTokenResource bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/custom-prm"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/custom-prm", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
			Resource:             prmResource,
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
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"registered-client-id"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		tokenResource, haveTokenResource = r.FormValue("resource"), true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged-at","token_type":"Bearer","expires_in":3600}`))
	})

	defer setOAuthLoginHTTPClientForTesting(srv.Client())()

	remote := latest.Remote{URL: srv.URL + mcpPath + "?" + mcpQuery}

	errCh := make(chan error, 1)
	go func() { errCh <- PerformOAuthLogin(t.Context(), remote) }()

	authURL := requireCapturedAuthorizeURL(t, captureFile)
	deliverFakeCallback(t, authURL, "code-prm-resource-differs")

	require.NoError(t, <-errCh)

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	gotAuthorizeResource := parsedAuthURL.Query().Get("resource")
	assert.Equal(t, remote.URL, gotAuthorizeResource,
		"the /authorize resource parameter must be Remote.URL verbatim, including its query string")

	require.True(t, haveTokenResource, "token exchange request must carry a resource field")
	assert.Equal(t, remote.URL, tokenResource,
		"the token-exchange resource form value must be Remote.URL verbatim, including its query string")

	tok, err := store.GetToken(remote.URL)
	require.NoError(t, err, "the stored token must be retrievable by Remote.URL verbatim, byte-for-byte, including its query string")
	assert.Equal(t, "exchanged-at", tok.AccessToken)

	_, err = store.GetToken(prmResource)
	require.Error(t, err, "the PRM-reported resource must never become a token-store key")

	assert.NotEqual(t, prmResource, gotAuthorizeResource,
		"the PRM-reported resource must not leak into the /authorize resource parameter")
	assert.NotEqual(t, prmResource, tokenResource,
		"the PRM-reported resource must not leak into the token-exchange resource form value")
}
