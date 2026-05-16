package echolog

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedactedRequestLogger_DropsQueryString verifies that secrets passed
// through query parameters never reach slog. Echo's default RequestLogger
// emits v.URI (which includes "?token=..."); RedactedRequestLogger must
// log v.URIPath instead.
func TestRedactedRequestLogger_DropsQueryString(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := echo.New()
	e.Use(RedactedRequestLogger())
	e.GET("/api/things", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/api/things?token=swordfish&user=alice",
		http.NoBody,
	)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	logged := buf.String()
	assert.Contains(t, logged, "/api/things", "the path itself should be logged")
	assert.NotContains(t, logged, "swordfish", "query parameter values must not leak into logs")
	assert.NotContains(t, logged, "token=", "query parameter names+values must not leak")
	// Sanity: the path key should be present, not "uri".
	assert.Contains(t, logged, "path=", "expected path= attribute, got: %s", logged)
}

// ctxCapturingHandler is a slog.Handler that records a value extracted
// from the context passed to Handle, so tests can assert that per-request
// context values reach slog.
type ctxCapturingHandler struct {
	inner slog.Handler
	key   any
	value any
}

func (h *ctxCapturingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *ctxCapturingHandler) Handle(ctx context.Context, r slog.Record) error {
	if v := ctx.Value(h.key); v != nil {
		h.value = v
	}
	return h.inner.Handle(ctx, r)
}

func (h *ctxCapturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ctxCapturingHandler{inner: h.inner.WithAttrs(attrs), key: h.key}
}

func (h *ctxCapturingHandler) WithGroup(name string) slog.Handler {
	return &ctxCapturingHandler{inner: h.inner.WithGroup(name), key: h.key}
}

type ctxKey struct{}

// TestRedactedRequestLogger_PropagatesRequestContext verifies that the
// per-request context (carrying trace spans, correlation IDs, etc.) is
// passed through to slog instead of being replaced by context.Background().
func TestRedactedRequestLogger_PropagatesRequestContext(t *testing.T) {
	var buf bytes.Buffer
	capturing := &ctxCapturingHandler{
		inner: slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
		key:   ctxKey{},
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(capturing))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := echo.New()
	e.Use(RedactedRequestLogger())
	e.GET("/ping", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	ctx := context.WithValue(t.Context(), ctxKey{}, "trace-42")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/ping", http.NoBody)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, "trace-42", capturing.value,
		"request context values must be propagated to slog (not replaced by context.Background())")
}
