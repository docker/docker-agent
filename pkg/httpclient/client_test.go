package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userid"
)

// TestMain redirects the config directory used by [userid.Get] to a
// throw-away temp dir so the package's tests, which exercise
// gateway-bound HTTP requests, never read or write the real user-uuid
// file in the developer's config dir. Individual tests can still
// override the directory and call [userid.ResetForTests] for finer
// control.
func TestMain(m *testing.M) {
	//nolint:forbidigo // TestMain has no *testing.T, so t.TempDir is unavailable.
	dir, err := os.MkdirTemp("", "httpclient-test-config-*")
	if err != nil {
		panic(err)
	}

	paths.SetConfigDir(dir)
	userid.ResetForTests()

	code := m.Run()

	paths.SetConfigDir("")
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		opts       []Opt
		wantHeader string
		wantValue  string
	}{
		{
			name:       "WithModel sets X-Cagent-Model",
			opts:       []Opt{WithModel("gpt-4o")},
			wantHeader: "X-Cagent-Model",
			wantValue:  "gpt-4o",
		},
		{
			name:       "WithModelName sets X-Cagent-Model-Name",
			opts:       []Opt{WithModelName("my-fast-model")},
			wantHeader: "X-Cagent-Model-Name",
			wantValue:  "my-fast-model",
		},
		{
			name:       "WithModelName skips header when empty",
			opts:       []Opt{WithModelName("")},
			wantHeader: "X-Cagent-Model-Name",
			wantValue:  "",
		},
		{
			name:       "WithProvider sets X-Cagent-Provider",
			opts:       []Opt{WithProvider("openai")},
			wantHeader: "X-Cagent-Provider",
			wantValue:  "openai",
		},
		{
			name:       "compression is disabled to support SSE streaming",
			wantHeader: "Accept-Encoding",
			wantValue:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			headers := doRequest(t, tt.opts...)

			if tt.wantValue != "" {
				assert.Equal(t, tt.wantValue, headers.Get(tt.wantHeader))
			} else {
				assert.Empty(t, headers.Get(tt.wantHeader))
			}
		})
	}
}

// doRequest creates an HTTP client with the given options, sends a GET request
// to a test server, and returns the headers the server received.
func doRequest(t *testing.T, opts ...Opt) http.Header {
	t.Helper()
	return doRequestWithCtx(t, t.Context(), opts...)
}

// doRequestWithCtx is like doRequest but uses the supplied context for
// the outbound request, so callers can exercise context-derived header
// injection (e.g. session ID propagation).
func doRequestWithCtx(t *testing.T, ctx context.Context, opts ...Opt) http.Header {
	t.Helper()

	var capturedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header
	}))
	defer srv.Close()

	client := NewHTTPClient(ctx, opts...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	return capturedHeaders
}

func TestSessionIDHeader_GatewayBoundOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		ctxSessionID   string
		opts           []Opt
		wantHeaderSent bool
	}{
		{
			name:           "session ID present, gateway-bound (X-Cagent-Forward set) → header sent",
			ctxSessionID:   "sess-abc",
			opts:           []Opt{WithProxiedBaseURL("https://gateway.example/v1")},
			wantHeaderSent: true,
		},
		{
			name:           "session ID present, no X-Cagent-Forward → header skipped",
			ctxSessionID:   "sess-abc",
			opts:           nil,
			wantHeaderSent: false,
		},
		{
			name:           "no session ID on context, gateway-bound → header skipped",
			ctxSessionID:   "",
			opts:           []Opt{WithProxiedBaseURL("https://gateway.example/v1")},
			wantHeaderSent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			if tt.ctxSessionID != "" {
				ctx = ContextWithSessionID(ctx, tt.ctxSessionID)
			}
			headers := doRequestWithCtx(t, ctx, tt.opts...)

			if tt.wantHeaderSent {
				assert.Equal(t, tt.ctxSessionID, headers.Get("X-Cagent-Session-Id"))
			} else {
				assert.Empty(t, headers.Get("X-Cagent-Session-Id"))
			}
		})
	}
}

func TestContextWithSessionID_RoundTrip(t *testing.T) {
	t.Parallel()

	assert.Empty(t, SessionIDFromContext(t.Context()), "empty context yields empty session ID")
	ctx := ContextWithSessionID(t.Context(), "sess-xyz")
	assert.Equal(t, "sess-xyz", SessionIDFromContext(ctx))
}

func TestCagentIDHeader_GatewayBoundOnly(t *testing.T) {
	// Pin the persistent UUID file to a temp dir so the test does
	// not touch the real config dir and the value is deterministic.
	// We do not call t.Parallel because we mutate the package-level
	// paths override and the userid cache.
	const stored = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	withStoredUserUUID(t, stored)

	tests := []struct {
		name           string
		opts           []Opt
		wantHeaderSent bool
	}{
		{
			name:           "gateway-bound (X-Cagent-Forward set) → X-Cagent-Id sent",
			opts:           []Opt{WithProxiedBaseURL("https://gateway.example/v1")},
			wantHeaderSent: true,
		},
		{
			name:           "no X-Cagent-Forward → X-Cagent-Id skipped",
			opts:           nil,
			wantHeaderSent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := doRequest(t, tt.opts...)

			if tt.wantHeaderSent {
				assert.Equal(t, stored, headers.Get("X-Cagent-Id"))
			} else {
				assert.Empty(t, headers.Get("X-Cagent-Id"))
			}
		})
	}
}

// withStoredUserUUID seeds a fixed UUID into a temporary config dir for
// the duration of the test, so the persistent identifier surfaced by
// userid.Get is deterministic and isolated from other tests. The
// previous override is restored on cleanup so we keep the package-wide
// isolation set up by [TestMain].
func withStoredUserUUID(t *testing.T, id string) {
	t.Helper()

	_, err := uuid.Parse(id)
	require.NoError(t, err, "seeded value must be a valid UUID")

	previous := paths.GetConfigDir()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "user-uuid"), []byte(id), 0o600))

	paths.SetConfigDir(dir)
	userid.ResetForTests()
	t.Cleanup(func() {
		paths.SetConfigDir(previous)
		userid.ResetForTests()
	})
}
