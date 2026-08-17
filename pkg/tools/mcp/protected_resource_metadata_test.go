package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFetchProtectedResourceMetadata characterizes fetchProtectedResourceMetadata,
// the helper shared by the managed and driven-unmanaged OAuth flows, against
// the RFC 9728 discovery outcome/action table: exact challenged metadata is
// authoritative, a 404 defaults AuthorizationServers to the supplied origin
// without touching decoded Resource/ScopesSupported, and any decode failure
// or non-404/non-200 response is a hard error with exactly one request made.
func TestFetchProtectedResourceMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantResult protectedResourceMetadata
	}{
		{
			name:   "exact challenged metadata with 200 is decoded and returned unmodified",
			status: http.StatusOK,
			body:   `{"resource":"https://res.example.test","authorization_servers":["https://as.example.test"],"scopes_supported":["read","write"]}`,
			wantResult: protectedResourceMetadata{
				Resource:             "https://res.example.test",
				AuthorizationServers: []string{"https://as.example.test"},
				ScopesSupported:      []string{"read", "write"},
			},
		},
		{
			name:   "404 is not an error and defaults AuthorizationServers to the supplied origin",
			status: http.StatusNotFound,
			wantResult: protectedResourceMetadata{
				AuthorizationServers: []string{"https://auth.example.test"},
			},
		},
		{
			name:    "non-404/non-200 is a hard error",
			status:  http.StatusInternalServerError,
			body:    "boom",
			wantErr: "failed to fetch protected resource metadata",
		},
		{
			name:    "a non-200 2xx status (e.g. 204) is a hard error, not treated as success",
			status:  http.StatusNoContent,
			wantErr: "failed to fetch protected resource metadata",
		},
		{
			name:    "200 with an undecodable body is a hard error",
			status:  http.StatusOK,
			body:    "{not valid json",
			wantErr: "invalid character",
		},
		{
			name:   "200 with empty authorization_servers defaults to the supplied origin while preserving resource and scopes",
			status: http.StatusOK,
			body:   `{"resource":"https://res.example.test","scopes_supported":["read"]}`,
			wantResult: protectedResourceMetadata{
				Resource:             "https://res.example.test",
				AuthorizationServers: []string{"https://auth.example.test"},
				ScopesSupported:      []string{"read"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requestCount atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
				requestCount.Add(1)
				if tt.body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tt.status)
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			resourceURL := srv.URL + "/.well-known/oauth-protected-resource"
			result, err := fetchProtectedResourceMetadata(t.Context(), srv.Client(), resourceURL, "https://auth.example.test", protectedResourceMetadataOptions{})

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Equal(t, int32(1), requestCount.Load(), "a hard error must not retry or try another candidate")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantResult, result)
			assert.Equal(t, int32(1), requestCount.Load(), "fallback is disabled by default: exactly one request for the exact candidate")
		})
	}
}

// TestFetchProtectedResourceMetadata_RuntimeFallbackDisabled proves the
// zero-value protectedResourceMetadataOptions (every runtime call site's
// default) never tries a fallback candidate: even when the primary
// candidate 404s, the result matches the current runtime outcome (default
// AuthorizationServers to the supplied origin) with no second request.
func TestFetchProtectedResourceMetadata_RuntimeFallbackDisabled(t *testing.T) {
	t.Parallel()

	var primaryCalls, fallbackCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/mcp/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// opts is the zero value: FallbackCandidateURLs is nil, which is what
	// every runtime caller (managed and driven-unmanaged flows) passes.
	result, err := fetchProtectedResourceMetadata(
		t.Context(), srv.Client(),
		srv.URL+"/.well-known/oauth-protected-resource",
		srv.URL,
		protectedResourceMetadataOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{srv.URL}, result.AuthorizationServers)
	assert.Equal(t, int32(1), primaryCalls.Load(), "only the primary candidate is tried when fallback is disabled")
	assert.Equal(t, int32(0), fallbackCalls.Load(), "no path-aware fallback candidate must be tried when disabled (the default)")
}

// TestFetchProtectedResourceMetadata_FallbackCandidatesTriedInOrderAfter404
// exercises the opt-in path: when FallbackCandidateURLs is non-empty, they
// are tried in order only after the primary candidate 404s, and the walk
// stops at the first candidate that isn't a 404. Every candidate request
// uses GET. Runtime callers never set this; it exists for the standalone
// CLI discovery flow to build on.
func TestFetchProtectedResourceMetadata_FallbackCandidatesTriedInOrderAfter404(t *testing.T) {
	t.Parallel()

	var order []string
	var methods []string
	mux := http.NewServeMux()
	mux.HandleFunc("/primary", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "primary")
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/fallback1", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "fallback1")
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/fallback2", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "fallback2")
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource":"` + r.Host + `","authorization_servers":["https://as.example.test"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := fetchProtectedResourceMetadata(
		t.Context(), srv.Client(),
		srv.URL+"/primary",
		"https://auth.example.test",
		protectedResourceMetadataOptions{
			FallbackCandidateURLs: []string{srv.URL + "/fallback1", srv.URL + "/fallback2"},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"primary", "fallback1", "fallback2"}, order,
		"candidates must be tried strictly in order, exactly once each, stopping at the first non-404")
	assert.Equal(t, []string{http.MethodGet, http.MethodGet, http.MethodGet}, methods,
		"every candidate request, including fallbacks, must use GET")
	assert.Equal(t, []string{"https://as.example.test"}, result.AuthorizationServers)
}

// TestFetchProtectedResourceMetadata_FallbackHardStopsAndExhaustion covers
// the remaining opt-in fallback outcomes required by the discovery
// outcome/action table: a decode failure or a non-404/non-200 status
// (including a non-200 2xx like 204, which must not be mistaken for
// success) on any candidate reached after a prior 404 is a hard error that
// tries no further candidate, and exhausting every candidate with 404s
// falls back to the supplied origin. Every request, including ones that
// never fire, is accounted for.
func TestFetchProtectedResourceMetadata_FallbackHardStopsAndExhaustion(t *testing.T) {
	t.Parallel()

	const authServer = "https://auth.example.test"

	tests := []struct {
		name        string
		statuses    []int // one entry per candidate that must be reached
		bodies      []string
		wantErr     string
		wantServers []string
	}{
		{
			name:     "404 then invalid JSON on the fallback hard-stops with no later candidate tried",
			statuses: []int{http.StatusNotFound, http.StatusOK},
			bodies:   []string{"", "{not valid json"},
			wantErr:  "invalid character",
		},
		{
			name:     "404 then non-404/non-200 on the fallback hard-stops with no later candidate tried",
			statuses: []int{http.StatusNotFound, http.StatusInternalServerError},
			bodies:   []string{"", "boom"},
			wantErr:  "failed to fetch protected resource metadata",
		},
		{
			name:     "404 then a non-200 2xx (204) on the fallback hard-stops with no later candidate tried",
			statuses: []int{http.StatusNotFound, http.StatusNoContent},
			bodies:   []string{"", ""},
			wantErr:  "failed to fetch protected resource metadata",
		},
		{
			name:        "exhausting every candidate with 404 defaults AuthorizationServers to the supplied origin",
			statuses:    []int{http.StatusNotFound, http.StatusNotFound, http.StatusNotFound},
			bodies:      []string{"", "", ""},
			wantServers: []string{authServer},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const numCandidateSlots = 3
			var callCounts [numCandidateSlots]atomic.Int32
			var methods [numCandidateSlots]atomic.Value

			mux := http.NewServeMux()
			for i := range numCandidateSlots {
				mux.HandleFunc(fmt.Sprintf("/c%d", i), func(w http.ResponseWriter, r *http.Request) {
					callCounts[i].Add(1)
					methods[i].Store(r.Method)
					if i >= len(tt.statuses) {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if tt.bodies[i] != "" {
						w.Header().Set("Content-Type", "application/json")
					}
					w.WriteHeader(tt.statuses[i])
					if tt.bodies[i] != "" {
						_, _ = w.Write([]byte(tt.bodies[i]))
					}
				})
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()

			fallbacks := make([]string, numCandidateSlots-1)
			for i := 1; i < numCandidateSlots; i++ {
				fallbacks[i-1] = fmt.Sprintf("%s/c%d", srv.URL, i)
			}

			result, err := fetchProtectedResourceMetadata(
				t.Context(), srv.Client(),
				srv.URL+"/c0",
				authServer,
				protectedResourceMetadataOptions{FallbackCandidateURLs: fallbacks},
			)

			wantReached := len(tt.statuses)
			for i := range numCandidateSlots {
				wantCalls := int32(0)
				if i < wantReached {
					wantCalls = 1
				}
				assert.Equal(t, wantCalls, callCounts[i].Load(), "candidate /c%d call count", i)
				if wantCalls == 1 {
					assert.Equal(t, http.MethodGet, methods[i].Load(), "candidate /c%d must be requested with GET", i)
				}
			}

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantServers, result.AuthorizationServers)
		})
	}
}

// TestHandleManagedOAuthFlow_ProtectedResourceMetadataHardErrorStopsFlow and
// its unmanaged counterpart below prove that a hard PRM error (non-404/
// non-200, or a decode failure) returned by fetchProtectedResourceMetadata
// stops the OAuth flow immediately: no authorization-server discovery, DCR,
// browser, elicitation, or token request follows.
func TestHandleManagedOAuthFlow_ProtectedResourceMetadataHardErrorStopsFlow(t *testing.T) {
	t.Parallel()

	var authServerCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		authServerCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	capture := &elicitCaptured{}
	transport := &oauthTransport{
		base:               newTestTransport(t),
		requestElicitation: capture.handler,
		tokenStore:         NewInMemoryTokenStore(),
		baseURL:            srv.URL,
		managed:            true,
		oauthHTTPClient:    oauthHTTPClientForAllowPrivateIPs(true),
	}

	err := transport.handleManagedOAuthFlow(t.Context(), srv.URL, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch protected resource metadata")
	assert.Equal(t, int32(0), authServerCalls.Load(), "a hard PRM error must stop before authorization-server discovery")
	assert.Nil(t, capture.req, "no elicitation must be sent after a hard PRM error")
}

func TestHandleUnmanagedOAuthFlow_ProtectedResourceMetadataHardErrorStopsFlow(t *testing.T) {
	t.Parallel()

	var authServerCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		authServerCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	capture := &elicitCaptured{}
	transport, _ := newUnmanagedTestTransport(t, srv.URL, "", capture)

	err := transport.handleUnmanagedOAuthFlow(t.Context(), srv.URL, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch protected resource metadata")
	assert.Equal(t, int32(0), authServerCalls.Load(), "a hard PRM error must stop before authorization-server discovery")
	assert.Nil(t, capture.req, "no elicitation must be sent after a hard PRM error")
}
