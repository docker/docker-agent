package app

import (
	"context"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestEventQueueWakeupAndDrain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		queue := newEventQueue()
		received := make(chan tea.Msg, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				msg, ok := queue.next(ctx, nil)
				if !ok {
					return
				}
				received <- msg
			}
		}()
		for i := range 10 {
			synctest.Wait()
			queue.pushGeneration(i, 0)
			require.Equal(t, i, <-received)
			synctest.Wait()
			require.Nil(t, queue.pending, "draining must release the backlog storage")
		}
		cancel()
		<-done
	})
}
