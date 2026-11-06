package tools_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools"
)

type coordinatedToolSet struct {
	err error
}

func (t *coordinatedToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (t *coordinatedToolSet) Start(context.Context) error {
	return t.err
}

func (t *coordinatedToolSet) Stop(context.Context) error {
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
