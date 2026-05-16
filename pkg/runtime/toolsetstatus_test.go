package runtime

import (
	"context"
	"errors"
	"testing"

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
