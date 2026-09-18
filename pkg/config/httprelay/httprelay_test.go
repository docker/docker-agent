package httprelay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func source(raw string) Source {
	return func(*http.Request) (*url.URL, error) { return url.Parse(raw) }
}

func TestHandlerRelaysConfiguration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("X-Internal", "secret")
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = io.WriteString(w, "version: 1\n")
	}))
	defer upstream.Close()

	h, err := New(source(upstream.URL), WithMaxBytes(64))
	require.NoError(t, err)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, "/agent", http.NoBody))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/yaml", w.Header().Get("Content-Type"))
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		assert.Empty(t, w.Header().Get("X-Internal"))
		if method == http.MethodGet {
			assert.Equal(t, "version: 1\n", w.Body.String())
		} else {
			assert.Empty(t, w.Body.String())
		}
	}
}

func TestHandlerRejectsInvalidResponses(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"empty":     func(http.ResponseWriter, *http.Request) {},
		"oversized": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 9)) },
		"status":    func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "private detail", http.StatusNotFound) },
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(handler)
			defer upstream.Close()
			h, err := New(source(upstream.URL), WithMaxBytes(8))
			require.NoError(t, err)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
			assert.Equal(t, http.StatusBadGateway, w.Code)
			assert.NotContains(t, w.Body.String(), "private detail")
		})
	}
}

func TestHandlerRefusesRedirects(t *testing.T) {
	followed := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			followed = true
		}
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		t.Fatal("caller redirect policy must be replaced")
		return nil
	}}
	h, err := New(source(upstream.URL), WithClient(client))
	require.NoError(t, err)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.False(t, followed)
	assert.Empty(t, w.Header().Get("Location"))
}

func TestHandlerMethodAndOptions(t *testing.T) {
	_, err := New(nil)
	require.Error(t, err)
	_, err = New(source("https://example.com"), WithMaxBytes(0))
	require.Error(t, err)
	_, err = New(source("https://example.com"), WithTimeout(0*time.Second))
	require.Error(t, err)

	h, err := New(source("https://example.com"))
	require.NoError(t, err)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", http.NoBody))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
