package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type flakyStartToolset struct {
	calls atomic.Int64
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*flakyStartToolset)(nil)
	_ tools.Startable = (*flakyStartToolset)(nil)
)

func (f *flakyStartToolset) Start(context.Context) error {
	if f.calls.Add(1) == 1 {
		return errors.New("no events channel available for elicitation")
	}
	return nil
}

func (f *flakyStartToolset) Stop(context.Context) error { return nil }

func (f *flakyStartToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

// trackingStartToolset records Start/Stop call counts.
type trackingStartToolset struct {
	starts atomic.Int64
	stops  atomic.Int64
}

var (
	_ tools.ToolSet   = (*trackingStartToolset)(nil)
	_ tools.Startable = (*trackingStartToolset)(nil)
)

func (t *trackingStartToolset) Start(context.Context) error {
	t.starts.Add(1)
	return nil
}

func (t *trackingStartToolset) Stop(context.Context) error {
	t.stops.Add(1)
	return nil
}

func (t *trackingStartToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func TestStartableToolSet_RetriesAfterFailure(t *testing.T) {
	ctx := t.Context()
	inner := &flakyStartToolset{}
	ts := tools.NewStartable(inner)

	err := ts.Start(ctx)
	require.Error(t, err)
	require.False(t, ts.IsStarted())

	err = ts.Start(ctx)
	require.NoError(t, err)
	require.True(t, ts.IsStarted())

	// Once started, subsequent calls should not call inner.Start again.
	err = ts.Start(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), inner.calls.Load())
}

func TestStartableToolSet_RestartAfterStop(t *testing.T) {
	ctx := t.Context()
	inner := &trackingStartToolset{}
	ts := tools.NewStartable(inner)

	// Start the toolset.
	require.NoError(t, ts.Start(ctx))
	require.True(t, ts.IsStarted())
	require.Equal(t, int64(1), inner.starts.Load())

	// Stop must reset IsStarted so that a future Start re-initializes.
	require.NoError(t, ts.Stop(ctx))
	require.False(t, ts.IsStarted())
	require.Equal(t, int64(1), inner.stops.Load())

	// Start again: the inner Start must be called a second time.
	require.NoError(t, ts.Start(ctx))
	require.True(t, ts.IsStarted())
	require.Equal(t, int64(2), inner.starts.Load())
}
