package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	assert.Empty(t, agents[0].Commands)

	assert.Contains(t, agents[1].Name, "multi_agents")
	assert.Equal(t, "Multi Agent", agents[1].Description)
	assert.True(t, agents[1].Multi)
	assert.Equal(t, []string{"contradict", "pirate"}, agents[1].Commands)

	assert.Contains(t, agents[2].Name, "pirate")
	assert.Equal(t, "Talk like a pirate", agents[2].Description)
	assert.False(t, agents[2].Multi)
	assert.Empty(t, agents[2].Commands)
}

func TestServer_EmptyList(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	lnPath := startServer(t, ctx, prepareAgentsDir(t))

	buf := httpGET(t, ctx, lnPath, "/api/agents")
	assert.Equal(t, "[]\n", string(buf)) // We don't want null, but an empty array
}

// TestServer_ZeroAgentSource pins the fix for docker/docker-agent#3588:
// a config source with no agents must never make GET /api/agents panic
// (latest.Agents.First() panics on an empty slice). Today validateConfig
// rejects the agent-less config at load time, so the handler's own
// len(cfg.Agents)==0 guard (agentsAPIEntry) never even gets exercised by
// this path — the request still yields a clean, empty listing rather than
// a panic either way.
func TestServer_ZeroAgentSource(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	lnPath := startServer(t, ctx, prepareAgentsDir(t, "no_agents.yaml", "pirate.yaml"))

	buf := httpGET(t, ctx, lnPath, "/api/agents")

	var agents []api.Agent
	unmarshal(t, buf, &agents)

	require.Len(t, agents, 1)
	assert.Contains(t, agents[0].Name, "pirate")
}

// TestServer_OversizedBodyRejected pins the fix for docker/docker-agent#3595:
// a request body over the 1 MiB cap must be rejected with 413 before it
// reaches a JSON-decoding handler. The Content-Length header alone triggers
// the rejection, so no SessionManager is needed.
func TestServer_OversizedBodyRejected(t *testing.T) {
	t.Parallel()

	srv := NewWithManager(nil, "")

	body := bytes.Repeat([]byte("a"), int(defaultMaxRequestBytes)+1)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// TestServer_MaxRequestBytesOption verifies that WithMaxRequestBytes wires a
// custom body-size cap: bodies under the limit reach handlers normally, while
// bodies over the limit are rejected with 413 before any handler runs.
//
// The test targets POST /api/sessions/:id/messages because the issue (#3937)
// specifically calls out that route. With a nil SessionManager the handler
// returns 400 ("message is required") for an under-limit request — any
// non-413 status confirms the body cap was not exceeded.
func TestServer_MaxRequestBytesOption(t *testing.T) {
	t.Parallel()

	const bodyLimit = 16
	srv := NewWithManager(nil, "", WithMaxRequestBytes(bodyLimit))

	cases := []struct {
		name    string
		body    string
		want413 bool
	}{
		{"under limit", `{}`, false},
		{"over limit", `{"message":{"role":"user","content":"exceeds the cap"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/sessions/abc/messages", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			if tc.want413 {
				assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
			} else {
				// A body under the limit reaches the handler; addMessage returns
				// 400 for an empty message before it touches the SessionManager.
				assert.Equal(t, http.StatusBadRequest, rec.Code)
			}
		})
	}
}

// TestServer_WithMaxRequestBytesZeroFallback verifies that zero and negative
// values fall back to the 1 MiB default. A body just over 1 MiB must still
// trigger 413 even when WithMaxRequestBytes received 0 or -1.
func TestServer_WithMaxRequestBytesZeroFallback(t *testing.T) {
	t.Parallel()

	for _, n := range []int64{0, -1} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			srv := NewWithManager(nil, "", WithMaxRequestBytes(n))
			// A body over the default 1 MiB cap must still be rejected.
			body := bytes.Repeat([]byte("a"), int(defaultMaxRequestBytes)+1)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/sessions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		})
	}
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
	srv, err := New(ctx, store, &runConfig, 0, sources, "", 0)
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

// TestServer_CreateSessionWorkingDirWithSeparators pins the removal of a
// Copilot-autofix validation that rejected any working_dir containing path
// separators or an absolute path: POST /api/sessions must accept a real
// host directory (which always contains separators) and store it on the
// session. Only a raw working_dir containing ".." is rejected up front.
// Containment stays opt-in via WithSessionWorkingDirRoot (CodeQL alert
// #57); the unrestricted default is intentional (#3788).
func TestServer_CreateSessionWorkingDirWithSeparators(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")

	wd := t.TempDir()
	require.True(t, strings.ContainsAny(wd, `/\`))

	body, err := json.Marshal(map[string]any{"working_dir": wd})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created session.Session
	unmarshal(t, rec.Body.Bytes(), &created)
	require.NotEmpty(t, created.ID)
	assert.Equal(t, wd, created.WorkingDir)

	stored, err := sm.GetSession(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, wd, stored.WorkingDir)
}

// TestServer_CreateSessionWorkingDirDotDotRejected pins the HTTP mapping of
// the raw ".." rejection: POST /api/sessions with a traversal-carrying
// working_dir must answer 400 (ErrInvalidWorkingDir), not 500, and create
// no session.
func TestServer_CreateSessionWorkingDirDotDotRejected(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")

	wd := t.TempDir() + string(filepath.Separator) + ".."
	body, err := json.Marshal(map[string]any{"working_dir": wd})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var errResp struct {
		Message string `json:"message"`
	}
	unmarshal(t, rec.Body.Bytes(), &errResp)
	assert.Contains(t, errResp.Message, `must not contain ".."`)

	sessions, err := sm.GetSessions(ctx)
	require.NoError(t, err)
	assert.Empty(t, sessions)
}

// TestServer_GetSessionsRace pins the data-race fix for the GET
// /api/sessions and GET /api/sessions/:id handlers (#3591): the in-memory
// store hands them live *session.Session pointers, so reading
// Title/InputTokens/OutputTokens directly races the granular store updates
// (UpdateSessionTitle/UpdateSessionTokens) a running stream issues on other
// goroutines. Both handlers must go through one TitleSnapshot() and one
// Usage() snapshot. Run with -race; the writer goroutine keeps updating for
// the whole duration of the HTTP reads. Every update stores an (n, 2n)
// token pair, so the single-snapshot invariant output == 2*input holds in
// every response regardless of scheduling.
//
// The name is kept short: it feeds t.TempDir(), which becomes a unix socket
// path bounded by sun_path (104 bytes on macOS).
func TestServer_GetSessionsRace(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithTitle("initial"))
	require.NoError(t, store.AddSession(ctx, sess))

	lnPath := startServerWithStore(t, ctx, prepareAgentsDir(t), store)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := int64(1); ; n++ {
			select {
			case <-done:
				return
			default:
			}
			if err := store.UpdateSessionTitle(ctx, sess.ID, "concurrent title"); err != nil {
				t.Errorf("UpdateSessionTitle: %v", err)
				return
			}
			if err := store.UpdateSessionTokens(ctx, sess.ID, n, 2*n, float64(n)); err != nil {
				t.Errorf("UpdateSessionTokens: %v", err)
				return
			}
		}
	})

	for range 25 {
		var sessions []api.SessionsResponse
		unmarshal(t, httpGET(t, ctx, lnPath, "/api/sessions"), &sessions)
		require.Len(t, sessions, 1)
		assert.Equal(t, sess.ID, sessions[0].ID)
		assert.Equal(t, 2*sessions[0].InputTokens, sessions[0].OutputTokens)

		var single api.SessionResponse
		unmarshal(t, httpGET(t, ctx, lnPath, "/api/sessions/"+sess.ID), &single)
		assert.Equal(t, sess.ID, single.ID)
		assert.Equal(t, 2*single.InputTokens, single.OutputTokens)
	}
	close(done)
	wg.Wait()
}

// TestServer_ForkSession exercises the POST /api/sessions/:id/fork
// endpoint end-to-end: a fork at the Nth user message must return a
// new session with the history before that message, a fork-numbered
// title, and a fresh ID. An out-of-range ordinal must be rejected with
// 400 Bad Request.
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

	// Happy path: fork before the second user message (ordinal 1).
	resp := httpDo(t, ctx, http.MethodPost, lnPath,
		"/api/sessions/"+parent.ID+"/fork",
		api.ForkSessionRequest{UserMessageIndex: 1})
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

	// Forking past the last user message (no "full clone" shortcut) must
	// return 400, not 500. This pins the sentinel-driven classification so
	// future error-message reshuffles can't silently flip the status code.
	outOfRange := httpRaw(t, ctx, http.MethodPost, lnPath,
		"/api/sessions/"+parent.ID+"/fork",
		api.ForkSessionRequest{UserMessageIndex: 99})
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
	srv, err := New(ctx, store, &runConfig, 0, sources, "", 0)
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
