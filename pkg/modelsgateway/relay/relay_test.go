package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokens struct {
	values      []string
	invalidated []string
}

func (t *tokens) Token(context.Context) (string, error) {
	if len(t.values) == 0 {
		return "", errors.New("no token")
	}
	value := t.values[0]
	t.values = t.values[1:]
	return value, nil
}
func (t *tokens) Invalidate(value string) { t.invalidated = append(t.invalidated, value) }

type validator struct{}

func (validator) Validate(body []byte, query url.Values) (RequestInfo, error) {
	if query.Get("allowed") != "true" {
		return RequestInfo{}, errors.New("query is not allowed")
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return RequestInfo{}, errors.New("invalid JSON")
	}
	return RequestInfo{Stream: request.Stream, Values: map[string]string{"kind": "test"}}, nil
}

type observer struct{ complete bool }

func (o *observer) Observe(data []byte) { o.complete = o.complete || string(data) == "done" }
func (o *observer) Complete() bool      { return o.complete }

func target(raw string) Target {
	return func(*http.Request) (*url.URL, error) { return url.Parse(raw) }
}

func TestRelayForwardsValidatedRequest(t *testing.T) {
	var received *http.Request
	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Clone(r.Context())
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.Header().Set("Request-Id", "req-1")
		w.Header().Set("X-Internal", "secret")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	r, err := New(target(upstream.URL+"/proxy"), "/v1/messages", &tokens{values: []string{"token"}}, validator{},
		WithRequestHeaders("Content-Type", "X-Custom"))
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/relay?allowed=true", strings.NewReader(`{"stream":false}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Custom", "value")
	request.Header.Set("Cookie", "session=secret")
	request.Header.Set("Authorization", "Bearer attacker")
	w := httptest.NewRecorder()
	result, err := r.Serve(w, request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `{"ok":true}`, w.Body.String())
	assert.Equal(t, http.StatusOK, result.Status)
	assert.Equal(t, "test", result.Info.Values["kind"])
	assert.Equal(t, "/proxy/v1/messages", received.URL.Path)
	assert.Equal(t, "allowed=true", received.URL.RawQuery)
	assert.Equal(t, `{"stream":false}`, receivedBody)
	assert.Equal(t, "Bearer token", received.Header.Get("Authorization"))
	assert.Empty(t, received.Header.Get("X-Api-Key"))
	assert.Equal(t, "value", received.Header.Get("X-Custom"))
	assert.Empty(t, received.Header.Get("Cookie"))
	assert.Equal(t, "req-1", w.Header().Get("Request-Id"))
	assert.Empty(t, w.Header().Get("X-Internal"))
}

func TestRelayRefreshesOnceAndHidesPersistentUnauthorized(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	toks := &tokens{values: []string{"stale", "fresh"}}
	r, err := New(target(upstream.URL), "/messages", toks, validator{})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	_, err = r.Serve(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?allowed=true", strings.NewReader(`{}`)))
	require.NoError(t, err)
	assert.Equal(t, "ok", w.Body.String())
	assert.Equal(t, []string{"stale"}, toks.invalidated)
	assert.Equal(t, 2, calls)

	always401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer always401.Close()
	r, err = New(target(always401.URL), "/messages", &tokens{values: []string{"one", "two"}}, validator{})
	require.NoError(t, err)
	w = httptest.NewRecorder()
	_, err = r.Serve(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?allowed=true", strings.NewReader(`{}`)))
	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "credentials")
}

func TestRelayStreamsAndDetectsTruncation(t *testing.T) {
	for name, body := range map[string]string{
		"complete":  "data: hello\n\ndata: done\n\n",
		"truncated": "data: hello\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer upstream.Close()
			r, err := New(target(upstream.URL), "/messages", &tokens{values: []string{"token"}}, validator{},
				WithStreamObserver(func() StreamObserver { return &observer{} }),
				WithStreamInterruptedEvent([]byte("data: interrupted\n\n")))
			require.NoError(t, err)
			w := httptest.NewRecorder()
			_, err = r.Serve(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?allowed=true", strings.NewReader(`{"stream":true}`)))
			if name == "complete" {
				require.NoError(t, err)
				assert.Equal(t, body, w.Body.String())
			} else {
				require.Error(t, err)
				assert.Equal(t, body+"data: interrupted\n\n", w.Body.String())
			}
		})
	}
}

func TestRelayRefusesRedirectsAndBadInput(t *testing.T) {
	followed := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			followed = true
		}
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	r, err := New(target(upstream.URL), "/messages", &tokens{values: []string{"token"}}, validator{}, WithClient(&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { t.Fatal("must be replaced"); return nil },
	}))
	require.NoError(t, err)
	w := httptest.NewRecorder()
	_, err = r.Serve(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?allowed=true", strings.NewReader(`{}`)))
	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.False(t, followed)
	assert.Empty(t, w.Header().Get("Location"))

	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/?allowed=true", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/?no=true", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/?allowed=true", `{`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		_, _ = r.Serve(w, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body)))
		assert.Equal(t, tc.status, w.Code)
	}
}
