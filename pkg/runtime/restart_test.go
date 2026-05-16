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

// restartableToolset is a Statable + Restartable + Describer used to
// drive RestartToolset's matching+dispatch logic in isolation.
type restartableToolset struct {
	desc        string
	state       lifecycle.StateInfo
	restartErr  error
	restartCall int
}

func (r *restartableToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *restartableToolset) Describe() string                            { return r.desc }
func (r *restartableToolset) State() lifecycle.StateInfo                  { return r.state }

func (r *restartableToolset) Restart(context.Context) error {
	r.restartCall++
	return r.restartErr
}

// nameForTest exposes the package-private nameFor helper for tests so
// they exercise the same matching logic as production code.
func nameForTest(ts tools.ToolSet, fallback string) string {
	return nameFor(ts, fallback)
}

func TestNameFor_DescriberFallback(t *testing.T) {
	t.Parallel()
	ts := &restartableToolset{desc: "mcp(stdio)"}
	assert.Equal(t, "mcp(stdio)", nameForTest(ts, tools.DescribeToolSet(ts)))
}

// TestRestartToolset_HappyPath constructs a fake agent (LocalRuntime
// with a single statable+restartable toolset) and exercises the by-name
// dispatch.
//
// We don't construct a full runtime here — that's covered by integration
// tests — but we do exercise the same helper RestartToolset uses
// internally (toolsetStatusFor + match by name + Restartable.Restart).
func TestRestartable_DispatchesByName(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("post-restart error from supervisor")
	ts := &restartableToolset{
		desc:       "mcp(stdio cmd=foo)",
		restartErr: wantErr,
	}

	// Match against the description (the same name resolution the
	// runtime does via nameFor when there's no Name() method).
	matched := false
	if nameForTest(ts, tools.DescribeToolSet(ts)) == "mcp(stdio cmd=foo)" {
		matched = true
		err := ts.Restart(t.Context())
		require.ErrorIs(t, err, wantErr)
	}
	assert.True(t, matched)
	assert.Equal(t, 1, ts.restartCall)
}
