package board

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSessions struct{}

func (fakeSessions) NewSession(_, _, _, _, _, _, _, _ string) error { return nil }
func (fakeSessions) KillSession(string) error                       { return nil }
func (fakeSessions) Alive(string) (bool, error)                     { return true, nil }
func (fakeSessions) Exists(string) (bool, error)                    { return true, nil }

// fakeClient replays a scripted event stream, then blocks like a live
// connection until the watcher is cancelled.
type fakeClient struct {
	snap   snapshot
	events []event
}

func (f *fakeClient) Snapshot(context.Context) (snapshot, error) { return f.snap, nil }

func (f *fakeClient) Followup(context.Context, string, string) error { return nil }

func (f *fakeClient) StreamEvents(ctx context.Context, _ uint64, onEvent func(event) bool) error {
	for _, ev := range f.events {
		if !onEvent(ev) {
			return nil
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// watchCard spins up a controller whose client replays the given events for
// a fresh card, and returns the store to observe the mirrored state.
func watchCard(t *testing.T, snap snapshot, events []event) (*Store, *controller) {
	t.Helper()
	store := testStore(t)
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Title: "Task", Column: "dev", Status: StatusStarting}))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	c := newController(ctx, store, fakeSessions{}, func() {})
	c.clientFor = func(_, _ string) sessionClient {
		return &fakeClient{snap: snap, events: events}
	}
	card, err := store.GetCard("c1")
	require.NoError(t, err)
	c.Start(card)
	t.Cleanup(func() { c.Stop("c1") })
	return store, c
}

func waitForStatus(t *testing.T, store *Store, want CardStatus) {
	t.Helper()
	assert.Eventually(t, func() bool {
		card, err := store.GetCard("c1")
		return err == nil && card.Status == want
	}, 10*time.Second, 10*time.Millisecond, "expected status %s", want)
}

func TestControllerRunningThenWaiting(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store, _ := watchCard(t, snapshot{}, []event{
			{Type: eventUserMessage},
			{Type: eventStreamStarted},
			{Type: eventStreamStopped, Reason: reasonNormal},
		})
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusWaiting, card.Status)
	})
}

func TestControllerExpectedTurnSkipsReadyFlash(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// A fresh card launches with an initial prompt: the control plane
		// answers before the first event, but the card must not flash "ready"
		// before its first turn (starting → running → ready).
		store := testStore(t)
		require.NoError(t, store.InsertCard(&Card{ID: "c1", Title: "Task", Column: "dev", Status: StatusStarting}))

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		c := newController(ctx, store, fakeSessions{}, func() {})
		c.clientFor = func(_, _ string) sessionClient {
			return &fakeClient{}
		}
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		c.ExpectTurn("c1")
		c.Start(card)
		t.Cleanup(func() { c.Stop("c1") })

		// With no events yet, the card stays "starting" instead of "ready".
		assert.Never(t, func() bool {
			card, err := store.GetCard("c1")
			return err == nil && card.Status != StatusStarting
		}, 300*time.Millisecond, 10*time.Millisecond)
	})
}

func TestControllerExpectedTurnRunsThenWaits(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := testStore(t)
		require.NoError(t, store.InsertCard(&Card{ID: "c1", Title: "Task", Column: "dev", Status: StatusStarting}))

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		c := newController(ctx, store, fakeSessions{}, func() {})
		c.clientFor = func(_, _ string) sessionClient {
			return &fakeClient{events: []event{
				{Type: eventUserMessage},
				{Type: eventStreamStarted},
				{Type: eventStreamStopped, Reason: reasonNormal},
			}}
		}
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		c.ExpectTurn("c1")
		c.Start(card)
		t.Cleanup(func() { c.Stop("c1") })

		// The first turn runs and completes: only then is the card ready.
		synctest.Wait()
		card, err = store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusWaiting, card.Status)
		assert.False(t, c.turnExpected("c1"), "expectation should be cleared by the first turn")
	})
}

func TestControllerStaysRunningWithNestedStreams(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store, _ := watchCard(t, snapshot{}, []event{
			{Type: eventStreamStarted},
			{Type: eventStreamStarted}, // sub-agent
			{Type: eventStreamStopped, Reason: reasonNormal},
		})
		// The outer stream is still open: the card stays running.
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusRunning, card.Status)
	})
}

func TestControllerErrorIsSticky(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store, _ := watchCard(t, snapshot{}, []event{
			{Type: eventStreamStarted},
			{Type: eventError},
			{Type: eventStreamStopped, Reason: "error"},
		})
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusError, card.Status)
	})
}

func TestControllerNormalStopClearsSubAgentError(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// A sub-agent error the parent recovered from: the outermost stop's
		// "normal" reason is authoritative.
		store, _ := watchCard(t, snapshot{}, []event{
			{Type: eventStreamStarted},
			{Type: eventError},
			{Type: eventStreamStopped, Reason: reasonNormal},
		})
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusWaiting, card.Status)
	})
}

func TestControllerPause(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store, _ := watchCard(t, snapshot{}, []event{
			{Type: eventStreamStarted},
			{Type: eventRuntimePaused},
		})
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusPaused, card.Status)
	})
}

func TestControllerReplayAppliesFinalStatusOnly(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// Replayed history contains a long-resolved error: only the state at the
		// snapshot's seq lands in the store.
		store, _ := watchCard(t, snapshot{LastEventSeq: 3}, []event{
			{Type: eventStreamStarted, Seq: 1},
			{Type: eventError, Seq: 2},
			{Type: eventStreamStopped, Reason: reasonNormal, Seq: 3},
		})
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusWaiting, card.Status)
	})
}

func TestControllerTitleFromSnapshot(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store, _ := watchCard(t, snapshot{Title: "Real title"}, nil)
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, "Real title", card.Title)
	})
}

// recordingSessions counts session creations so tests can assert whether a
// relaunch really happened.
type recordingSessions struct {
	alive       bool
	newSessions int
}

func (r *recordingSessions) NewSession(_, _, _, _, _, _, _, _ string) error {
	r.newSessions++
	return nil
}
func (r *recordingSessions) KillSession(string) error    { return nil }
func (r *recordingSessions) Alive(string) (bool, error)  { return r.alive, nil }
func (r *recordingSessions) Exists(string) (bool, error) { return r.alive, nil }

func TestRelaunchAbortsForDeletedCard(t *testing.T) {
	t.Parallel()

	sessions := &recordingSessions{}
	c := newController(t.Context(), testStore(t), sessions, func() {})

	err := c.relaunch(&Card{ID: "gone", Session: "s"}, "prompt")
	require.ErrorIs(t, err, ErrCardNotFound)
	assert.Zero(t, sessions.newSessions)
}

func TestRelaunchSkipsResurrectedSession(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Session: "s"}))
	sessions := &recordingSessions{alive: true}
	c := newController(t.Context(), store, sessions, func() {})
	card, err := store.GetCard("c1")
	require.NoError(t, err)

	// A plain resume backs off when the session is already alive again…
	require.NoError(t, c.relaunch(card, ""))
	assert.Zero(t, sessions.newSessions)

	// …but a prompt-bearing relaunch proceeds: its prompt must be delivered.
	require.NoError(t, c.relaunch(card, "do it"))
	assert.Equal(t, 1, sessions.newSessions)
}

func TestControllerStopWaits(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store, c := watchCard(t, snapshot{}, []event{{Type: eventStreamStarted}})
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusRunning, card.Status)

		c.Stop("c1")
		// Stopping twice (or a never-watched card) is safe.
		c.Stop("c1")
		c.Stop("unknown")
	})
}

// downClient simulates an agent whose control plane never comes up.
type downClient struct{}

func (downClient) Snapshot(context.Context) (snapshot, error) {
	return snapshot{}, errors.New("connection refused")
}

func (downClient) StreamEvents(context.Context, uint64, func(event) bool) error {
	return errors.New("connection refused")
}

func (downClient) Followup(context.Context, string, string) error {
	return errors.New("connection refused")
}

// crashingSessions simulates an agent that dies at startup: the session is
// never alive, and creating a new one optionally fails too.
type crashingSessions struct {
	newErr      error
	newSessions atomic.Int32
}

func (s *crashingSessions) NewSession(_, _, _, _, _, _, _, _ string) error {
	s.newSessions.Add(1)
	return s.newErr
}
func (s *crashingSessions) KillSession(string) error    { return nil }
func (s *crashingSessions) Alive(string) (bool, error)  { return false, nil }
func (s *crashingSessions) Exists(string) (bool, error) { return false, nil }

// watchCrashingCard spins up a watcher for a card whose agent never answers
// and whose tmux pane is dead, simulating a startup crash.
func watchCrashingCard(t *testing.T, sessions *crashingSessions) (*Store, *controller) {
	t.Helper()
	store := testStore(t)
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Column: "dev", Status: StatusStarting, Session: "s", Worktree: t.TempDir()}))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	c := newController(ctx, store, sessions, func() {})
	c.clientFor = func(_, _ string) sessionClient { return downClient{} }
	card, err := store.GetCard("c1")
	require.NoError(t, err)
	c.Start(card)
	t.Cleanup(func() { c.Stop("c1") })
	return store, c
}

func TestControllerStartupCrashLoopGoesRed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The agent dies before its control plane ever answers, relaunch after
		// relaunch: the watcher must surface the failure instead of silently
		// relaunching forever with the card stuck "starting".
		sessions := &crashingSessions{}
		store, c := watchCrashingCard(t, sessions)
		waitForStatus(t, store, StatusError)
		// Relaunches stop at the cap, preserving the dead pane's error output.
		assert.Equal(t, int32(maxLaunchFailures-1), sessions.newSessions.Load())
		// The relaunches themselves worked: the dead pane is the record, not a
		// launch error.
		assert.NoError(t, c.LaunchError("c1"))
	})
}

func TestControllerFailedRelaunchGoesRed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The session cannot even be recreated (e.g. tmux new-session fails):
		// the card must go red, not stay "starting" forever, and the failure
		// must be recorded — there is no pane left to read it from.
		sessions := &crashingSessions{newErr: errors.New("tmux: bad working directory")}
		store, c := watchCrashingCard(t, sessions)
		synctest.Wait()
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusError, card.Status)
		assert.ErrorContains(t, c.LaunchError("c1"), "bad working directory")
	})
}

// flakyClient fails its first snapshots, then behaves like a healthy agent
// replaying the given events.
type flakyClient struct {
	failures atomic.Int32
	healthy  fakeClient
}

func (f *flakyClient) Snapshot(ctx context.Context) (snapshot, error) {
	if f.failures.Add(-1) >= 0 {
		return snapshot{}, errors.New("connection refused")
	}
	return f.healthy.Snapshot(ctx)
}

func (f *flakyClient) StreamEvents(ctx context.Context, since uint64, onEvent func(event) bool) error {
	return f.healthy.StreamEvents(ctx, since, onEvent)
}

func (f *flakyClient) Followup(ctx context.Context, key, msg string) error {
	return f.healthy.Followup(ctx, key, msg)
}

func TestControllerCrashCapToleratesTransientFailures(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The agent dies fewer times than the cap before its control plane
		// answers: the card must recover, not go red.
		store := testStore(t)
		require.NoError(t, store.InsertCard(&Card{ID: "c1", Column: "dev", Status: StatusStarting, Session: "s", Worktree: t.TempDir()}))

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		client := &flakyClient{healthy: fakeClient{events: []event{
			{Type: eventUserMessage},
			{Type: eventStreamStarted},
			{Type: eventStreamStopped, Reason: reasonNormal},
		}}}
		client.failures.Store(int32(maxLaunchFailures - 1))

		sessions := &crashingSessions{}
		c := newController(ctx, store, sessions, func() {})
		c.clientFor = func(_, _ string) sessionClient { return client }
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		c.Start(card)
		t.Cleanup(func() { c.Stop("c1") })

		waitForStatus(t, store, StatusWaiting)
		// The successful snapshot reset the count: a later death gets fresh
		// relaunch attempts instead of tripping the cap immediately.
		assert.Equal(t, 1, c.launchFailed("c1"), "count should have been reset by the successful snapshot")
	})
}

func TestControllerExitedResumeFailureGoesRed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The agent reports session_exited and the resume cannot recreate the
		// session: the failure must be surfaced.
		store := testStore(t)
		require.NoError(t, store.InsertCard(&Card{ID: "c1", Column: "dev", Status: StatusStarting, Session: "s", Worktree: t.TempDir()}))

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		sessions := &crashingSessions{newErr: errors.New("tmux: server exited")}
		c := newController(ctx, store, sessions, func() {})
		c.clientFor = func(_, _ string) sessionClient {
			return &fakeClient{events: []event{{Type: eventSessionExited}}}
		}
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		c.Start(card)
		t.Cleanup(func() { c.Stop("c1") })

		synctest.Wait()
		card, err = store.GetCard("c1")
		require.NoError(t, err)
		assert.Equal(t, StatusError, card.Status)
	})
}

func TestRelaunchWithPromptResetsCrashCap(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Session: "s", Worktree: t.TempDir()}))

	c := newController(t.Context(), store, &crashingSessions{}, func() {})
	for range maxLaunchFailures {
		c.launchFailed("c1")
	}
	card, err := store.GetCard("c1")
	require.NoError(t, err)

	// A user-initiated (prompt-bearing) relaunch grants fresh attempts…
	require.NoError(t, c.relaunch(card, "try again"))
	assert.Equal(t, 1, c.launchFailed("c1"), "cap should have been reset")

	// …but a background resume does not, or the cap could never trip.
	require.NoError(t, c.relaunch(card, ""))
	assert.Equal(t, 2, c.launchFailed("c1"))
}

// argSessions records the arguments of the last NewSession call.
type argSessions struct {
	workDir, worktreeName, worktreeBase string
}

func (s *argSessions) NewSession(_, workDir, _, _, _, worktreeName, worktreeBase, _ string) error {
	s.workDir, s.worktreeName, s.worktreeBase = workDir, worktreeName, worktreeBase
	return nil
}
func (s *argSessions) KillSession(string) error    { return nil }
func (s *argSessions) Alive(string) (bool, error)  { return false, nil }
func (s *argSessions) Exists(string) (bool, error) { return false, nil }

func TestRelaunchRecreatesMissingWorktree(t *testing.T) {
	t.Parallel()

	// The first launch died before docker-agent created the worktree:
	// relaunching from the (missing) worktree directory would fail, so the
	// relaunch goes back to the repository and recreates the worktree.
	store := testStore(t)
	repo := t.TempDir()
	wt := filepath.Join(t.TempDir(), "board-abc")
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Session: "s", RepoPath: repo, Worktree: wt}))

	sessions := &argSessions{}
	c := newController(t.Context(), store, sessions, func() {})
	card, err := store.GetCard("c1")
	require.NoError(t, err)

	require.NoError(t, c.relaunch(card, ""))
	assert.Equal(t, repo, sessions.workDir)
	assert.Equal(t, "board-abc", sessions.worktreeName)
	assert.NotEmpty(t, sessions.worktreeBase)
}

func TestRelaunchResumesFromExistingWorktree(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	wt := t.TempDir()
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Session: "s", RepoPath: t.TempDir(), Worktree: wt}))

	sessions := &argSessions{}
	c := newController(t.Context(), store, sessions, func() {})
	card, err := store.GetCard("c1")
	require.NoError(t, err)

	require.NoError(t, c.relaunch(card, ""))
	assert.Equal(t, wt, sessions.workDir)
	assert.Empty(t, sessions.worktreeName)
}

func TestTeardownForgetsControllerState(t *testing.T) {
	t.Parallel()

	// A SendPrompt relaunch runs outside the watcher, so Stop cannot wait
	// for it: Teardown must drop the per-card state it may have re-added.
	store := testStore(t)
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Session: "s", Worktree: t.TempDir()}))

	c := newController(t.Context(), store, &crashingSessions{newErr: errors.New("boom")}, func() {})
	card, err := store.GetCard("c1")
	require.NoError(t, err)
	c.resume(card)
	require.Error(t, c.LaunchError("c1"))

	c.Teardown(card)
	require.NoError(t, c.LaunchError("c1"))
	assert.Equal(t, 1, c.launchFailed("c1"), "failure count should have been dropped")
}

func TestStartupPhase(t *testing.T) {
	t.Parallel()

	// The socket dir is per-user and process-global: use a unique session
	// id so parallel tests cannot collide.
	session := "phase-" + newID()
	card := &Card{AgentSession: session, Worktree: filepath.Join(t.TempDir(), "wt")}

	// Nothing on disk yet: the agent process is still booting.
	assert.Equal(t, StatusStarting, startupPhase(card))

	// The worktree appeared: the agent is loading models and tools.
	require.NoError(t, os.MkdirAll(card.Worktree, 0o755))
	assert.Equal(t, StatusLoading, startupPhase(card))

	// The control-plane socket is bound: the board is attaching.
	socket := socketPath(session)
	require.NoError(t, os.WriteFile(socket, nil, 0o600))
	t.Cleanup(func() { _ = os.Remove(socket) })
	assert.Equal(t, StatusAttaching, startupPhase(card))
}

// TestControllerStartupPhaseProgression proves the watcher surfaces the
// startup milestones of a live agent whose control plane has not answered
// yet, so a stuck launch shows how far it got.
func TestControllerStartupPhaseProgression(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		session := "progress-" + newID()
		store := testStore(t)
		wt := filepath.Join(t.TempDir(), "wt")
		require.NoError(t, store.InsertCard(&Card{ID: "c1", Column: "dev", Status: StatusStarting, Session: "s", AgentSession: session, Worktree: wt}))

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		// The pane is alive but the control plane never answers.
		c := newController(ctx, store, fakeSessions{}, func() {})
		c.clientFor = func(_, _ string) sessionClient { return downClient{} }
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		c.Start(card)
		t.Cleanup(func() { c.Stop("c1") })

		require.NoError(t, os.MkdirAll(wt, 0o755))
		waitForStatus(t, store, StatusLoading)

		socket := socketPath(session)
		require.NoError(t, os.WriteFile(socket, nil, 0o600))
		t.Cleanup(func() { _ = os.Remove(socket) })
		waitForStatus(t, store, StatusAttaching)
	})
}

// TestControllerNoDowngradeToStartupPhase proves a card mid-turn is not
// demoted to a startup phase when its control plane is transiently
// unreachable while the pane is still alive.
func TestControllerNoDowngradeToStartupPhase(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := testStore(t)
		require.NoError(t, store.InsertCard(&Card{ID: "c1", Column: "dev", Status: StatusRunning, Session: "s", Worktree: t.TempDir()}))

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		c := newController(ctx, store, fakeSessions{}, func() {})
		c.clientFor = func(_, _ string) sessionClient { return downClient{} }
		card, err := store.GetCard("c1")
		require.NoError(t, err)
		c.Start(card)
		t.Cleanup(func() { c.Stop("c1") })

		assert.Never(t, func() bool {
			card, err := store.GetCard("c1")
			return err == nil && card.Status != StatusRunning
		}, 300*time.Millisecond, 10*time.Millisecond)
	})
}
