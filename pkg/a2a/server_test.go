package a2a

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestRoutableAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ipv6 wildcard", "[::]:8080", "localhost:8080"},
		{"ipv4 wildcard", "0.0.0.0:8080", "localhost:8080"},
		{"empty host", ":8080", "localhost:8080"},
		{"localhost stays", "localhost:8080", "localhost:8080"},
		{"ipv4 loopback stays", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"specific ip stays", "192.168.1.1:9090", "192.168.1.1:9090"},
		{"hostname stays", "my-host:8080", "my-host:8080"},
		{"invalid addr returned as-is", "not-a-host-port", "not-a-host-port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, routableAddr(tt.addr))
		})
	}
}

func TestRun_RejectsAutonomousYAMLSafety(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	err = Run(t.Context(), "testdata/autonomous.yaml", "root", filepath.Join(t.TempDir(), "session.db"), &config.RuntimeConfig{}, ln, RunOptions{})
	require.ErrorContains(t, err, "--safety autonomous")
}

// TestRun_StopsOnContextCancel: canceling the context must make Run stop
// serving and return, releasing the session store's file handles (otherwise
// t.TempDir cleanup fails on Windows with an open session.db).
func TestRun_StopsOnContextCancel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	sessionDB := filepath.Join(t.TempDir(), "session.db")

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "testdata/basic.yaml", "root", sessionDB, &config.RuntimeConfig{}, ln, RunOptions{})
	}()

	// Cancel only once the server actually serves, so the test exercises a
	// mid-serve shutdown rather than a pre-start one.
	cardURL := "http://" + ln.Addr().String() + a2asrv.WellKnownAgentCardPath
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, http.NoBody)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 20*time.Millisecond, "A2A server never started serving")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Run must return nil on context cancellation")
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestServerSecurity(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "test")))
	store := session.NewInMemorySessionStore()
	server, err := newServer(tm, "test.yaml", "root", store, servesafety.Resolved{}, testWorkspaceRoot, "127.0.0.1:0", RunOptions{AuthToken: "secret", CORSOrigin: "https://app.example.com"})
	require.NoError(t, err)

	request := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}

	for _, path := range []string{a2asrv.WellKnownAgentCardPath, "/invoke"} {
		response := request(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
	}
	response := request(http.MethodGet, a2asrv.WellKnownAgentCardPath, map[string]string{"Authorization": "Bearer secret"})
	require.Equal(t, http.StatusOK, response.Code)

	response = request(http.MethodOptions, "/invoke", map[string]string{
		"Origin": "https://app.example.com", "Access-Control-Request-Method": http.MethodPost,
		"Access-Control-Request-Headers": "authorization,content-type",
	})
	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "https://app.example.com", response.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, response.Header().Get("Access-Control-Allow-Headers"), "Authorization")
}

func TestCorsMiddlewareConfigRejectsInvalidOrigin(t *testing.T) {
	t.Parallel()
	_, err := corsMiddlewareConfig("not an origin")
	require.Error(t, err)
}
