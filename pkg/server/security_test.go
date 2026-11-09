package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestServerBrowserMutations(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, origin, fetchSite string
		want                    int
	}{
		{name: "native client", want: http.StatusOK},
		{name: "same origin", origin: "http://localhost:8080", fetchSite: "same-origin", want: http.StatusOK},
		{name: "origin fallback", origin: "http://localhost:8080", want: http.StatusOK},
		{name: "cross site form", origin: "http://attacker.example", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "same site is not same origin", fetchSite: "same-site", want: http.StatusForbidden},
		{name: "fetch metadata only", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "legacy browser", origin: "http://attacker.example", want: http.StatusForbidden},
		{name: "different port", origin: "http://localhost:3000", want: http.StatusForbidden},
		{name: "opaque origin", origin: "null", want: http.StatusForbidden},
		{name: "malformed origin", origin: "://invalid", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := session.New(session.WithID("known-session"), session.WithSafetyPolicy(session.SafetyPolicyStrict))
			sm := newTestSessionManager(t, sess, &resumeRecordingRuntime{})
			srv := NewWithManager(sm, "")
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080/api/sessions/known-session/tools/toggle", http.NoBody)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			req.Header.Set("Sec-Fetch-Mode", "navigate")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
			if tc.want == http.StatusForbidden {
				assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())
				assert.False(t, sess.IsToolsApproved())
			} else {
				assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy())
			}
		})
	}
}

func TestServerCrossOriginRoutes(t *testing.T) {
	t.Parallel()

	srv := NewWithManager(nil, "secret")
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/sessions/known/tools/toggle"},
		{http.MethodPost, "/api/sessions/known/agent/known-agent"},
		{http.MethodPost, "/api/sessions/known/agent/known-agent/root"},
		{http.MethodPatch, "/api/sessions/known/safety-policy"},
		{http.MethodDelete, "/api/sessions/known"},
		{http.MethodPost, "/api/mcp-oauth/callback?state=known&code=attacker"},
		{http.MethodPost, "/unknown-route"},
		{http.MethodGet, "/api/agents/known-agent/root/tools/count"},
		{http.MethodHead, "/api/agents/known-agent/root/tools/count"},
		{http.MethodOptions, "/api/agents/known-agent/root/tools/count"},
		{http.MethodGet, "/api/sessions"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "http://localhost:8080"+tc.path, strings.NewReader(`{"messages":[]}`))
			req.Header.Set("Origin", "http://attacker.example")
			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
}

func TestServerJSONContentType(t *testing.T) {
	t.Parallel()

	srv := NewWithManager(nil, "")
	for _, route := range srv.e.Routes() {
		if route.Method != http.MethodPost && route.Method != http.MethodPatch {
			continue
		}
		if strings.HasSuffix(route.Path, "/tools/toggle") || route.Path == "/api/mcp-oauth/callback" {
			continue
		}
		for _, contentType := range []string{"", "text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=abc", "application/jsonp", "application/json; invalid"} {
			t.Run(route.Method+route.Path+"/"+contentType, func(t *testing.T) {
				t.Parallel()
				req := httptest.NewRequestWithContext(t.Context(), route.Method, route.Path, strings.NewReader(`{"messages":[]}`))
				req.Header.Set("Content-Type", contentType)
				// Unknown lengths must not bypass the media-type check.
				req.ContentLength = -1
				rec := httptest.NewRecorder()
				srv.e.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
			})
		}
	}
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		t.Run(contentType, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/sessions/known/messages", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "message is required")
		})
	}
}

func TestServerBearerAuthentication(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		authorization string
		want          int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Basic secret", http.StatusUnauthorized},
		{"Bearer secret", http.StatusOK},
	} {
		t.Run(tc.authorization, func(t *testing.T) {
			t.Parallel()
			sess := session.New(session.WithID("known"), session.WithSafetyPolicy(session.SafetyPolicyStrict))
			srv := NewWithManager(newTestSessionManager(t, sess, &resumeRecordingRuntime{}), "secret")
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/sessions/known/tools/toggle", http.NoBody)
			req.Header.Set("Authorization", tc.authorization)
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
			assert.Equal(t, tc.want == http.StatusOK, sess.IsToolsApproved())
		})
	}
}

func TestServerTCPHost(t *testing.T) {
	t.Parallel()

	for _, token := range []string{"", "secret"} {
		t.Run("token="+token, func(t *testing.T) {
			t.Parallel()
			ln, err := Listen(t.Context(), "127.0.0.1:0")
			require.NoError(t, err)
			srv := NewWithManager(nil, token)
			done := make(chan error, 1)
			go func() { done <- srv.Serve(t.Context(), ln) }()
			t.Cleanup(func() {
				_ = ln.Close()
				<-done
			})
			client := newTestHTTPClient(t)
			for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "rebind.attacker.example:8080", "_", "agent"} {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/api/ping", http.NoBody)
				require.NoError(t, err)
				req.Host = host
				req.Header.Set("Origin", "http://"+host)
				req.Header.Set("Sec-Fetch-Site", "same-origin")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := client.Do(req)
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				want := http.StatusOK
				if token == "" && (host == "rebind.attacker.example:8080" || host == "_" || host == "agent") {
					want = http.StatusForbidden
				}
				assert.Equal(t, want, resp.StatusCode, host)
			}
		})
	}
}

func TestServerRefusesUnauthenticatedNonLocalListener(t *testing.T) {
	t.Parallel()

	ln, err := Listen(t.Context(), "0.0.0.0:0")
	require.NoError(t, err)
	defer ln.Close()
	require.ErrorContains(t, NewWithManager(nil, "").Serve(t.Context(), ln), "require --auth-token")
}

func TestServerKnownSessionFormSequence(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("known"), session.WithSafetyPolicy(session.SafetyPolicyStrict))
	srv := NewWithManager(newTestSessionManager(t, sess, &resumeRecordingRuntime{}), "")
	for _, path := range []string{"/api/sessions/known/tools/toggle", "/api/sessions/known/agent/known-agent"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080"+path, bytes.NewBufferString(`{"messages":[{"role":"user","content":"untrusted prompt"}]}`))
		req.Header.Set("Origin", "http://attacker.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code)
	}
	assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())
	assert.Empty(t, sess.GetAllMessages())
}

// Preserve the synthetic Host used by native local-socket clients.
func TestServerLocalSocketHost(t *testing.T) {
	t.Parallel()

	ln, err := Listen(t.Context(), "unix://"+shortTempDir(t)+"/api.sock")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- NewWithManager(nil, "").Serve(t.Context(), ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", ln.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	for _, host := range []string{"_", "agent"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/api/ping", http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

func TestServerInheritedListenerAuth(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			original, err := Listen(t.Context(), addr)
			require.NoError(t, err)
			defer original.Close()
			wantAddr := original.Addr()
			file, err := original.(*net.TCPListener).File()
			require.NoError(t, err)
			require.NoError(t, original.Close())
			pinFDOwnership(file)
			ln, err := Listen(t.Context(), fmt.Sprintf("fd://%d", file.Fd()))
			require.NoError(t, err)
			defer ln.Close()
			require.Equal(t, wantAddr, ln.Addr())

			srv := NewWithManager(nil, "")
			if addr == "0.0.0.0:0" {
				require.ErrorContains(t, srv.Serve(t.Context(), ln), "require --auth-token")
				return
			}
			done := make(chan error, 1)
			go func() { done <- srv.Serve(t.Context(), ln) }()
			t.Cleanup(func() {
				_ = ln.Close()
				<-done
			})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/api/ping", http.NoBody)
			require.NoError(t, err)
			req.Host = "rebind.attacker.example"
			resp, err := newTestHTTPClient(t).Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		})
	}
}

func TestServerSameOriginToolCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, origin, fetchSite string
		want                    int
	}{
		{name: "native client", want: http.StatusNotFound},
		{name: "same origin", origin: "http://localhost:8080", fetchSite: "same-origin", want: http.StatusNotFound},
		{name: "cross site navigation", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "same site", fetchSite: "same-site", want: http.StatusForbidden},
		{name: "legacy browser", origin: "http://attacker.example", want: http.StatusForbidden},
		{name: "opaque origin", origin: "null", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A nil manager makes any attempt to load the team fail the test.
			var sm *SessionManager
			if tc.want == http.StatusNotFound {
				sm = NewSessionManager(t.Context(), nil, session.NewInMemorySessionStore(), 0, nil)
			}
			srv := NewWithManager(sm, "")
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8080/api/agents/known-agent/root/tools/count", http.NoBody)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			req.Header.Set("Sec-Fetch-Mode", "navigate")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}
