//go:build darwin || linux || windows

package desktop

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	secretsengine "github.com/docker/secrets-engine/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretsEngineLegacyFallback(t *testing.T) {
	dir, err := os.MkdirTemp("", "se") //nolint:forbidigo,usetesting // t.TempDir embeds the test name and can exceed the Unix socket path limit.
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	for _, tt := range []struct {
		name   string
		status int
	}{
		{name: "missing"},
		{name: "404", status: http.StatusNotFound},
		{name: "501", status: http.StatusNotImplemented},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []Source{SourceDesktop, SourceMinted} {
				t.Run(string(source), func(t *testing.T) {
					fromLegacy := makeToken(t, time.Now().Add(time.Hour))
					account := DockerHubInfo{Username: "legacy-user", Email: "legacy@example.com"}
					backend := &fakeBackend{info: &account}
					if source == SourceDesktop {
						backend.token = fromLegacy
					}
					installFakeBackend(t, backend)
					if source == SourceMinted {
						mintToken = func(context.Context) (string, error) { return fromLegacy, nil }
					}

					opts := []secretsengine.Option{secretsengine.WithSocketPath(filepath.Join(dir, "engine.sock"))}
					if tt.status != 0 {
						ln := newMemListener()
						server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							http.Error(w, "unsupported API", tt.status)
						})}
						go func() { _ = server.Serve(ln) }()
						t.Cleanup(func() {
							_ = server.Close()
							_ = ln.Close()
						})
						opts = []secretsengine.Option{secretsengine.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
							return ln.dial(ctx)
						})}
					}
					client, err := secretsengine.New(opts...)
					require.NoError(t, err)
					engine := &secretsEngine{client: client.HubAuth()}
					old := defaultSecretsEngine
					defaultSecretsEngine = func() *secretsEngine { return engine }
					t.Cleanup(func() {
						engine.wait()
						defaultSecretsEngine = old
					})

					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					assert.Equal(t, account, GetUserInfo(ctx))
					token, gotSource := GetTokenWithSource(ctx)
					assert.Equal(t, fromLegacy, token)
					assert.Equal(t, source, gotSource)
					require.NoError(t, ctx.Err())
					require.Error(t, SecretsEngineError())
					if tt.status == 0 {
						require.ErrorIs(t, SecretsEngineError(), secretsengine.ErrSecretsEngineNotRunning)
					}
					_, err = engine.fetch(ctx)
					require.ErrorIs(t, err, errSecretsEngineCoolingDown)
					assert.Zero(t, backend.refreshes())
				})
			}
		})
	}
}
