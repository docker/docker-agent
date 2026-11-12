package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryMessageQueueCancel(t *testing.T) {
	t.Parallel()
	q := NewInMemoryMessageQueue(3).(*inMemoryMessageQueue)
	require.True(t, q.Enqueue(t.Context(), QueuedMessage{ID: "first", Content: "one"}))
	require.True(t, q.Enqueue(t.Context(), QueuedMessage{ID: "second", Content: "two"}))
	require.True(t, q.Enqueue(t.Context(), QueuedMessage{ID: "third", Content: "three"}))

	assert.True(t, q.Cancel("second"))
	assert.False(t, q.Cancel("second"))
	assert.False(t, q.Cancel(""))
	assert.Equal(t, []QueuedMessage{
		{ID: "first", Content: "one"},
		{ID: "third", Content: "three"},
	}, q.Drain(t.Context()))
}

func TestInMemoryMessageQueueRejectsCanceledContext(t *testing.T) {
	t.Parallel()
	q := NewInMemoryMessageQueue(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	assert.False(t, q.Enqueue(ctx, QueuedMessage{Content: "nope"}))
	_, ok := q.Dequeue(t.Context())
	assert.False(t, ok)
}
