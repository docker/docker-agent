package concurrent

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMapSlice(t *testing.T) {
	t.Parallel()

	got := MapSlice([]int{1, 2, 3, 4}, func(v int) int { return v * v })
	assert.Equal(t, []int{1, 4, 9, 16}, got)
}

func TestMapSlice_Empty(t *testing.T) {
	t.Parallel()

	got := MapSlice(nil, func(int) int { t.Fatal("f must not be called"); return 0 })
	assert.Empty(t, got)
}

func TestMapSlice_RunsConcurrently(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const n = 8
		var running atomic.Int32
		start := make(chan struct{})

		// Every call blocks until all n are running — only possible if MapSlice
		// is concurrent. The timeout makes a sequential regression fail fast
		// instead of hanging until the package deadline.
		timeout := time.After(10 * time.Second)
		got := MapSlice(make([]struct{}, n), func(struct{}) int {
			if running.Add(1) == n {
				close(start)
			}
			select {
			case <-start:
				return 1
			case <-timeout:
				t.Error("MapSlice did not run all calls concurrently")
				return 0
			}
		})

		assert.Len(t, got, n)
	})
}
