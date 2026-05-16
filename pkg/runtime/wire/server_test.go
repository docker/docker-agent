package wire_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/wire"
)

// stubBackend records the last request received and returns canned
// responses, so the round-trip tests can assert on both directions of
// the wire.
type stubBackend struct {
	loadFn func(context.Context, runtime.LoadTeamRequest) (wire.LoadTeamResponse, error)
	sessFn func(context.Context, runtime.CreateSessionRequest) (wire.CreateSessionResponse, error)
}

func (s *stubBackend) LoadTeam(ctx context.Context, req runtime.LoadTeamRequest) (wire.LoadTeamResponse, error) {
	return s.loadFn(ctx, req)
}

func (s *stubBackend) CreateSession(ctx context.Context, req runtime.CreateSessionRequest) (wire.CreateSessionResponse, error) {
	return s.sessFn(ctx, req)
}

// post is a small helper that issues a context-bound POST against the
// test server, so test cancellation doesn't leak goroutines and the
// noctx linter stays happy.
func post(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestLoadTeam_RoundTrip(t *testing.T) {
	t.Parallel()

	var got runtime.LoadTeamRequest
	srv := httptest.NewServer(wire.NewHandler(&stubBackend{
		loadFn: func(_ context.Context, req runtime.LoadTeamRequest) (wire.LoadTeamResponse, error) {
			got = req
			return wire.LoadTeamResponse{
				AgentNames:   []string{"main", "helper"},
				DefaultAgent: "main",
			}, nil
		},
	}))
	t.Cleanup(srv.Close)

	body, err := json.Marshal(runtime.LoadTeamRequest{
		ModelOverrides: []string{"openai/gpt-4o"},
		PromptFiles:    []string{"role.md"},
	})
	require.NoError(t, err)

	resp := post(t, srv.URL+"/v1/team/load", body)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out wire.LoadTeamResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	// Server received the same fields the client sent.
	assert.Equal(t, []string{"openai/gpt-4o"}, got.ModelOverrides)
	assert.Equal(t, []string{"role.md"}, got.PromptFiles)

	// Client received what the server sent.
	assert.Equal(t, []string{"main", "helper"}, out.AgentNames)
	assert.Equal(t, "main", out.DefaultAgent)
}

func TestCreateSession_RoundTrip(t *testing.T) {
	t.Parallel()

	var got runtime.CreateSessionRequest
	srv := httptest.NewServer(wire.NewHandler(&stubBackend{
		sessFn: func(_ context.Context, req runtime.CreateSessionRequest) (wire.CreateSessionResponse, error) {
			got = req
			return wire.CreateSessionResponse{SessionID: "sess-123"}, nil
		},
	}))
	t.Cleanup(srv.Close)

	body, err := json.Marshal(runtime.CreateSessionRequest{
		AgentName:        "main",
		ToolsApproved:    true,
		HideToolResults:  true,
		SessionDB:        "/tmp/s.db",
		ResumeSessionID:  "01HF8...",
		SnapshotsEnabled: true,
		WorkingDir:       "/tmp/work",
	})
	require.NoError(t, err)

	resp := post(t, srv.URL+"/v1/session/create", body)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out wire.CreateSessionResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	assert.Equal(t, "main", got.AgentName)
	assert.True(t, got.ToolsApproved)
	assert.True(t, got.HideToolResults)
	assert.Equal(t, "/tmp/s.db", got.SessionDB)
	assert.Equal(t, "01HF8...", got.ResumeSessionID)
	assert.True(t, got.SnapshotsEnabled)
	assert.Equal(t, "/tmp/work", got.WorkingDir)

	assert.Equal(t, "sess-123", out.SessionID)
}

func TestLoadTeam_BackendErrorMapsTo500(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(wire.NewHandler(&stubBackend{
		loadFn: func(context.Context, runtime.LoadTeamRequest) (wire.LoadTeamResponse, error) {
			return wire.LoadTeamResponse{}, errors.New("config invalid")
		},
	}))
	t.Cleanup(srv.Close)

	resp := post(t, srv.URL+"/v1/team/load", []byte(`{}`))
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestCreateSession_ErrUnsupportedMapsTo501(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(wire.NewHandler(&stubBackend{
		sessFn: func(context.Context, runtime.CreateSessionRequest) (wire.CreateSessionResponse, error) {
			return wire.CreateSessionResponse{}, fmt.Errorf("snapshots: %w", runtime.ErrUnsupported)
		},
	}))
	t.Cleanup(srv.Close)

	resp := post(t, srv.URL+"/v1/session/create", []byte(`{}`))
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestLoadTeam_RejectsUnknownField(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(wire.NewHandler(&stubBackend{
		loadFn: func(context.Context, runtime.LoadTeamRequest) (wire.LoadTeamResponse, error) {
			return wire.LoadTeamResponse{}, nil
		},
	}))
	t.Cleanup(srv.Close)

	body := []byte(`{"model_overrides":["x"],"future_field":"oops"}`)
	resp := post(t, srv.URL+"/v1/team/load", body)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestLoadTeam_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	// Pins the [http.MaxBytesReader] guard: an over-cap body must be
	// rejected with 4xx instead of being streamed into memory. The
	// handler logs the cause and replies via http.Error, which
	// translates the MaxBytesReader limit error into a 400 today;
	// what the test cares about is that the request never reaches
	// the backend and the server stays healthy.
	var reached bool
	srv := httptest.NewServer(wire.NewHandler(&stubBackend{
		loadFn: func(context.Context, runtime.LoadTeamRequest) (wire.LoadTeamResponse, error) {
			reached = true
			return wire.LoadTeamResponse{}, nil
		},
	}))
	t.Cleanup(srv.Close)

	// 2 MiB of JSON-string padding, well past the 1 MiB cap.
	padding := bytes.Repeat([]byte("a"), 2<<20)
	body := append([]byte(`{"model_overrides":["`), padding...)
	body = append(body, []byte(`"]}`)...)

	resp := post(t, srv.URL+"/v1/team/load", body)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.GreaterOrEqual(t, resp.StatusCode, 400, "oversized body must not return success")
	assert.Less(t, resp.StatusCode, 500, "oversized body is a client error, not a server crash")
	assert.False(t, reached, "backend must not see an oversized body")
}
