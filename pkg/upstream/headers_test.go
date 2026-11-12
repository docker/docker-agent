package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeadersRoundTrip(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Set("Authorization", "Bearer token123")
	h.Set("X-Custom", "value")

	ctx := WithHeaders(t.Context(), h)
	got := HeadersFromContext(ctx)

	require.NotNil(t, got)
	assert.Equal(t, "Bearer token123", got.Get("Authorization"))
	assert.Equal(t, "value", got.Get("X-Custom"))
}

func TestHeadersFromContext_Empty(t *testing.T) {
	t.Parallel()

	got := HeadersFromContext(t.Context())
	assert.Nil(t, got)
}

func TestHandler_InjectsHeaders(t *testing.T) {
	t.Parallel()

	var captured http.Header
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = HeadersFromContext(r.Context())
	})

	handler := Handler(inner)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	req.Header.Set("X-Test", "hello")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.NotNil(t, captured)
	assert.Equal(t, "hello", captured.Get("X-Test"))
}

func TestSameOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		origin string
		target string
		want   bool
	}{
		{name: "same", origin: "https://example.com/path", target: "https://example.com/other", want: true},
		{name: "case insensitive", origin: "HTTPS://EXAMPLE.COM/path", target: "https://example.com/other", want: true},
		{name: "default HTTPS port", origin: "https://example.com", target: "https://example.com:443/other", want: true},
		{name: "different port", origin: "https://example.com", target: "https://example.com:8443/other", want: false},
		{name: "different scheme", origin: "https://example.com", target: "http://example.com/other", want: false},
		{name: "compressed IPv6", origin: "https://[2001:db8::1]", target: "https://[2001:0db8:0:0:0:0:0:1]:443/other", want: true},
		{name: "IPv4 mapped", origin: "https://[::ffff:192.0.2.1]", target: "https://192.0.2.1/other", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, err := url.Parse(tt.target)
			require.NoError(t, err)
			assert.Equal(t, tt.want, SameOrigin(tt.origin, target))
		})
	}
}

func TestResolveHeaders(t *testing.T) {
	t.Parallel()

	upstream := http.Header{}
	upstream.Set("Authorization", "Bearer secret")
	upstream.Set("X-Request-Id", "abc-123")

	ctx := WithHeaders(t.Context(), upstream)

	tests := []struct {
		name     string
		headers  map[string]string
		expected map[string]string
	}{
		{
			name:     "no placeholders",
			headers:  map[string]string{"Content-Type": "application/json"},
			expected: map[string]string{"Content-Type": "application/json"},
		},
		{
			name:     "single placeholder",
			headers:  map[string]string{"Authorization": "${headers.Authorization}"},
			expected: map[string]string{"Authorization": "Bearer secret"},
		},
		{
			name:     "case insensitive header name",
			headers:  map[string]string{"Authorization": "${headers.authorization}"},
			expected: map[string]string{"Authorization": "Bearer secret"},
		},
		{
			name:     "multiple headers with placeholders",
			headers:  map[string]string{"Authorization": "${headers.Authorization}", "X-Req": "${headers.X-Request-Id}"},
			expected: map[string]string{"Authorization": "Bearer secret", "X-Req": "abc-123"},
		},
		{
			name:     "mixed static and placeholder",
			headers:  map[string]string{"Authorization": "${headers.Authorization}", "Accept": "text/html"},
			expected: map[string]string{"Authorization": "Bearer secret", "Accept": "text/html"},
		},
		{
			name:     "placeholder with surrounding text",
			headers:  map[string]string{"X-Info": "id=${headers.X-Request-Id}&ok"},
			expected: map[string]string{"X-Info": "id=abc-123&ok"},
		},
		{
			name:     "missing upstream header resolves to empty",
			headers:  map[string]string{"Authorization": "${headers.X-Missing}"},
			expected: map[string]string{"Authorization": ""},
		},
		{
			name:     "nil headers",
			headers:  nil,
			expected: nil,
		},
		{
			name:     "empty headers",
			headers:  map[string]string{},
			expected: map[string]string{},
		},
		{
			name:     "trimmed spaces in name",
			headers:  map[string]string{"Auth": "${headers. Authorization }"},
			expected: map[string]string{"Auth": "Bearer secret"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveHeaders(ctx, tt.headers)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestResolveHeaders_NoUpstreamContext(t *testing.T) {
	t.Parallel()

	headers := map[string]string{
		"Authorization": "${headers.Authorization}",
		"Accept":        "text/html",
	}

	// No upstream headers in context — placeholders are left as-is.
	got := ResolveHeaders(t.Context(), headers)
	assert.Equal(t, headers, got)
}

// captureTransport is a stub RoundTripper that records the headers of each
// request it receives.
type captureTransport struct {
	seen []http.Header
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.seen = append(c.seen, req.Header.Clone())
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestNewHeaderTransport_ResolvesFromContextPerRequest(t *testing.T) {
	t.Parallel()

	capture := &captureTransport{}
	transport := NewHeaderTransport(capture, map[string]string{"Authorization": "${headers.Authorization}"})

	do := func(ctx context.Context) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/", http.NoBody)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	up1 := http.Header{}
	up1.Set("Authorization", "Bearer one")
	do(WithHeaders(t.Context(), up1))

	up2 := http.Header{}
	up2.Set("Authorization", "Bearer two")
	do(WithHeaders(t.Context(), up2))

	require.Len(t, capture.seen, 2)
	assert.Equal(t, "Bearer one", capture.seen[0].Get("Authorization"))
	assert.Equal(t, "Bearer two", capture.seen[1].Get("Authorization"),
		"each request must resolve against its own context, not a cached value")
}

func TestNewHeaderTransportWithResolver_InvokedPerRequest(t *testing.T) {
	t.Parallel()

	var calls int
	resolve := func(_ context.Context, headers map[string]string) map[string]string {
		calls++
		return map[string]string{"X-Call": fmt.Sprintf("%s-%d", headers["X-Call"], calls)}
	}

	capture := &captureTransport{}
	transport := NewHeaderTransportWithResolver(capture, map[string]string{"X-Call": "req"}, resolve)

	for range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test/", http.NoBody)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	require.Len(t, capture.seen, 2)
	assert.Equal(t, "req-1", capture.seen[0].Get("X-Call"))
	assert.Equal(t, "req-2", capture.seen[1].Get("X-Call"),
		"the resolver must run on every request so dynamic values stay fresh")
}

func TestNewHeaderTransport_DoesNotForwardHeadersAcrossOrigins(t *testing.T) {
	t.Parallel()

	var redirectedHeader string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	var sourceHeader string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHeader = r.Header.Get("Authorization")
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	client := &http.Client{
		Transport: NewHeaderTransportForOrigin(
			source.Client().Transport,
			source.URL,
			map[string]string{"Authorization": "Bearer secret"},
		),
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, source.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "Bearer secret", sourceHeader)
	assert.Empty(t, redirectedHeader)
}

func TestNewHeaderTransport_ForwardsHeadersOnSameOriginRedirect(t *testing.T) {
	t.Parallel()

	var redirectedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		redirectedHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &http.Client{
		Transport: NewHeaderTransportForOrigin(
			server.Client().Transport,
			server.URL,
			map[string]string{"Authorization": "Bearer secret"},
		),
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/start", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "Bearer secret", redirectedHeader)
}

func TestNewHeaderTransport_RejectsHTTPSDowngradeRedirect(t *testing.T) {
	t.Parallel()

	capture := &captureTransport{}
	transport := NewHeaderTransportForOrigin(capture, "https://example.test", map[string]string{
		"Authorization": "Bearer secret",
	})
	original := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test/start", http.NoBody)
	redirected := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test/final", http.NoBody)
	redirected.Response = &http.Response{Request: original}

	resp, err := transport.RoundTrip(redirected)
	if resp != nil {
		defer resp.Body.Close()
	}
	require.ErrorContains(t, err, "refusing HTTPS downgrade redirect")
	assert.Empty(t, capture.seen)
}
