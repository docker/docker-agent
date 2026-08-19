package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
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
	t.Parallel()
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
	t.Parallel()
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

// TestWithStructuredOutput verifies the original structured-output config is
// retained on the agent so the runtime can enforce tool mode.
func TestWithStructuredOutput(t *testing.T) {
	t.Parallel()

	so := &latest.StructuredOutput{
		Name:   "result",
		Schema: map[string]any{"type": "object"},
		Mode:   latest.StructuredOutputModeTool,
	}

	a := New("root", "prompt", WithStructuredOutput(so))
	require.Same(t, so, a.StructuredOutput())

	require.Nil(t, New("bare", "prompt").StructuredOutput())
}

// TestStructuredOutputTool_CompiledOnceAndResetByOpt pins the lazy cache
// contract: the tool-mode output tool is compiled once and reused across
// calls, WithStructuredOutput resets the cache, native mode (and no
// structured output) compiles nothing, and compile errors surface to the
// caller.
func TestStructuredOutputTool_CompiledOnceAndResetByOpt(t *testing.T) {
	t.Parallel()

	toolMode := &latest.StructuredOutput{
		Name:   "result",
		Mode:   latest.StructuredOutputModeTool,
		Schema: map[string]any{"type": "object"},
	}

	a := New("root", "prompt", WithStructuredOutput(toolMode))
	first, err := a.StructuredOutputTool()
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := a.StructuredOutputTool()
	require.NoError(t, err)
	require.Same(t, first, second, "repeated calls must return the cached tool")

	WithStructuredOutput(toolMode)(a)
	third, err := a.StructuredOutputTool()
	require.NoError(t, err)
	require.NotSame(t, first, third, "WithStructuredOutput must reset the compile cache")

	native := New("native", "prompt", WithStructuredOutput(&latest.StructuredOutput{
		Name:   "result",
		Schema: map[string]any{"type": "object"},
	}))
	tool, err := native.StructuredOutputTool()
	require.NoError(t, err)
	require.Nil(t, tool, "native mode must not compile the output tool")

	bare, err := New("bare", "prompt").StructuredOutputTool()
	require.NoError(t, err)
	require.Nil(t, bare)

	broken := New("broken", "prompt", WithStructuredOutput(&latest.StructuredOutput{
		Name:   "broken",
		Mode:   latest.StructuredOutputModeTool,
		Schema: map[string]any{"type": 42},
	}))
	_, err = broken.StructuredOutputTool()
	require.ErrorContains(t, err, "schema")
}
