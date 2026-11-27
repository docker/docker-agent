package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
)

// activeSessionsServer serves GET /api/sessions?active=true with the given
// session IDs, mimicking a --listen control plane.
func activeSessionsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("active") != "true" {
			http.Error(w, "test server only serves active sessions", http.StatusBadRequest)
			return
		}
		sessions := make([]api.SessionsResponse, 0, len(ids))
		for _, id := range ids {
			sessions = append(sessions, api.SessionsResponse{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sessions); err != nil {
			t.Errorf("encoding sessions: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveAttachSession_UsesAttachedRecordedSession(t *testing.T) {
	t.Parallel()

	srv := activeSessionsServer(t, "initial", "spawned")

	sessionID, err := ResolveAttachSession(t.Context(), srv.URL, "initial", true)
	require.NoError(t, err)
	assert.Equal(t, "initial", sessionID)

	sessionID, err = ResolveAttachSession(t.Context(), srv.URL, "spawned", false)
	require.NoError(t, err)
	assert.Equal(t, "spawned", sessionID)
}

func TestResolveAttachSession_ExplicitClosedSessionFailsClearly(t *testing.T) {
	t.Parallel()

	srv := activeSessionsServer(t, "spawned")

	_, err := ResolveAttachSession(t.Context(), srv.URL, "closed-initial", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed-initial")
	assert.Contains(t, err.Error(), "no longer attached")
	assert.Contains(t, err.Error(), "spawned", "the error must list the live sessions")
}

func TestResolveAttachSession_DiscoveryResolvesUnambiguousLiveSession(t *testing.T) {
	t.Parallel()

	srv := activeSessionsServer(t, "spawned")

	sessionID, err := ResolveAttachSession(t.Context(), srv.URL, "closed-initial", false)
	require.NoError(t, err)
	assert.Equal(t, "spawned", sessionID, "run-level discovery must resolve the live tab instead of the closed initial session")
}

func TestResolveAttachSession_DiscoveryFailsWhenAmbiguous(t *testing.T) {
	t.Parallel()

	srv := activeSessionsServer(t, "tab-a", "tab-b")

	_, err := ResolveAttachSession(t.Context(), srv.URL, "closed-initial", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tab-a")
	assert.Contains(t, err.Error(), "tab-b")
	assert.Contains(t, err.Error(), "--attach=<session-id>")
}

func TestResolveAttachSession_DiscoveryFailsWhenRunHasNoTabs(t *testing.T) {
	t.Parallel()

	srv := activeSessionsServer(t)

	_, err := ResolveAttachSession(t.Context(), srv.URL, "closed-initial", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no attached session")
}

func TestAttachServer_RequiresArgs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, err := AttachServer(ctx, "", "session-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "addr and sessionID")

	_, err = AttachServer(ctx, "http://127.0.0.1:1234", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "addr and sessionID")
}

func TestAttachServer_BuildsWithValidArgs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	srv, err := AttachServer(ctx, "http://127.0.0.1:1234", "session-1")
	require.NoError(t, err)
	assert.NotNil(t, srv)
}
