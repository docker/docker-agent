package builtins_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestHTTPPostSendsBodyToURL pins the happy path: POST with body
// and Content-Type: application/json, and a nil Output.
func TestHTTPPostSendsBodyToURL(t *testing.T) {
	t.Parallel()

	const payload = `{"event":"turn_start"}`

	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	fn := lookup(t, builtins.HTTPPost)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, []string{srv.URL, payload})
	require.NoError(t, err)
	assert.Nil(t, out)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.JSONEq(t, payload, string(gotBody))
}

// TestHTTPPostEmptyBodyIsAllowed: omitting the second arg sends an
// empty body — useful for ping-style webhooks.
func TestHTTPPostEmptyBodyIsAllowed(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	fn := lookup(t, builtins.HTTPPost)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, []string{srv.URL})
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.Empty(t, gotBody)
}

// TestHTTPPostNoOpWithoutURL: a missing or empty URL is a no-op so
// a misconfigured YAML doesn't break the run loop.
func TestHTTPPostNoOpWithoutURL(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.HTTPPost)

	cases := [][]string{
		nil,
		{},
		{""},
		{"", "body"},
	}
	for _, args := range cases {
		out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, args)
		require.NoErrorf(t, err, "args=%v: must not error", args)
		assert.Nilf(t, out, "args=%v: must be a no-op", args)
	}
}

// TestHTTPPostSwallowsErrors: neither a non-2xx response nor an
// unreachable receiver propagates as a hook error.
func TestHTTPPostSwallowsErrors(t *testing.T) {
	t.Parallel()

	serverError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(serverError.Close)

	// Bind, capture URL, then close: the port is now guaranteed-unreachable.
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := unreachable.URL
	unreachable.Close()

	cases := map[string]string{
		"non-2xx response":     serverError.URL,
		"unreachable receiver": unreachableURL,
	}

	fn := lookup(t, builtins.HTTPPost)
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, []string{url, "{}"})
			require.NoError(t, err)
			assert.Nil(t, out)
		})
	}
}

// TestHTTPPostRejectsNonHTTPSchemes: file://, ftp://, javascript: and
// scheme-less or host-less inputs all surface as a config error
// rather than being silently dispatched to a transport.
func TestHTTPPostRejectsNonHTTPSchemes(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.HTTPPost)

	cases := []string{
		"file:///etc/passwd",
		"ftp://example.com/",
		"javascript:alert(1)",
		"not-a-url",
		"http://",
		"http://\x7f\x00.example",
	}
	for _, raw := range cases {
		out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, []string{raw, "{}"})
		require.Errorf(t, err, "input %q must be rejected", raw)
		assert.Nil(t, out)
		assert.Contains(t, err.Error(), "http_post:")
		assert.Contains(t, err.Error(), "http(s)")
	}
}

// TestHTTPPostHonoursContextCancellation: the request returns
// promptly after ctx deadline instead of waiting for the handler.
func TestHTTPPostHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	// Bounded sleep so the client must abandon before the response,
	// while keeping httptest.Server.Close() cleanup quick.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	fn := lookup(t, builtins.HTTPPost)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	out, err := fn(ctx, &hooks.Input{SessionID: "s"}, []string{srv.URL, "{}"})
	elapsed := time.Since(start)

	// Network errors (incl. cancellation) are swallowed by design.
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.Less(t, elapsed, 250*time.Millisecond)
}
