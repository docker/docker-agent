package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

func TestServer_ListAgents(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("ANTHROPIC_API_KEY", "dummy")

	ctx := t.Context()
	lnPath := startServer(t, ctx, prepareAgentsDir(t, "contradict.yaml", "multi_agents.yaml", "pirate.yaml"))

	buf := httpGET(t, ctx, lnPath, "/api/agents")

	var agents []api.Agent
	unmarshal(t, buf, &agents)

	assert.Len(t, agents, 3)

	assert.Contains(t, agents[0].Name, "contradict")
	assert.Equal(t, "Contrarian viewpoint provider", agents[0].Description)
	assert.False(t, agents[0].Multi)

	assert.Contains(t, agents[1].Name, "multi_agents")
	assert.Equal(t, "Multi Agent", agents[1].Description)
	assert.True(t, agents[1].Multi)

	assert.Contains(t, agents[2].Name, "pirate")
	assert.Equal(t, "Talk like a pirate", agents[2].Description)
	assert.False(t, agents[2].Multi)
}

func TestServer_EmptyList(t *testing.T) {
	ctx := t.Context()
	lnPath := startServer(t, ctx, prepareAgentsDir(t))

	buf := httpGET(t, ctx, lnPath, "/api/agents")
	assert.Equal(t, "[]\n", string(buf)) // We don't want null, but an empty array
}

func TestServer_ListSessions(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	lnPath := startServer(t, ctx, prepareAgentsDir(t, "pirate.yaml"))

	buf := httpGET(t, ctx, lnPath, "/api/sessions")

	var sessions []api.SessionsResponse
	unmarshal(t, buf, &sessions)

	assert.Empty(t, sessions)
}

func prepareAgentsDir(t *testing.T, testFiles ...string) string {
	t.Helper()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	err := os.MkdirAll(agentsDir, 0o700)
	require.NoError(t, err)

	for _, file := range testFiles {
		buf, err := os.ReadFile(filepath.Join("testdata", file))
		require.NoError(t, err)

		err = os.WriteFile(filepath.Join(agentsDir, filepath.Base(file)), buf, 0o600)
		require.NoError(t, err)
	}

	return agentsDir
}

func startServer(t *testing.T, ctx context.Context, agentsDir string) string {
	t.Helper()

	var store mockStore
	runConfig := config.RuntimeConfig{}

	sources, err := config.ResolveSources(agentsDir, nil)
	require.NoError(t, err)
	srv, err := New(ctx, store, &runConfig, 0, sources, "")
	require.NoError(t, err)

	socketPath := "unix://" + filepath.Join(t.TempDir(), "sock")
	ln, err := Listen(ctx, socketPath)
	require.NoError(t, err)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	go func() {
		_ = srv.Serve(ctx, ln)
	}()

	return socketPath
}

func httpGET(t *testing.T, ctx context.Context, socketPath, path string) []byte {
	t.Helper()
	return httpDo(t, ctx, http.MethodGet, socketPath, path, nil)
}

func httpDo(t *testing.T, ctx context.Context, method, socketPath, path string, payload any) []byte {
	t.Helper()

	var (
		body        io.Reader
		contentType string
	)
	switch v := payload.(type) {
	case nil:
		body = http.NoBody
	case []byte:
		body = bytes.NewReader(v)
	case string:
		body = strings.NewReader(v)
	default:
		buf, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(buf)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://_"+path, body)
	require.NoError(t, err)

	req.Header.Set("Content-Type", contentType)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", strings.TrimPrefix(socketPath, "unix://"))
			},
		},
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	buf, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Less(t, resp.StatusCode, 400, string(buf))
	return buf
}

func unmarshal(t *testing.T, buf []byte, v any) {
	t.Helper()
	err := json.Unmarshal(buf, &v)
	require.NoError(t, err)
}

func TestServer_UpdateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	lnPath := startServerWithStore(t, ctx, prepareAgentsDir(t), store)

	// Create a session first
	createResp := httpDo(t, ctx, http.MethodPost, lnPath, "/api/sessions", map[string]any{})
	var createdSession session.Session
	unmarshal(t, createResp, &createdSession)
	require.NotEmpty(t, createdSession.ID)

	// Update the session title
	newTitle := "My Custom Title"
	updateResp := httpDo(t, ctx, http.MethodPatch, lnPath, "/api/sessions/"+createdSession.ID+"/title", api.UpdateSessionTitleRequest{Title: newTitle})
	var titleResp api.UpdateSessionTitleResponse
	unmarshal(t, updateResp, &titleResp)

	assert.Equal(t, createdSession.ID, titleResp.ID)
	assert.Equal(t, newTitle, titleResp.Title)

	// Verify the session was updated in the store
	getResp := httpGET(t, ctx, lnPath, "/api/sessions/"+createdSession.ID)
	var sessionResp api.SessionResponse
	unmarshal(t, getResp, &sessionResp)

	assert.Equal(t, newTitle, sessionResp.Title)
}

func startServerWithStore(t *testing.T, ctx context.Context, agentsDir string, store session.Store) string {
	t.Helper()

	runConfig := config.RuntimeConfig{}

	sources, err := config.ResolveSources(agentsDir, nil)
	require.NoError(t, err)
	srv, err := New(ctx, store, &runConfig, 0, sources, "")
	require.NoError(t, err)

	socketPath := "unix://" + filepath.Join(t.TempDir(), "sock")
	ln, err := Listen(ctx, socketPath)
	require.NoError(t, err)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	go func() {
		_ = srv.Serve(ctx, ln)
	}()

	return socketPath
}

type mockStore struct {
	session.Store
}

func (s mockStore) GetSessions(context.Context) ([]*session.Session, error) {
	return nil, nil
}

func (s mockStore) GetSessionSummaries(context.Context) ([]session.Summary, error) {
	return nil, nil
}
