package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetToolStatusRecordsExecutionTimes(t *testing.T) {
	t.Parallel()

	for _, terminal := range []ToolStatus{ToolStatusCompleted, ToolStatusError} {
		msg := &Message{}
		msg.SetToolStatus(ToolStatusPending)
		assert.Nil(t, msg.StartedAt)
		assert.Nil(t, msg.FinishedAt)
		msg.SetToolStatus(ToolStatusRunning)
		require.NotNil(t, msg.StartedAt)
		started := msg.StartedAt
		msg.SetToolStatus(ToolStatusRunning)
		assert.Same(t, started, msg.StartedAt)
		msg.SetToolStatus(terminal)
		require.NotNil(t, msg.FinishedAt)
		assert.False(t, msg.FinishedAt.Before(*started))
		finished := msg.FinishedAt
		msg.SetToolStatus(terminal)
		assert.Same(t, finished, msg.FinishedAt)
		assert.WithinDuration(t, time.Now(), *finished, time.Second)
	}
}

func TestSetToolStatusWithoutExecutionHasNoDuration(t *testing.T) {
	t.Parallel()

	for _, status := range []ToolStatus{ToolStatusCompleted, ToolStatusError} {
		msg := &Message{}
		msg.SetToolStatus(status)
		assert.Nil(t, msg.StartedAt)
		assert.Nil(t, msg.FinishedAt)
	}
}
