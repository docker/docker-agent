package root

import (
	"context"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type recallRuntime struct {
	runtime.Runtime

	store         session.Store
	recallHandler runtime.RecallHandler
}

func (r *recallRuntime) SessionStore() session.Store { return r.store }

func (r *recallRuntime) SetRecallHandler(handler runtime.RecallHandler) {
	r.recallHandler = handler
}

// The App started by the supervisor below asks the runtime for its startup info
// and registers event sinks; the embedded nil Runtime would panic, so the bare
// recall fixture stubs them out.
func (r *recallRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}

func (r *recallRuntime) OnToolsChanged(func(runtime.Event)) {}

func (r *recallRuntime) OnBackgroundEvent(func(runtime.Event)) {}

func (r *recallRuntime) OnElicitationRequest(func(runtime.Event)) {}

// testListenFlags builds the flags for a control-plane test with an isolated
// run registry: the listener writes a discovery record under the data dir, and
// tests must never advertise a fake live run (or leave a stale record) in the
// real one.
func testListenFlags(t *testing.T) (*runExecFlags, *runregistry.Registry) {
	t.Helper()
	registry := runregistry.New(t.TempDir())
	return &runExecFlags{
		listenAddr:  "127.0.0.1:0",
		runConfig:   config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})},
		runRegistry: registry,
	}, registry
}

const testSpawnAgentManifest = `agents:
  root:
    model: openai/gpt-5-mini
    instruction: test agent
`

func TestStartSessionCoordinatorWithoutListenRoutesIdleRecallToApp(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.AddSession(ctx, sess))

		rt := &recallRuntime{store: store}
		flags := &runExecFlags{runConfig: config.RuntimeConfig{}}
		opt, _, err := flags.startSessionCoordinator(ctx, nil, rt, sess)
		require.NoError(t, err)
		require.NotNil(t, opt)
		require.NotNil(t, rt.recallHandler)

		a := app.New(ctx, rt, sess, opt)
		events := make(chan tea.Msg, 1)
		subCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go a.SubscribeWith(subCtx, func(msg tea.Msg) {
			select {
			case events <- msg:
			default:
			}
			cancel()
		})

		require.True(t, rt.recallHandler(ctx, runtime.QueuedMessage{Content: "background job finished"}))

		select {
		case msg := <-events:
			sendMsg, ok := msg.(msgtypes.SendMsg)
			require.True(t, ok, "expected SendMsg, got %T", msg)
			assert.Equal(t, "background job finished", sendMsg.Content)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for injected recall message")
		}
	})
}

func TestStartSessionCoordinatorRejectsNonLocalListener(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	flags := &runExecFlags{listenAddr: "0.0.0.0:0"}

	opt, detach, err := flags.startSessionCoordinator(t.Context(), nil, &recallRuntime{store: store}, sess)
	require.ErrorContains(t, err, "require --auth-token")
	assert.Nil(t, opt)
	assert.Nil(t, detach)
}

// TestStartSessionCoordinatorListenUsesIsolatedRegistry covers the listener
// lifecycle and the test-registry isolation: the record is written to the
// injected registry, and cancelling the run context removes it again.
func TestStartSessionCoordinatorListenUsesIsolatedRegistry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	flags, registry := testListenFlags(t)

	opt, detach, err := flags.startSessionCoordinator(ctx, cli.NewPrinter(io.Discard), &recallRuntime{store: store}, sess)
	require.NoError(t, err)
	require.NotNil(t, opt)
	require.NotNil(t, detach)
	require.NotNil(t, flags.listenSM)
	require.Len(t, flags.listenSM.GetActiveSessions(), 1)

	records, err := registry.List()
	require.NoError(t, err)
	require.Len(t, records, 1, "the listener must advertise the run in the injected registry")
	assert.Equal(t, sess.ID, records[0].SessionID)

	detach()
	require.Empty(t, flags.listenSM.GetActiveSessions())
	assert.False(t, flags.listenSM.WaitSessionAttached(ctx, sess.ID, 10*time.Millisecond))

	cancel()
	require.Eventually(t, func() bool {
		records, err := registry.List()
		return err == nil && len(records) == 0
	}, 5*time.Second, 10*time.Millisecond, "cancelling the run must remove the discovery record")
}

func TestDetachOnCloseRunsDetachOnceBeforeCleanup(t *testing.T) {
	t.Parallel()

	var order []string
	cleanup := detachOnClose(
		func() { order = append(order, "cleanup") },
		func() { order = append(order, "detach") },
	)

	cleanup()
	cleanup()

	assert.Equal(t, []string{"detach", "cleanup"}, order)
}

// activeSessionIDs snapshots the control plane's active session IDs.
func activeSessionIDs(sm *server.SessionManager) []string {
	sessions := sm.GetActiveSessions()
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	return ids
}

// TestInitialAndSpawnedTabCloseDetachFromListenManager is the lifecycle
// regression test for #4909: it closes a real initial tab and a real spawned tab
// through supervisor.CloseSession (the path the TUI uses) and checks the serving
// manager afterwards, instead of invoking the detach/cleanup callbacks directly.
func TestInitialAndSpawnedTabCloseDetachFromListenManager(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	rt := &recallRuntime{store: store}
	flags, _ := testListenFlags(t)
	flags.agentName = "root"

	coordinatorOpt, detachCoordinator, err := flags.startSessionCoordinator(ctx, cli.NewPrinter(io.Discard), rt, sess)
	require.NoError(t, err)
	require.NotNil(t, coordinatorOpt)
	require.NotNil(t, detachCoordinator)

	// The spawner is the real one runOrExec hands to the TUI.
	spawner := flags.createSessionSpawner(config.NewBytesSource("agent.yaml", []byte(testSpawnAgentManifest)), store)
	sup := supervisor.New(spawner)

	// Initial tab: wired exactly as runOrExec wires it before handing the
	// cleanup to the TUI.
	initialCleanup := detachOnClose(func() {}, detachCoordinator)
	initialApp := app.New(ctx, rt, sess, coordinatorOpt)
	sup.AddSession(ctx, initialApp, sess, t.TempDir(), initialCleanup)
	require.Equal(t, []string{sess.ID}, activeSessionIDs(flags.listenSM))

	// Spawned tab: created by the real spawner and added by the supervisor.
	spawnedID, err := sup.SpawnSession(ctx, t.TempDir())
	require.NoError(t, err)
	require.NotEqual(t, sess.ID, spawnedID)
	require.ElementsMatch(t, []string{sess.ID, spawnedID}, activeSessionIDs(flags.listenSM))

	// Closing the initial tab while another tab is open must detach only it.
	sup.CloseSession(sess.ID)
	require.Eventually(t, func() bool {
		return slices.Equal([]string{spawnedID}, activeSessionIDs(flags.listenSM))
	}, 5*time.Second, 10*time.Millisecond, "closing the initial tab must detach it and leave the spawned tab attached")

	// Tab close is not session delete: the initial session is still stored.
	_, err = store.GetSession(ctx, sess.ID)
	require.NoError(t, err, "closing the initial tab must not delete the session")

	// Closing the last tab releases the manager completely.
	sup.CloseSession(spawnedID)
	require.Eventually(t, func() bool {
		return len(flags.listenSM.GetActiveSessions()) == 0
	}, 5*time.Second, 10*time.Millisecond, "closing the spawned tab must detach it")
}
