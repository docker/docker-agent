package lifecycle_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

type blockedCloseSession struct {
	*fakeSession

	closing chan struct{}
	release chan struct{}
}

func (s *blockedCloseSession) Close(ctx context.Context) error {
	close(s.closing)
	<-s.release
	return s.fakeSession.Close(ctx)
}

type blockedCloseConnector struct {
	first  *blockedCloseSession
	second *fakeSession
	calls  atomic.Int32
}

func (c *blockedCloseConnector) Connect(context.Context) (lifecycle.Session, error) {
	if c.calls.Add(1) == 1 {
		return c.first, nil
	}
	return c.second, nil
}

func TestStopDeadlineKeepsCloseBarrier(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first := &blockedCloseSession{fakeSession: newFakeSession(), closing: make(chan struct{}), release: make(chan struct{})}
		c := &blockedCloseConnector{first: first, second: newFakeSession()}
		s := lifecycle.New("test", c, lifecycle.Policy{})
		assert.NilError(t, s.Start(t.Context()))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		stopped := make(chan error, 1)
		go func() { stopped <- s.Stop(ctx) }()
		<-first.closing
		assert.ErrorIs(t, <-stopped, context.DeadlineExceeded)
		resumed := make(chan error, 1)
		go func() { resumed <- s.Start(t.Context()) }()
		synctest.Wait()
		assert.Equal(t, c.calls.Load(), int32(1))
		close(first.release)
		assert.NilError(t, <-resumed)
		assert.Equal(t, c.calls.Load(), int32(2))
		assert.NilError(t, s.Stop(t.Context()))
	})
}
