package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

type lateHandshake struct{ entered, release, closed chan struct{} }

func (c *lateHandshake) Connect(context.Context) (lifecycle.Session, error) {
	close(c.entered)
	<-c.release
	return c, nil
}
func (c *lateHandshake) Wait() error                 { <-c.closed; return nil }
func (c *lateHandshake) Close(context.Context) error { close(c.closed); return nil }

type supervisedToolSet struct{ supervisor *lifecycle.Supervisor }

func (s *supervisedToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *supervisedToolSet) Start(ctx context.Context) error             { return s.supervisor.Start(ctx) }

func (s *supervisedToolSet) Stop(ctx context.Context) error { return s.supervisor.Stop(ctx) }

func TestDeleteSessionReapsLateHandshakeAndRejectsReuse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := &lateHandshake{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	release := sync.OnceFunc(func() { close(c.release) })
	defer release()
	ts := &supervisedToolSet{supervisor: lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: 10 * time.Millisecond})}
	root := agent.New("root", "root", agent.WithToolSets(ts))
	owner := root.DisposableToolSets()[0].(*tools.StartableToolSet)
	require.ErrorIs(t, owner.Start(ctx), lifecycle.ErrInitTimeout)
	<-c.entered
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	rt := &closeTrackingRuntime{}
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: rt, team: team.New(team.WithAgents(root)), cancel: func() {}, session: sess})
	require.NoError(t, sm.DeleteSession(ctx, sess.ID))
	require.Eventually(t, func() bool {
		return rt.closed.Load() == 1 && owner.Start(ctx) != nil && ts.supervisor.State().State == lifecycle.StateStopped
	}, time.Second, time.Millisecond)
	require.ErrorContains(t, owner.Start(ctx), "disposed")
	release()
	select {
	case <-c.closed:
	case <-time.After(time.Second):
		t.Fatal("late handshake was not reaped")
	}
}

type trackedClose struct{ closes atomic.Int32 }

func (s *trackedClose) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *trackedClose) Close() error                                { s.closes.Add(1); return nil }

type slowSession struct {
	closed  chan struct{}
	release chan struct{}
}

func (s *slowSession) Wait() error                 { <-s.closed; return nil }
func (s *slowSession) Close(context.Context) error { <-s.release; close(s.closed); return nil }

type singleConnector struct{ sess *slowSession }

func (c *singleConnector) Connect(context.Context) (lifecycle.Session, error) { return c.sess, nil }

func TestDisposalContinuesAfterSupervisorCloseTimeout(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	sess := &slowSession{closed: make(chan struct{}), release: make(chan struct{})}
	defer close(sess.release)
	first := &supervisedToolSet{supervisor: lifecycle.New("test", &singleConnector{sess}, lifecycle.Policy{})}
	second := &trackedClose{}
	root := agent.New("root", "root", agent.WithToolSets(first, second))
	require.NoError(t, first.Start(ctx))
	disposeCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := team.New(team.WithAgents(root)).Dispose(disposeCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.EqualValues(t, 1, second.closes.Load())
}
