package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// statefulToolset is a test ToolSet that implements tools.Statable and
// tools.Describer so we can assert toolsetStatusFor unwraps and reports
// each piece correctly.
type statefulToolset struct {
	desc string
	info lifecycle.StateInfo
}

func (s *statefulToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *statefulToolset) Describe() string                            { return s.desc }
func (s *statefulToolset) State() lifecycle.StateInfo                  { return s.info }

func TestToolsetStatusFor_StatableReportsState(t *testing.T) {
	t.Parallel()

	want := errors.New("kaboom")
	ts := &statefulToolset{
		desc: "mcp(stdio cmd=foo)",
		info: lifecycle.StateInfo{
			State:        lifecycle.StateRestarting,
			LastError:    want,
			RestartCount: 3,
		},
	}

	got := toolsetStatusFor(ts)
	assert.Equal(t, "mcp(stdio cmd=foo)", got.Description)
	assert.Equal(t, lifecycle.StateRestarting, got.State)
	require.ErrorIs(t, got.LastError, want)
	assert.Equal(t, 3, got.RestartCount)
}

// describerOnly is a Describer that does NOT implement Statable.
type describerOnly struct{ desc string }

func (s *describerOnly) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *describerOnly) Describe() string                            { return s.desc }

func TestToolsetStatusFor_NoStatableMeansReady(t *testing.T) {
	t.Parallel()

	ts := &describerOnly{desc: "filesystem"}
	got := toolsetStatusFor(ts)
	assert.Equal(t, lifecycle.StateReady, got.State)
	assert.Equal(t, 0, got.RestartCount)
	require.NoError(t, got.LastError)
	assert.Equal(t, "filesystem", got.Description)
	assert.False(t, got.Restartable, "toolset without Restart() must report Restartable=false")
}

func TestToolsetStatusFor_RestartableReportsTrue(t *testing.T) {
	t.Parallel()

	ts := &restartableToolset{desc: "mcp(stdio cmd=foo)", state: lifecycle.StateInfo{State: lifecycle.StateReady}}
	got := toolsetStatusFor(ts)
	assert.True(t, got.Restartable)
}

func TestToolsetStatusFor_NonRestartableReportsFalse(t *testing.T) {
	t.Parallel()

	ts := &statefulToolset{desc: "filesystem", info: lifecycle.StateInfo{State: lifecycle.StateReady}}
	got := toolsetStatusFor(ts)
	assert.False(t, got.Restartable)
}

// TestToolsetStatusFor_UnwrapsStartable verifies the inner Statable is
// observed even when wrapped by StartableToolSet, which is how toolsets
// are typically registered with the agent runtime.
func TestToolsetStatusFor_UnwrapsStartable(t *testing.T) {
	t.Parallel()

	want := errors.New("inner")
	inner := &statefulToolset{
		desc: "mcp(remote host=example.com)",
		info: lifecycle.StateInfo{State: lifecycle.StateFailed, LastError: want, RestartCount: 5},
	}
	wrapped := tools.NewStartable(inner)

	got := toolsetStatusFor(wrapped)
	assert.Equal(t, lifecycle.StateFailed, got.State)
	require.ErrorIs(t, got.LastError, want)
	assert.Equal(t, 5, got.RestartCount)
	assert.Equal(t, "mcp(remote host=example.com)", got.Description)
}

// TestToolsetStatusFor_UnsupervisedReportsStartingWithoutBlocking pins the
// #4073 motivation for the non-blocking status probe: a toolset with no
// Statable supervisor whose Start is still running (e.g. RAG indexing a
// large knowledge base) must report lifecycle.StateStarting rather than
// blocking toolsetStatusFor until Start settles.
func TestToolsetStatusFor_UnsupervisedReportsStartingWithoutBlocking(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	inner := &inFlightStartToolSet{entered: make(chan struct{}), release: release}
	wrapped := tools.NewStartable(inner)

	startDone := make(chan error, 1)
	go func() { startDone <- wrapped.Start(t.Context()) }()
	<-inner.entered

	resultCh := make(chan tools.ToolsetStatus, 1)
	go func() { resultCh <- toolsetStatusFor(wrapped) }()

	select {
	case status := <-resultCh:
		assert.Equal(t, lifecycle.StateStarting, status.State, "a mid-start toolset must report Starting, not block or misreport")
	case <-time.After(5 * time.Second):
		t.Fatal("toolsetStatusFor blocked behind an in-flight Start instead of reporting Starting")
	}

	close(release)
	require.NoError(t, <-startDone)

	assert.Equal(t, lifecycle.StateReady, toolsetStatusFor(wrapped).State, "settled start reports Ready")
}
