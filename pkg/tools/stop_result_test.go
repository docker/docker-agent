package tools

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stopResultToolset struct {
	started chan struct{}
	release chan struct{}
	stopErr error
	stops   int
}

func (*stopResultToolset) Tools(context.Context) ([]Tool, error) { return nil, nil }
func (s *stopResultToolset) Start(context.Context) error {
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	return nil
}

func (s *stopResultToolset) Restart(context.Context) error { return nil }

func (s *stopResultToolset) Stop(context.Context) error {
	s.stops++
	return s.stopErr
}

func TestStopIfStartedRetainsHandoffFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("stop failed")
		inner := &stopResultToolset{started: make(chan struct{}), release: make(chan struct{}), stopErr: boom}
		s := NewStartable(inner)
		started := make(chan error, 1)
		go func() { started <- s.Start(t.Context()) }()
		<-inner.started
		stopped := make(chan error, 1)
		go func() { stopped <- s.StopIfStarted(t.Context()) }()
		synctest.Wait()
		close(inner.release)
		require.NoError(t, <-started)
		require.ErrorIs(t, <-stopped, boom)
		require.ErrorIs(t, s.StopIfStarted(t.Context()), boom)
		assert.Equal(t, 1, inner.stops)

		inner.started = nil
		inner.stopErr = nil
		require.NoError(t, s.Start(t.Context()))
		require.NoError(t, s.stopErr)
		require.NoError(t, s.StopIfStarted(t.Context()))
		assert.Equal(t, 2, inner.stops)
	})
}

func TestSuccessfulRestartClearsStopFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("stop failed")
	inner := &stopResultToolset{stopErr: boom}
	s := NewStartable(inner)
	require.NoError(t, s.Start(t.Context()))
	require.ErrorIs(t, s.Stop(t.Context()), boom)
	require.ErrorIs(t, s.stopErr, boom)
	require.NoError(t, s.RestartIfSupported(t.Context()))
	require.NoError(t, s.stopErr)
}
