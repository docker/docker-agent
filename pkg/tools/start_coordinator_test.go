package tools_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools"
)

type coordinatedToolSet struct {
	err     error
	started atomic.Bool
}

func (t *coordinatedToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (t *coordinatedToolSet) Start(context.Context) error {
	if t.err != nil {
		return t.err
	}
	t.started.Store(true)
	return nil
}

func (t *coordinatedToolSet) Stop(context.Context) error {
	t.started.Store(false)
	return nil
}

type dependentToolSet struct {
	coordinatedToolSet

	peer        *coordinatedToolSet
	peerStarted atomic.Bool
}

func (*dependentToolSet) StartsAfterPeers() {}
func (t *dependentToolSet) Start(context.Context) error {
	t.peerStarted.Store(t.peer.started.Load())
	return nil
}

func TestStartToolSetsClassifiesAndPreservesOrder(t *testing.T) {
	t.Parallel()

	failed := errors.New("unavailable")
	first := tools.NewStartable(&coordinatedToolSet{})
	second := tools.NewStartable(&coordinatedToolSet{err: failed})

	outcomes := tools.StartToolSets(t.Context(), []*tools.StartableToolSet{first, second}, time.Second)
	firstOutcome := <-outcomes[0]
	secondOutcome := <-outcomes[1]

	assert.Check(t, cmp.Equal(firstOutcome.Kind, tools.StartReady))
	assert.Check(t, cmp.Equal(secondOutcome.Kind, tools.StartFailed))
	assert.ErrorIs(t, secondOutcome.Err, failed)
	assert.Check(t, secondOutcome.ReportFailure)
}

func TestStartToolSetsStartsDependentToolSetsSecond(t *testing.T) {
	t.Parallel()

	peer := &coordinatedToolSet{}
	dependent := &dependentToolSet{peer: peer}
	outcomes := tools.StartToolSets(t.Context(), []*tools.StartableToolSet{
		tools.NewStartable(dependent),
		tools.NewStartable(peer),
	}, time.Second)

	assert.Check(t, cmp.Equal((<-outcomes[0]).Kind, tools.StartReady))
	assert.Check(t, cmp.Equal((<-outcomes[1]).Kind, tools.StartReady))
	assert.Check(t, dependent.peerStarted.Load())
}
