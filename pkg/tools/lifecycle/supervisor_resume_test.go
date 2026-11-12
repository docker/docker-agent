package lifecycle_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestSupervisor_StartAfterStop(t *testing.T) {
	t.Parallel()
	for _, stopFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "never-started"}[stopFirst], func(t *testing.T) {
			sessions := []*fakeSession{newFakeSession(), newFakeSession(), newFakeSession()}
			c := newScriptedConnector(scriptStep{session: sessions[0]}, scriptStep{session: sessions[1]}, scriptStep{session: sessions[2]})
			s := lifecycle.New("test", c, lifecycle.Policy{})
			if stopFirst {
				assert.NilError(t, s.Stop(t.Context()))
			}
			for _, sess := range sessions {
				assert.NilError(t, s.Start(t.Context()))
				assert.Equal(t, s.State().State, lifecycle.StateReady)
				sess.waitParked(t)
				assert.NilError(t, s.Stop(t.Context()))
				assert.Equal(t, s.State().State, lifecycle.StateStopped)
				sess.waitClosed(t)
			}
			assert.Equal(t, c.Calls(), len(sessions))
		})
	}
}

func TestSupervisor_StartWaitsForStoppedWatcher(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		disconnected, release := make(chan struct{}), make(chan struct{})
		first, second := newFakeSession(), newFakeSession()
		c := newScriptedConnector(scriptStep{session: first}, scriptStep{session: second})
		s := lifecycle.New("test", c, lifecycle.Policy{OnDisconnect: func(error) { close(disconnected); <-release }})
		assert.NilError(t, s.Start(t.Context()))
		first.fail(errors.New("disconnected"))
		<-disconnected
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.ErrorIs(t, s.Stop(ctx), context.Canceled)
		assert.ErrorIs(t, s.Start(ctx), context.Canceled)
		assert.Equal(t, c.Calls(), 1)
		resumed := make(chan error, 1)
		go func() { resumed <- s.Start(t.Context()) }()
		synctest.Wait()
		assert.Equal(t, c.Calls(), 1)
		close(release)
		assert.NilError(t, <-resumed)
		assert.Equal(t, c.Calls(), 2)
		assert.NilError(t, s.Stop(t.Context()))
	})
}

type resumeConnector struct {
	calls         atomic.Int32
	release       chan struct{}
	first, second *fakeSession
	delay         time.Duration
}

func (c *resumeConnector) Connect(context.Context) (lifecycle.Session, error) {
	if c.calls.Add(1) == 1 {
		<-c.release
		return c.first, nil
	}
	time.Sleep(c.delay) //nolint:forbidigo // Simulate handshake latency inside a synctest bubble.
	return c.second, nil
}

func TestSupervisor_StartAfterStopWaitsForLateConnect(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &resumeConnector{release: make(chan struct{}), first: newFakeSession(), second: newFakeSession()}
		s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: time.Millisecond})
		assert.ErrorIs(t, s.Start(t.Context()), lifecycle.ErrInitTimeout)
		assert.NilError(t, s.Stop(t.Context()))
		resumed := make(chan error, 1)
		go func() { resumed <- s.Start(t.Context()) }()
		synctest.Wait()
		assert.Equal(t, c.calls.Load(), int32(1))
		close(c.release)
		assert.NilError(t, <-resumed)
		c.first.waitClosed(t)
		assert.Equal(t, c.calls.Load(), int32(2))
		assert.NilError(t, s.Stop(t.Context()))
	})
}

func TestSupervisor_ResumeTimeoutWhileReaping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &resumeConnector{release: make(chan struct{}), first: newFakeSession(), second: newFakeSession()}
		release := c.release
		defer func() {
			if release != nil {
				close(release)
			}
		}()
		s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: time.Millisecond})
		assert.ErrorIs(t, s.Start(t.Context()), lifecycle.ErrInitTimeout)
		assert.NilError(t, s.Stop(t.Context()))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		for range 2 {
			started := time.Now()
			assert.ErrorIs(t, s.Start(ctx), lifecycle.ErrInitTimeout)
			assert.Equal(t, time.Since(started), time.Millisecond)
			assert.Equal(t, c.calls.Load(), int32(1))
		}
		close(c.release)
		release = nil
		synctest.Wait()
		assert.NilError(t, s.Start(t.Context()))
		assert.Equal(t, c.calls.Load(), int32(2))
		assert.NilError(t, s.Stop(t.Context()))
	})
}

func TestSupervisor_ResumeWaitsForShutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first, second := newFakeSession(), newFakeSession()
		c := newScriptedConnector(scriptStep{session: first}, scriptStep{session: second})
		cleaning, release := make(chan struct{}), make(chan struct{})
		var shutdowns atomic.Int32
		s := lifecycle.New("test", c, lifecycle.Policy{
			StartupTimeout: time.Millisecond,
			Shutdown: func(_ context.Context, teardown func() error) error {
				err := teardown()
				if shutdowns.Add(1) == 1 {
					close(cleaning)
					<-release
				}
				return err
			},
		})
		assert.NilError(t, s.Start(t.Context()))
		first.waitParked(t)
		stopped := make(chan error, 1)
		go func() { stopped <- s.Stop(t.Context()) }()
		<-cleaning
		joined := make(chan error, 1)
		go func() { joined <- s.Stop(t.Context()) }()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.ErrorIs(t, s.Start(ctx), context.Canceled)
		assert.ErrorIs(t, s.Stop(ctx), context.Canceled)
		assert.ErrorIs(t, s.Start(t.Context()), lifecycle.ErrInitTimeout)
		assert.Equal(t, c.Calls(), 1)
		assert.Equal(t, shutdowns.Load(), int32(1))
		close(release)
		assert.NilError(t, <-stopped)
		assert.NilError(t, <-joined)
		assert.NilError(t, s.Start(t.Context()))
		assert.Equal(t, c.Calls(), 2)
		assert.NilError(t, s.Stop(t.Context()))
	})
}

func TestSupervisor_ResumeSharesStartupBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first, second := newFakeSession(), newFakeSession()
		release := make(chan struct{})
		close(release)
		c := &resumeConnector{release: release, first: first, second: second, delay: 6 * time.Millisecond}
		var shutdowns atomic.Int32
		s := lifecycle.New("test", c, lifecycle.Policy{
			StartupTimeout: 10 * time.Millisecond,
			Shutdown: func(_ context.Context, teardown func() error) error {
				err := teardown()
				if shutdowns.Add(1) == 1 {
					time.Sleep(6 * time.Millisecond) //nolint:forbidigo // Consume part of the fake startup budget.
				}
				return err
			},
		})
		assert.NilError(t, s.Start(t.Context()))
		first.waitParked(t)
		stopped := make(chan error, 1)
		go func() { stopped <- s.Stop(t.Context()) }()
		synctest.Wait()
		started := time.Now()
		assert.ErrorIs(t, s.Start(t.Context()), lifecycle.ErrInitTimeout)
		assert.Equal(t, time.Since(started), 10*time.Millisecond)
		assert.NilError(t, <-stopped)
		synctest.Sleep(2 * time.Millisecond)
		assert.NilError(t, s.Start(t.Context()))
		assert.Equal(t, c.calls.Load(), int32(2))
		assert.NilError(t, s.Stop(t.Context()))
	})
}
