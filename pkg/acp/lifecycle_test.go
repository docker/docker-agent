package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func registerTestSession(ctx context.Context, a *Agent, s *Session) (*Session, bool, error) {
	a.mu.Lock()
	if a.lifecycles == nil {
		a.lifecycles = make(map[string]*sessionLifecycle)
	}
	lifecycle := a.lifecycles[s.id]
	if lifecycle == nil {
		lifecycle = &sessionLifecycle{id: s.id}
		a.lifecycles[s.id] = lifecycle
	}
	a.mu.Unlock()
	return a.registerSessionIfAbsent(ctx, s, lifecycle)
}

func closeSessionAsync(a *Agent, ctx context.Context, sid string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := a.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(sid)})
		done <- err
	}()
	return done
}

type barrierRuntime struct {
	fakeRuntime

	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	err     error
}

func (r *barrierRuntime) Close() error {
	r.calls.Add(1)
	close(r.started)
	<-r.release
	return r.err
}

type barrierToolset struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	err     error
}

func (*barrierToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*barrierToolset) Start(context.Context) error                 { return nil }
func (s *barrierToolset) Stop(context.Context) error {
	s.calls.Add(1)
	close(s.started)
	<-s.release
	return s.err
}

func barrierTeam(t *testing.T, ts tools.ToolSet) *teamloader.LoadResult {
	t.Helper()
	root := agent.New("root", "test", agent.WithToolSets(ts), agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "lifecycle")}))
	for _, started := range root.StartToolSets(t.Context(), time.Second) {
		require.NoError(t, (<-started).Err)
	}
	return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}
}

func assertPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("operation completed before cleanup: %v", err)
	default:
	}
}

func TestCloseWaitsForRuntimeAndToolsets(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, "")
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		ts := &barrierToolset{started: make(chan struct{}), release: make(chan struct{})}
		s.rt, s.team = rt, barrierTeam(t, ts).Team
		closed := closeSessionAsync(a, t.Context(), s.id)
		<-rt.started
		repeated := closeSessionAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, closed)
		assertPending(t, repeated)
		assert.Zero(t, ts.calls.Load(), "runtime/background cleanup precedes toolset stop")
		_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.ErrorContains(t, err, "closing")
		close(rt.release)
		<-ts.started
		assertPending(t, closed)
		close(ts.release)
		require.NoError(t, <-closed)
		require.NoError(t, <-repeated)
		require.NoError(t, <-closeSessionAsync(a, t.Context(), s.id))
		assert.Equal(t, int32(1), rt.calls.Load())
		assert.Equal(t, int32(1), ts.calls.Load())
		assert.Empty(t, a.sessions)
	})
}

func TestCanceledCloseWaiterDoesNotReopenSession(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, "")
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		s.rt = rt
		ctx, cancel := context.WithCancel(t.Context())
		closed := closeSessionAsync(a, ctx, s.id)
		<-rt.started
		cancel()
		require.ErrorIs(t, <-closed, context.Canceled)
		_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.ErrorContains(t, err, "closing")
		repeated := closeSessionAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, repeated)
		close(rt.release)
		require.NoError(t, <-repeated)
		loaded, _ := loadedLifecycleTeam(t)
		a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) { return loaded, nil }
		_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.NoError(t, err)
		assert.NotSame(t, s, a.sessions[s.id])
		assert.Equal(t, int32(1), rt.calls.Load())
	})
}

func TestCloseCachesCleanupFailures(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"runtime", "toolset"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				store := session.NewInMemorySessionStore()
				sess := session.New()
				require.NoError(t, store.AddSession(t.Context(), sess))
				a := NewAgent(nil, nil, store)
				a.team = team.New()
				boom := errors.New("cleanup boom")
				rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
				ts := &barrierToolset{started: make(chan struct{}), release: make(chan struct{})}
				close(rt.release)
				close(ts.release)
				if failure == "runtime" {
					rt.err = boom
				} else {
					ts.err = boom
				}
				s := &Session{id: sess.ID, sess: sess, rt: rt, team: barrierTeam(t, ts).Team}
				_, _, err := registerTestSession(t.Context(), a, s)
				require.NoError(t, err)
				for range 2 {
					require.ErrorIs(t, <-closeSessionAsync(a, t.Context(), s.id), boom)
				}
				_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
				require.ErrorIs(t, err, boom)
				require.ErrorIs(t, a.Stop(t.Context()), boom)
				require.ErrorIs(t, a.Stop(t.Context()), boom)
				assert.Equal(t, int32(1), rt.calls.Load())
				assert.Equal(t, int32(1), ts.calls.Load())
			})
		})
	}
}

type blockingReadStore struct {
	session.Store

	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

func (s *blockingReadStore) GetSession(ctx context.Context, sid string) (*session.Session, error) {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	return s.Store.GetSession(context.WithoutCancel(ctx), sid)
}

func TestCloseCancelsAndJoinsColdSessionRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.AddSession(t.Context(), sess))
		blocked := &blockingReadStore{Store: store, started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
		a := NewAgent(nil, nil, blocked)
		a.team = team.New()
		a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
			t.Error("canceled storage read must not start a team load")
			return nil, context.Canceled
		}
		resumed := make(chan error, 1)
		go func() {
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sess.ID)})
			resumed <- err
		}()
		<-blocked.started
		closed := closeSessionAsync(a, t.Context(), sess.ID)
		<-blocked.canceled
		synctest.Wait()
		assertPending(t, closed)
		close(blocked.release)
		require.ErrorIs(t, <-resumed, context.Canceled)
		require.NoError(t, <-closed)
		assert.Empty(t, a.sessions)
		require.NoError(t, a.Stop(t.Context()))
	})
}

func TestCloseJoinsCanceledTeamConstruction(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.AddSession(t.Context(), sess))
		a := NewAgent(nil, nil, store)
		a.team = team.New()
		loaded, ts := loadedLifecycleTeam(t)
		loading, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		a.loadTeam = func(ctx context.Context, _ string) (*teamloader.LoadResult, error) {
			close(loading)
			<-ctx.Done()
			close(canceled)
			<-release
			return loaded, nil
		}
		resumed := make(chan error, 1)
		go func() {
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sess.ID)})
			resumed <- err
		}()
		<-loading
		closed := closeSessionAsync(a, t.Context(), sess.ID)
		<-canceled
		synctest.Wait()
		assertPending(t, closed)
		close(release)
		require.Error(t, <-resumed)
		require.NoError(t, <-closed)
		assert.Equal(t, int32(1), ts.stops.Load())
		assert.Empty(t, a.sessions)
		require.NoError(t, a.Stop(t.Context()))
	})
}

func TestStopCancelsInitializationOutsideAgentLock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := NewAgent(config.NewBytesSource("test", nil), nil, session.NewInMemorySessionStore())
		loaded, ts := loadedLifecycleTeam(t)
		loading, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		a.loadTeam = func(ctx context.Context, _ string) (*teamloader.LoadResult, error) {
			close(loading)
			<-ctx.Done()
			close(canceled)
			<-release
			return loaded, nil
		}
		initialized := make(chan error, 1)
		go func() {
			_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
			initialized <- err
		}()
		<-loading
		stopped := make(chan error, 1)
		go func() { stopped <- a.Stop(t.Context()) }()
		<-canceled
		synctest.Wait()
		assertPending(t, stopped)
		close(release)
		require.ErrorIs(t, <-initialized, context.Canceled)
		require.NoError(t, <-stopped)
		assert.Nil(t, a.team)
		assert.Equal(t, int32(1), ts.stops.Load())
	})
}

func TestConcurrentStopJoinsValidationTeamCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := NewAgent(nil, nil, session.NewInMemorySessionStore())
		boom := errors.New("validation stop failed")
		ts := &barrierToolset{started: make(chan struct{}), release: make(chan struct{}), err: boom}
		a.team = barrierTeam(t, ts).Team
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { first <- a.Stop(t.Context()) }()
		<-ts.started
		go func() { second <- a.Stop(t.Context()) }()
		synctest.Wait()
		assertPending(t, first)
		assertPending(t, second)
		close(ts.release)
		require.ErrorIs(t, <-first, boom)
		require.ErrorIs(t, <-second, boom)
		assert.Equal(t, int32(1), ts.calls.Load())
	})
}

func TestUnknownLifecycleRequestsDoNotAccumulate(t *testing.T) {
	t.Parallel()
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	a.team = team.New()
	for i := range 25 {
		sid := strconv.Itoa(i)
		require.NoError(t, <-closeSessionAsync(a, t.Context(), sid))
		_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sid)})
		var rpcErr *acpsdk.RequestError
		require.ErrorAs(t, err, &rpcErr)
		assert.Equal(t, -32002, rpcErr.Code)
	}
	assert.Empty(t, a.lifecycles)
	require.NoError(t, a.Stop(t.Context()))
}

func TestLosingResumeCleansCandidateBeforeReplacingRoots(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := session.NewInMemorySessionStore()
				sess := session.New()
				require.NoError(t, store.AddSession(t.Context(), sess))
				a := NewAgent(nil, nil, store)
				a.team = team.New()
				oldRoot := t.TempDir()
				winner := &Session{id: sess.ID, sess: sess, rt: &fakeRuntime{}, additionalDirs: []string{oldRoot}}
				ts := &barrierToolset{started: make(chan struct{}), release: make(chan struct{})}
				if fail {
					ts.err = errors.New("loser cleanup failed")
				}
				candidate := barrierTeam(t, ts)
				a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
					_, _, err := registerTestSession(t.Context(), a, winner)
					return candidate, err
				}
				resumed := make(chan error, 1)
				go func() {
					_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sess.ID)})
					resumed <- err
				}()
				<-ts.started
				_, roots := winner.workspaceSnapshot()
				assert.Equal(t, []string{oldRoot}, roots)
				_, finish, err := winner.startTurn(t.Context())
				require.NoError(t, err)
				close(ts.release)
				err = <-resumed
				if fail {
					require.ErrorIs(t, err, ts.err)
				} else {
					require.ErrorContains(t, err, "running or pending")
				}
				_, roots = winner.workspaceSnapshot()
				assert.Equal(t, []string{oldRoot}, roots)
				finish()
				err = a.Stop(t.Context())
				if fail {
					require.ErrorIs(t, err, ts.err)
					assert.Equal(t, 1, strings.Count(err.Error(), "loser cleanup failed"))
				} else {
					require.NoError(t, err)
				}
			})
		})
	}
}

func TestCloseCancelsQueuedLoadWithoutStartingIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.AddSession(t.Context(), sess))
		a := NewAgent(nil, nil, store)
		a.team = team.New()
		a.loadGate <- struct{}{}
		a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
			t.Error("queued canceled load must not start")
			return nil, context.Canceled
		}
		resumed := make(chan error, 1)
		go func() {
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sess.ID)})
			resumed <- err
		}()
		synctest.Wait()
		closed := closeSessionAsync(a, t.Context(), sess.ID)
		require.ErrorIs(t, <-resumed, context.Canceled)
		require.NoError(t, <-closed)
		<-a.loadGate
		assert.Empty(t, a.sessions)
		require.NoError(t, a.Stop(t.Context()))
	})
}

type listingStore struct {
	session.Store

	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	finished atomic.Bool
	closed   atomic.Bool
	calls    atomic.Int32
}

func (s *listingStore) GetSessionSummaries(ctx context.Context) ([]session.Summary, error) {
	s.calls.Add(1)
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	if s.closed.Load() {
		return nil, errors.New("session store closed before request completed")
	}
	s.finished.Store(true)
	return nil, ctx.Err()
}

func (s *listingStore) Close() error {
	if !s.finished.Load() {
		return errors.New("store closed before admitted listing completed")
	}
	s.closed.Store(true)
	return s.Store.Close()
}

func TestStopDrainsStoreUsersBeforeOwnerClosesStore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &listingStore{Store: session.NewInMemorySessionStore(), started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
		a := NewAgent(nil, nil, store)
		listed := make(chan error, 1)
		go func() {
			_, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
			listed <- err
		}()
		<-store.started
		stopped := make(chan error, 1)
		go func() { stopped <- a.Stop(t.Context()) }()
		<-store.canceled
		synctest.Wait()
		assertPending(t, stopped)
		close(store.release)
		require.ErrorIs(t, <-listed, context.Canceled)
		require.NoError(t, <-stopped)
		require.NoError(t, store.Close())
		_, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
		require.ErrorContains(t, err, "agent stopped")
		assert.Equal(t, int32(1), store.calls.Load())
	})
}

func TestCloseResponseWaitsOnWire(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		a, s := newResumeFixture(t, "")
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		s.rt = rt
		input, send := io.Pipe()
		out := &captureWriter{}
		conn := acpsdk.NewAgentSideConnection(a, out, input)
		t.Cleanup(func() { _ = send.Close(); <-conn.Done() })
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/close", "params": map[string]any{"sessionId": s.id}})
		require.NoError(t, err)
		_, err = send.Write(append(data, '\n'))
		require.NoError(t, err)
		<-rt.started
		synctest.Wait()
		assert.Empty(t, out.lines(), "wire success cannot precede runtime cleanup")
		close(rt.release)
		synctest.Wait()
		require.Len(t, out.lines(), 1)
		assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{}}`, out.lines()[0])
	})
}

type startingToolset struct {
	started chan struct{}
	release chan struct{}
	stops   atomic.Int32
	err     error
}

func (*startingToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *startingToolset) Start(context.Context) error {
	close(s.started)
	<-s.release
	return nil
}

func (s *startingToolset) Stop(context.Context) error {
	s.stops.Add(1)
	return s.err
}

func TestCloseRetainsPendingToolsetStopFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.AddSession(t.Context(), sess))
		a := NewAgent(nil, nil, store)
		a.team = team.New()
		boom := errors.New("late stop failed")
		ts := &startingToolset{started: make(chan struct{}), release: make(chan struct{}), err: boom}
		root := agent.New("root", "", agent.WithToolSets(ts))
		s := &Session{id: sess.ID, sess: sess, rt: &fakeRuntime{}, team: team.New(team.WithAgents(root))}
		_, _, err := registerTestSession(t.Context(), a, s)
		require.NoError(t, err)
		starts := root.StartToolSets(t.Context(), time.Minute)
		<-ts.started
		closed := closeSessionAsync(a, t.Context(), s.id)
		synctest.Wait()
		assertPending(t, closed)
		close(ts.release)
		for _, started := range starts {
			require.NoError(t, (<-started).Err)
		}
		require.ErrorIs(t, <-closed, boom)
		require.ErrorIs(t, <-closeSessionAsync(a, t.Context(), s.id), boom)
		_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, a.Stop(t.Context()), boom)
		assert.Equal(t, int32(1), ts.stops.Load())
	})
}

type backgroundCloseProvider struct {
	mockProvider

	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (p *backgroundCloseProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	close(p.started)
	<-ctx.Done()
	close(p.canceled)
	<-p.release
	return nil, ctx.Err()
}

type backgroundLaunchProvider struct {
	mockProvider

	calls int
}

func (p *backgroundLaunchProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.calls++
	choice := chat.MessageStreamChoice{Delta: chat.MessageDelta{Content: "dispatched"}, FinishReason: chat.FinishReasonStop}
	if p.calls == 1 {
		choice = chat.MessageStreamChoice{
			Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
				ID: "background", Type: "function", Function: tools.FunctionCall{Name: "run_background_agent", Arguments: `{"agent":"worker","task":"wait"}`},
			}}},
			FinishReason: chat.FinishReasonToolCalls,
		}
	}
	return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{choice}}}}, nil
}

func TestCloseCancelsAndJoinsRealBackgroundAgent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		workerProvider := &backgroundCloseProvider{
			mockProvider: mockProvider{id: modelsdev.NewID("test", "worker")},
			started:      make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
		}
		worker := agent.New("worker", "test", agent.WithModel(workerProvider))
		rootProvider := &backgroundLaunchProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "root")}}
		root := agent.New("root", "test", agent.WithModel(rootProvider), agent.WithSubAgents(worker), agent.WithToolSets(agenttool.New()))
		tm := team.New(team.WithAgents(root, worker))
		rt, err := runtime.New(t.Context(), tm, runtime.WithSessionCompaction(false))
		require.NoError(t, err)
		a, s, _ := newPromptTestAgent(t, rt)
		s.team = tm
		s.sess.ToolsApproved = true
		_, err = a.Prompt(t.Context(), promptRequest("dispatch work"))
		require.NoError(t, err)
		<-workerProvider.started
		closed := closeSessionAsync(a, t.Context(), s.id)
		<-workerProvider.canceled
		synctest.Wait()
		assertPending(t, closed)
		close(workerProvider.release)
		require.NoError(t, <-closed)
		require.NoError(t, a.Stop(t.Context()))
	})
}

func TestCloseToolsetTimeoutDoesNotPermitResume(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.AddSession(t.Context(), sess))
		a := NewAgent(nil, nil, store)
		a.team = team.New()
		ts := &startingToolset{started: make(chan struct{}), release: make(chan struct{})}
		root := agent.New("root", "", agent.WithToolSets(ts))
		s := &Session{id: sess.ID, sess: sess, rt: &fakeRuntime{}, team: team.New(team.WithAgents(root))}
		_, _, err := registerTestSession(t.Context(), a, s)
		require.NoError(t, err)
		starts := root.StartToolSets(t.Context(), time.Minute)
		<-ts.started
		require.ErrorIs(t, <-closeSessionAsync(a, t.Context(), s.id), context.DeadlineExceeded)
		close(ts.release)
		for _, started := range starts {
			require.NoError(t, (<-started).Err)
		}
		require.ErrorIs(t, <-closeSessionAsync(a, t.Context(), s.id), context.DeadlineExceeded)
		_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, a.Stop(t.Context()), context.DeadlineExceeded)
		assert.Equal(t, int32(1), ts.stops.Load())
	})
}
