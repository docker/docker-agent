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
	"github.com/docker/docker-agent/pkg/chat"
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

// rawHTTPDo issues an HTTP request like httpDo but returns the status
// code and body without asserting < 400. Use it from tests that need
// to verify error responses (4xx/5xx).
func rawHTTPDo(t *testing.T, ctx context.Context, method, socketPath, path string, payload any) (int, []byte) {
	t.Helper()

	buf, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, method, "http://_"+path, bytes.NewReader(buf))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

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

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
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

func TestServer_UpdateSessionMode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	lnPath := startServerWithStore(t, ctx, prepareAgentsDir(t), store)

	// Create a session in default (build) mode.
	createResp := httpDo(t, ctx, http.MethodPost, lnPath, "/api/sessions", map[string]any{})
	var createdSession session.Session
	unmarshal(t, createResp, &createdSession)
	require.NotEmpty(t, createdSession.ID)

	// Switch the session into plan mode.
	patchResp := httpDo(t, ctx, http.MethodPatch, lnPath,
		"/api/sessions/"+createdSession.ID+"/mode",
		api.UpdateSessionModeRequest{Mode: session.ModePlan})
	var modeResp api.UpdateSessionModeResponse
	unmarshal(t, patchResp, &modeResp)

	assert.Equal(t, createdSession.ID, modeResp.ID)
	assert.Equal(t, session.ModePlan, modeResp.Mode)

	// GET should reflect the new mode.
	getResp := httpGET(t, ctx, lnPath, "/api/sessions/"+createdSession.ID)
	var sessionResp api.SessionResponse
	unmarshal(t, getResp, &sessionResp)
	assert.Equal(t, session.ModePlan, sessionResp.Mode)

	// Switch back to build mode.
	patchResp = httpDo(t, ctx, http.MethodPatch, lnPath,
		"/api/sessions/"+createdSession.ID+"/mode",
		api.UpdateSessionModeRequest{Mode: session.ModeBuild})
	unmarshal(t, patchResp, &modeResp)
	assert.Equal(t, session.ModeBuild, modeResp.Mode)
}

func TestServer_CreateSession_AcceptsMode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	lnPath := startServerWithStore(t, ctx, prepareAgentsDir(t), store)

	// Creating a session with mode=plan should persist that mode.
	createResp := httpDo(t, ctx, http.MethodPost, lnPath, "/api/sessions",
		map[string]any{"mode": string(session.ModePlan)})
	var createdSession session.Session
	unmarshal(t, createResp, &createdSession)
	require.NotEmpty(t, createdSession.ID)
	assert.Equal(t, session.ModePlan, createdSession.Mode)

	getResp := httpGET(t, ctx, lnPath, "/api/sessions/"+createdSession.ID)
	var sessionResp api.SessionResponse
	unmarshal(t, getResp, &sessionResp)
	assert.Equal(t, session.ModePlan, sessionResp.Mode)
}

func TestServer_RejectsInvalidMode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	lnPath := startServerWithStore(t, ctx, prepareAgentsDir(t), store)

	// Unknown mode values must be rejected with 400 rather than
	// silently coerced to build — mirrors the validation already done
	// by PATCH /api/sessions/:id/mode so clients get the same error
	// shape at create-time and at update-time.
	status, body := rawHTTPDo(t, ctx, http.MethodPost, lnPath, "/api/sessions",
		map[string]any{"mode": "yolo"})
	assert.Equal(t, http.StatusBadRequest, status)
	// Body is a JSON-escaped echo error message; match on the escaped form.
	assert.Contains(t, string(body), `invalid mode \"yolo\"`)
}

func TestServer_ForkSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()

	parent := session.New()
	parent.Title = "Original"
	parent.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("hello")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "hi there",
		})),
		session.NewMessageItem(session.UserMessage("ignore me")),
	}
	require.NoError(t, store.AddSession(ctx, parent))

	lnPath := startServerWithStore(t, ctx, prepareAgentsDir(t), store)

	// Happy path: fork before the second user message (flat index 2).
	resp := httpDo(t, ctx, http.MethodPost, lnPath,
		"/api/sessions/"+parent.ID+"/fork",
		api.ForkSessionRequest{MessageIndex: 2})
	var forked api.SessionResponse
	unmarshal(t, resp, &forked)

	assert.NotEqual(t, parent.ID, forked.ID)
	assert.Equal(t, "Original (fork 1)", forked.Title)
	require.Len(t, forked.Messages, 2)
	assert.Equal(t, "hello", forked.Messages[0].Message.Content)
	assert.Equal(t, "hi there", forked.Messages[1].Message.Content)

	// Fork must be persisted server-side so a subsequent GET returns it.
	getResp := httpGET(t, ctx, lnPath, "/api/sessions/"+forked.ID)
	var fetched api.SessionResponse
	unmarshal(t, getResp, &fetched)
	assert.Equal(t, forked.ID, fetched.ID)
	assert.Equal(t, "Original (fork 1)", fetched.Title)

	// Forking at the assistant message must be rejected with 400.
	rejected := httpRaw(t, ctx, http.MethodPost, lnPath,
		"/api/sessions/"+parent.ID+"/fork",
		api.ForkSessionRequest{MessageIndex: 1})
	assert.Equal(t, http.StatusBadRequest, rejected.StatusCode, rejected.body)

	// Forking past the end of the visible list (no real "after the last
	// message" full-clone shortcut) must also return 400, not 500. This
	// pins the sentinel-driven classification so future error-message
	// reshuffles can't silently flip the status code.
	outOfRange := httpRaw(t, ctx, http.MethodPost, lnPath,
		"/api/sessions/"+parent.ID+"/fork",
		api.ForkSessionRequest{MessageIndex: 99})
	assert.Equal(t, http.StatusBadRequest, outOfRange.StatusCode, outOfRange.body)
}

// httpRaw issues an HTTP request and returns the raw response without
// asserting on the status code, so tests can verify 4xx/5xx paths.
func httpRaw(t *testing.T, ctx context.Context, method, socketPath, path string, payload any) struct {
	StatusCode int
	body       string
} {
	t.Helper()

	var (
		body        io.Reader
		contentType string
	)
	if payload != nil {
		buf, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(buf)
		contentType = "application/json"
	} else {
		body = http.NoBody
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
	return struct {
		StatusCode int
		body       string
	}{StatusCode: resp.StatusCode, body: string(buf)}
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
