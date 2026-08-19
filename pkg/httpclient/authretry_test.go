package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnauthorizedRetry(t *testing.T) {
	t.Parallel()

	t.Run("replays the request with a fresh token", func(t *testing.T) {
		t.Parallel()

		var mu sync.Mutex
		var seen []string
		var bodies []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			bodies = append(bodies, string(body))
			mu.Unlock()

			if r.Header.Get("Authorization") != "Bearer fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}))
		defer srv.Close()

		client := NewHTTPClient(t.Context(), WithUnauthorizedRetry(func(_ context.Context, rejected string) (string, error) {
			assert.Equal(t, "stale", rejected)
			return "fresh", nil
		}))

		resp := post(t, client, srv.URL)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, []string{"Bearer stale", "Bearer fresh"}, seen)
		assert.Equal(t, []string{"payload", "payload"}, bodies, "the body is replayed as-is")
	})

	t.Run("refreshes every header carrying the token", func(t *testing.T) {
		t.Parallel()

		var mu sync.Mutex
		var apiKeys []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			apiKeys = append(apiKeys, r.Header.Get("X-Api-Key"))
			mu.Unlock()

			if r.Header.Get("X-Api-Key") != "fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}))
		defer srv.Close()

		client := NewHTTPClient(t.Context(), WithUnauthorizedRetry(func(context.Context, string) (string, error) {
			return "fresh", nil
		}))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("payload"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer stale")
		req.Header.Set("X-Api-Key", "stale")

		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, []string{"stale", "fresh"}, apiKeys)
	})

	t.Run("gives up after one retry", func(t *testing.T) {
		t.Parallel()

		var mu sync.Mutex
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		client := NewHTTPClient(t.Context(), WithUnauthorizedRetry(func(context.Context, string) (string, error) {
			return "fresh", nil
		}))

		resp := post(t, client, srv.URL)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, 2, calls)
	})

	t.Run("does not retry when no fresh token is available", func(t *testing.T) {
		t.Parallel()

		tests := map[string]func(context.Context, string) (string, error){
			"refresh fails":   func(context.Context, string) (string, error) { return "", assert.AnError },
			"same token":      func(_ context.Context, rejected string) (string, error) { return rejected, nil },
			"no token at all": func(context.Context, string) (string, error) { return "", nil },
		}

		for name, refresh := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var mu sync.Mutex
				var calls int
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					calls++
					mu.Unlock()
					w.WriteHeader(http.StatusUnauthorized)
				}))
				defer srv.Close()

				client := NewHTTPClient(t.Context(), WithUnauthorizedRetry(refresh))
				resp := post(t, client, srv.URL)
				defer resp.Body.Close()

				mu.Lock()
				defer mu.Unlock()
				assert.Equal(t, 1, calls)
			})
		}
	})

	t.Run("leaves other statuses alone", func(t *testing.T) {
		t.Parallel()

		var refreshed bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		client := NewHTTPClient(t.Context(), WithUnauthorizedRetry(func(context.Context, string) (string, error) {
			refreshed = true
			return "fresh", nil
		}))

		resp := post(t, client, srv.URL)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.False(t, refreshed)
	})

	t.Run("does not retry an unauthenticated request", func(t *testing.T) {
		t.Parallel()

		var refreshed bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		client := NewHTTPClient(t.Context(), WithUnauthorizedRetry(func(context.Context, string) (string, error) {
			refreshed = true
			return "fresh", nil
		}))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.False(t, refreshed, "nothing to refresh without a presented token")
	})
}

func TestPresentedToken(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "abc", presentedToken(http.Header{"Authorization": []string{"Bearer abc"}}))
	assert.Equal(t, "abc", presentedToken(http.Header{"Authorization": []string{"abc"}}))
	assert.Equal(t, "abc", presentedToken(http.Header{"X-Goog-Api-Key": []string{"abc"}}))
	assert.Empty(t, presentedToken(http.Header{}))
}

// post sends an authenticated POST presenting a token the fake servers below
// consider stale, with a body that has to survive a replay.
func post(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("payload"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer stale")

	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}
