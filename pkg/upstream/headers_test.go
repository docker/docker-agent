package upstream

import (
	"net/http"
	"net/http/httptest"
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
