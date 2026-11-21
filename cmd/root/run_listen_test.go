package root

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
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

func TestStartSessionCoordinatorListenReturnsDetach(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	flags := &runExecFlags{listenAddr: "127.0.0.1:0"}

	opt, detach, err := flags.startSessionCoordinator(ctx, cli.NewPrinter(io.Discard), &recallRuntime{store: store}, sess)
	require.NoError(t, err)
	require.NotNil(t, opt)
	require.NotNil(t, detach)
	require.NotNil(t, flags.listenSM)
	require.Len(t, flags.listenSM.GetActiveSessions(), 1)

	detach()

	require.Empty(t, flags.listenSM.GetActiveSessions())
	assert.False(t, flags.listenSM.WaitSessionAttached(ctx, sess.ID, 10*time.Millisecond))
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
