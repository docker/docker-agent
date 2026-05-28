package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

// httpDoStatus is a slim variant of httpDo that exposes the response
// status code. The standard helper assumes 2xx and only returns the body;
// these tests assert on 4xx, so they need direct access to the status.
func httpDoStatus(t *testing.T, ctx context.Context, method, socketPath, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://_"+path, http.NoBody)
	require.NoError(t, err)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", strings.TrimPrefix(socketPath, "unix://"))
			},
		},
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

func startServerBare(t *testing.T, ctx context.Context) string {
	t.Helper()
	var store mockStore
	runConfig := config.RuntimeConfig{}
	sources, err := config.ResolveSources(t.TempDir(), nil)
	require.NoError(t, err)
	srv, err := New(ctx, store, &runConfig, 0, sources, "")
	require.NoError(t, err)

	socketPath := "unix://" + filepath.Join(t.TempDir(), "sock")
	ln, err := Listen(ctx, socketPath)
	require.NoError(t, err)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	go func() { _ = srv.Serve(ctx, ln) }()
	return socketPath
}

// The happy path (waiter registered, callback delivered) is covered end
// to end in TestUnmanagedOAuthFlow_DriveFlow_AcceptsDirectCallback in
// pkg/tools/mcp. The server-side tests here focus on the input
// validation and the 404 response shape so the embedder's HTTP client
// can rely on it.

// Short test names because the macOS unix-socket path limit (104 bytes)
// includes t.TempDir() which embeds the test name.

func TestMcpOAuthCb_Unknown(t *testing.T) {
	ctx := t.Context()
	lnPath := startServerBare(t, ctx)

	status := httpDoStatus(t, ctx, http.MethodPost, lnPath,
		"/api/mcp-oauth/callback?state=unknown-state&code=abc")
	assert.Equal(t, http.StatusNotFound, status)
}

func TestMcpOAuthCb_NoState(t *testing.T) {
	ctx := t.Context()
	lnPath := startServerBare(t, ctx)

	status := httpDoStatus(t, ctx, http.MethodPost, lnPath,
		"/api/mcp-oauth/callback?code=abc")
	assert.Equal(t, http.StatusBadRequest, status)
}

func TestMcpOAuthCb_NoCode(t *testing.T) {
	ctx := t.Context()
	lnPath := startServerBare(t, ctx)

	status := httpDoStatus(t, ctx, http.MethodPost, lnPath,
		"/api/mcp-oauth/callback?state=some-state")
	assert.Equal(t, http.StatusBadRequest, status)
}
