package session

import (
	"sync"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestAddMessageUsageRecordConcurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			s.AddMessageUsageRecord("agent", "model", 0.1, &chat.Usage{InputTokens: 10, OutputTokens: 5})
		})
	}
	wg.Wait()
	if got := len(s.MessageUsageHistory); got != 100 {
		t.Errorf("expected 100 records, got %d", got)
	}
}

// TestCompactionInputConcurrent pins the data-race fix for the
// compactor: CompactionInput must read s.Messages under s.mu (via
// snapshotItems) so it stays safe against concurrent AddMessage and
// ApplyCompaction calls. Run with -race; without the lock the slice
// header read aliases the live backing array and the race detector
// flags the AddMessage append.
func TestCompactionInputConcurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			s.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
		})
		wg.Go(func() {
			_, _ = s.CompactionInput()
		})
	}
	// One concurrent ApplyCompaction-shaped write to exercise the same
	// lock from a writer that also bumps the cumulative token counts.
	wg.Go(func() {
		s.ApplyCompaction(0, 0, Item{Summary: "snap"})
	})
	wg.Wait()
}
