//go:build !js

package strategy

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/types"
)

// TestInitializeConcurrentFilesRecordsUsageSafely indexes files in parallel
// while another goroutine polls the totals: under -race this catches
// unsynchronized access to the usage counters, and the final total must be
// exactly one token per file.
func TestInitializeConcurrentFilesRecordsUsageSafely(t *testing.T) {
	t.Parallel()
	store, _, docPaths := newTestVectorStoreWithConcurrency(t, nil, 5)
	events := make(chan types.Event, 64)
	store.events = events

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				store.GetIndexingUsage()
			}
		}
	})
	err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	close(stop)
	wg.Wait()
	require.NoError(t, err)

	tokens, cost := store.GetIndexingUsage()
	assert.Equal(t, int64(len(docPaths)), tokens)
	assert.Zero(t, cost)

	var lastTotal int64
	for len(events) > 0 {
		if ev := <-events; ev.Type == types.EventTypeUsage {
			lastTotal = max(lastTotal, ev.TotalTokens)
		}
	}
	assert.Equal(t, tokens, lastTotal, "cumulative usage events end at the recorded total")
}
