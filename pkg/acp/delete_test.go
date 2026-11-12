package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func deleteAsync(a *Agent, ctx context.Context, sid string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := a.UnstableDeleteSession(ctx, acpsdk.UnstableDeleteSessionRequest{SessionId: acpsdk.SessionId(sid)})
		done <- err
	}()
	return done
}

type deletionStore struct {
	session.Store

	started, release chan struct{}
	calls            atomic.Int32
	failure          error
}

func (s *deletionStore) DeleteSession(ctx context.Context, id string) error {
	s.calls.Add(1)
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	if s.failure != nil {
		return s.failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.DeleteSession(ctx, id)
}

func TestDeleteSessionSQLiteTreeAndIdempotency(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	wd := t.TempDir()
	file := filepath.Join(wd, "keep.txt")
	require.NoError(t, os.WriteFile(file, []byte("workspace"), 0o600))
	root := session.New(session.WithWorkingDir(wd), session.WithUserMessage("root history"))
	require.NoError(t, store.AddSession(t.Context(), root))
	child := session.New(session.WithParentID(root.ID), session.WithWorkingDir(wd))
	require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
	a := NewAgent(nil, &config.RuntimeConfig{}, store)
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		loaded, _ := loadedLifecycleTeam(t)
		return loaded, nil
	}
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	out := &captureWriter{}
	captureReplay(t, a, out)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(root.ID), Cwd: wd})
	require.NoError(t, err)
	for range 2 {
		_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: acpsdk.SessionId(root.ID)})
		require.NoError(t, err)
	}
	for _, id := range []string{root.ID, child.ID} {
		_, err = store.GetSession(t.Context(), id)
		require.ErrorIs(t, err, session.ErrNotFound)
	}
	listed, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.NoError(t, err)
	assert.Empty(t, listed.Sessions)
	_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(root.ID), Cwd: wd})
	require.Error(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(root.ID), Cwd: wd})
	require.Error(t, err)
	require.FileExists(t, file)
}

func TestDeleteWaitsForDrainAndBlocksReconnectionThroughStoreCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		s.rt = rt
		store := &deletionStore{Store: a.sessionStore, started: make(chan struct{}), release: make(chan struct{})}
		a.sessionStore = store
		deleted := deleteAsync(a, t.Context(), s.id)
		<-rt.started
		synctest.Wait()
		assertPending(t, deleted)
		assert.Zero(t, store.calls.Load())
		close(rt.release)
		<-store.started
		_, err := a.LoadSession(t.Context(), loadRequest(s))
		require.ErrorContains(t, err, "deletion in progress")
		_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir})
		require.ErrorContains(t, err, "deletion in progress")
		_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: s.workingDir})
		require.ErrorContains(t, err, "deletion in progress")
		joined := deleteAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, joined)
		_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: "other"})
		require.ErrorContains(t, err, "another session deletion")
		close(store.release)
		require.NoError(t, <-deleted)
		require.NoError(t, <-joined)
		assert.Equal(t, int32(1), store.calls.Load())
		_, err = store.GetSession(t.Context(), s.id)
		require.ErrorIs(t, err, session.ErrNotFound)
	})
}

func TestDeleteCancelsPromptAndDrainsBeforeDeletion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		rt := &drainingPromptRuntime{started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
		s.rt = rt
		out := &captureWriter{}
		captureReplay(t, a, out)
		store := &deletionStore{Store: a.sessionStore}
		a.sessionStore = store
		prompted := make(chan error, 1)
		go func() {
			_, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(s.id), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("work")}})
			prompted <- err
		}()
		<-rt.started
		deleted := deleteAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, deleted)
		assert.Zero(t, store.calls.Load())
		close(rt.release)
		require.NoError(t, <-prompted)
		require.NoError(t, <-deleted)
		assert.Equal(t, int32(1), store.calls.Load())
	})
}

func TestDeleteCanceledOwnerDrainsButRetainsHistory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		s.rt = rt
		store := &deletionStore{Store: a.sessionStore}
		a.sessionStore = store
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		deleted := deleteAsync(a, ctx, s.id)
		<-rt.started
		cancel()
		require.ErrorIs(t, <-deleted, context.Canceled)
		_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir})
		require.ErrorContains(t, err, "deletion in progress")
		close(rt.release)
		synctest.Wait()
		assert.Zero(t, store.calls.Load())
		_, err = store.GetSession(t.Context(), s.id)
		require.NoError(t, err)
		require.NoError(t, <-deleteAsync(a, t.Context(), s.id))
	})
}

func TestDeleteWaiterCancellationDoesNotCancelOwner(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		store := &deletionStore{Store: a.sessionStore, started: make(chan struct{}), release: make(chan struct{})}
		a.sessionStore = store
		owner := deleteAsync(a, t.Context(), s.id)
		<-store.started
		ctx, cancel := context.WithCancel(t.Context())
		waiter := deleteAsync(a, ctx, s.id)
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-waiter, context.Canceled)
		close(store.release)
		require.NoError(t, <-owner)
		_, err := store.GetSession(t.Context(), s.id)
		require.ErrorIs(t, err, session.ErrNotFound)
	})
}

func TestStopJoinsDeletionStoreCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		store := &deletionStore{Store: a.sessionStore, started: make(chan struct{}), release: make(chan struct{})}
		a.sessionStore = store
		deleted := deleteAsync(a, t.Context(), s.id)
		<-store.started
		stopped := make(chan error, 1)
		go func() { stopped <- a.Stop(t.Context()) }()
		synctest.Wait()
		assertPending(t, stopped)
		close(store.release)
		require.ErrorIs(t, <-deleted, context.Canceled)
		require.NoError(t, <-stopped)
		_, err := store.GetSession(t.Context(), s.id)
		require.NoError(t, err)
	})
}

func TestDeleteFailuresPreserveHistory(t *testing.T) {
	t.Parallel()
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "store", true: "cleanup"}[cleanup], func(t *testing.T) {
			persisted := session.New(session.WithWorkingDir(t.TempDir()))
			a := NewAgent(nil, nil, session.NewInMemorySessionStore())
			a.team = team.New()
			require.NoError(t, a.sessionStore.AddSession(t.Context(), persisted))
			s := &Session{id: persisted.ID, sess: persisted, rt: &fakeRuntime{}}
			_, registered, err := registerTestSession(t.Context(), a, s)
			require.NoError(t, err)
			require.True(t, registered)
			defer func() {
				err := a.Stop(t.Context())
				if cleanup {
					require.ErrorContains(t, err, "cleanup failed")
				} else {
					require.NoError(t, err)
				}
			}()
			store := &deletionStore{Store: a.sessionStore}
			a.sessionStore = store
			if cleanup {
				rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{}), err: errors.New("cleanup failed")}
				close(rt.release)
				s.rt = rt
			} else {
				store.failure = errors.New("store failed")
			}
			_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: acpsdk.SessionId(s.id)})
			require.Error(t, err)
			_, err = store.GetSession(t.Context(), s.id)
			require.NoError(t, err)
			if cleanup {
				assert.Zero(t, store.calls.Load())
				require.ErrorContains(t, <-deleteAsync(a, t.Context(), s.id), "cleanup failed")
			} else {
				store.failure = nil
				require.NoError(t, <-deleteAsync(a, t.Context(), s.id))
			}
		})
	}
}

func TestDeleteRejectsLoadedDescendantsAndDirectChildren(t *testing.T) {
	t.Parallel()
	a, root := newResumeFixture(t, t.TempDir())
	child := session.New(session.WithParentID(root.id), session.WithWorkingDir(root.workingDir))
	grandchild := session.New(session.WithParentID(child.ID), session.WithWorkingDir(root.workingDir))
	require.NoError(t, a.sessionStore.AddSession(t.Context(), child))
	require.NoError(t, a.sessionStore.AddSession(t.Context(), grandchild))
	loaded := &Session{id: grandchild.ID, sess: grandchild, workingDir: root.workingDir, rt: &fakeRuntime{}}
	_, ok, err := registerTestSession(t.Context(), a, loaded)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorContains(t, <-deleteAsync(a, t.Context(), root.id), "descendant")
	require.ErrorContains(t, <-deleteAsync(a, t.Context(), child.ID), "root session")
	assert.Same(t, root, a.sessions[root.id])
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(grandchild.ID)})
	require.NoError(t, err)
	require.NoError(t, <-deleteAsync(a, t.Context(), root.id))
}

func TestDeleteCancelsSameSessionConstructor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.NoError(t, err)
		entered, release := make(chan struct{}), make(chan struct{})
		loaded, ts := loadedLifecycleTeam(t)
		a.loadTeam = func(ctx context.Context, _ string) (*teamloader.LoadResult, error) {
			close(entered)
			<-release
			return loaded, nil
		}
		resumed := make(chan error, 1)
		go func() {
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir})
			resumed <- err
		}()
		<-entered
		deleted := deleteAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, deleted)
		close(release)
		require.Error(t, <-resumed)
		require.NoError(t, <-deleted)
		assert.Equal(t, int32(1), ts.stops.Load())
		assert.Empty(t, a.sessions)
	})
}

func TestDeleteRejectsUnknownConcurrentConstructor(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	_, op, err := a.beginSessionConstruction(t.Context(), "possible-child")
	require.NoError(t, err)
	_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.ErrorContains(t, err, "construction in progress")
	a.finishOperation(op)
	require.NoError(t, <-deleteAsync(a, t.Context(), s.id))
}

func TestDeleteWireAndCapability(t *testing.T) {
	t.Parallel()
	a := NewAgent(config.NewBytesSource("test.yaml", nil), &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		loaded, _ := loadedLifecycleTeam(t)
		return loaded, nil
	}
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	response, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.NoError(t, err)
	assert.NotNil(t, response.AgentCapabilities.SessionCapabilities.Delete)
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := acpsdk.NewAgentSideConnection(a, output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, id := range []string{"", "missing", "missing"} {
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": "session/delete", "params": map[string]any{"sessionId": id}}))
		var result struct {
			ID     int                  `json:"id"`
			Result json.RawMessage      `json:"result"`
			Error  *acpsdk.RequestError `json:"error"`
		}
		require.NoError(t, decoder.Decode(&result))
		assert.Equal(t, i, result.ID)
		if id == "" {
			require.NotNil(t, result.Error)
			assert.Equal(t, -32602, result.Error.Code)
		} else {
			require.Nil(t, result.Error)
			assert.JSONEq(t, `{}`, string(result.Result))
		}
	}
}

func TestDeleteIgnoresFailedUnpersistedNewRoot(t *testing.T) {
	t.Parallel()
	a := NewAgent(nil, nil, failingAddStore{session.NewInMemorySessionStore()})
	a.team = team.New()
	wd := t.TempDir()
	saved := session.New(session.WithWorkingDir(wd))
	require.NoError(t, a.sessionStore.(failingAddStore).Store.AddSession(t.Context(), saved))
	ts := &barrierToolset{started: make(chan struct{}), release: make(chan struct{}), err: errors.New("cleanup failed")}
	close(ts.release)
	root := agent.New("root", "test", agent.WithToolSets(ts), agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "delete")}))
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
	}
	for _, started := range root.StartToolSets(t.Context(), 0) {
		require.NoError(t, (<-started).Err)
	}
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	require.ErrorContains(t, err, "store failed")
	require.ErrorContains(t, err, "cleanup failed")
	require.NoError(t, <-deleteAsync(a, t.Context(), saved.ID))
	require.NoError(t, <-deleteAsync(a, t.Context(), "never existed"))
	require.ErrorContains(t, a.Stop(t.Context()), "cleanup failed", "original cleanup failure remains sticky")
}

func TestDeleteJoinsLoadReplay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		s.sess.AddMessage(session.UserMessage("history"))
		entered, release := make(chan struct{}), make(chan struct{})
		out := &captureWriter{failOn: func(n int) error {
			if n == 1 {
				close(entered)
				<-release
			}
			return nil
		}}
		captureReplay(t, a, out)
		store := &deletionStore{Store: a.sessionStore}
		a.sessionStore = store
		loaded := make(chan error, 1)
		go func() { _, err := a.LoadSession(t.Context(), loadRequest(s)); loaded <- err }()
		<-entered
		deleted := deleteAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, deleted)
		assert.Zero(t, store.calls.Load())
		close(release)
		require.Error(t, <-loaded)
		require.NoError(t, <-deleted)
		assert.Equal(t, int32(1), store.calls.Load())
	})
}

func TestDeleteRejectsClosingDescendant(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, t.TempDir())
		child := session.New(session.WithParentID(s.id), session.WithWorkingDir(s.workingDir))
		require.NoError(t, a.sessionStore.AddSession(t.Context(), child))
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		owned := &Session{id: child.ID, sess: child, workingDir: s.workingDir, rt: rt}
		_, ok, err := registerTestSession(t.Context(), a, owned)
		require.NoError(t, err)
		require.True(t, ok)
		closed := closeSessionAsync(a, t.Context(), child.ID)
		<-rt.started
		require.ErrorContains(t, <-deleteAsync(a, t.Context(), s.id), "descendant")
		close(rt.release)
		require.NoError(t, <-closed)
		require.NoError(t, <-deleteAsync(a, t.Context(), s.id))
	})
}

func TestDeleteStopsClientMCPBackgroundWork(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		worker := agent.New("worker", "test", agent.WithModel(&clientMCPWaitProvider{mockProvider{id: modelsdev.NewID("test", "worker")}}))
		root := agent.New("root", "test", agent.WithModel(&clientMCPBackgroundProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "root")}}), agent.WithSubAgents(worker), agent.WithToolSets(agenttool.New()))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root, worker))}, nil
	}
	markers, wd := t.TempDir(), t.TempDir()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a.sessionStore = store
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	out := &captureWriter{}
	captureReplay(t, a, out)
	spec := clientServerSpec(t, "background", "value")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_CALL_STARTED", Value: filepath.Join(markers, "called")}, acpsdk.EnvVariable{Name: "ACP_STOPPED", Value: filepath.Join(markers, "stopped")})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	rootSession := a.sessions[string(created.SessionId)].sess
	rootSession.SetToolsApproved(true)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	_, err = a.Prompt(ctx, acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("dispatch")}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(markers, "called")); return err == nil }, 5*time.Second, time.Millisecond)
	_, err = a.UnstableDeleteSession(ctx, acpsdk.UnstableDeleteSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(markers, "stopped"))
	all, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, all)
	var childIDs []string
	for _, item := range rootSession.MessagesSnapshot() {
		if item.SubSession != nil {
			childIDs = append(childIDs, item.SubSession.ID)
		}
	}
	require.NotEmpty(t, childIDs, "background child must finish attaching before delete returns")
	for _, id := range childIDs {
		_, err := store.GetSession(t.Context(), id)
		require.ErrorIs(t, err, session.ErrNotFound)
	}
}
