package backoff

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCalculate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt     int
		minExpected time.Duration
		maxExpected time.Duration
	}{
		{attempt: 0, minExpected: 180 * time.Millisecond, maxExpected: 220 * time.Millisecond},
		{attempt: 1, minExpected: 360 * time.Millisecond, maxExpected: 440 * time.Millisecond},
		{attempt: 2, minExpected: 720 * time.Millisecond, maxExpected: 880 * time.Millisecond},
		{attempt: 3, minExpected: 1440 * time.Millisecond, maxExpected: 1760 * time.Millisecond},
		{attempt: 10, minExpected: 1800 * time.Millisecond, maxExpected: 2200 * time.Millisecond}, // capped at 2s
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			t.Parallel()
			b := Calculate(tt.attempt)
			assert.GreaterOrEqual(t, b, tt.minExpected, "backoff should be at least %v", tt.minExpected)
			assert.LessOrEqual(t, b, tt.maxExpected, "backoff should be at most %v", tt.maxExpected)
		})
	}

	t.Run("negative attempt treated as 0", func(t *testing.T) {
		t.Parallel()
		b := Calculate(-1)
		assert.GreaterOrEqual(t, b, 180*time.Millisecond)
		assert.LessOrEqual(t, b, 220*time.Millisecond)
	})
}

func TestSleepWithContext(t *testing.T) {
	t.Parallel()

	t.Run("completes normally", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			completed := SleepWithContext(t.Context(), 10*time.Millisecond)

			assert.True(t, completed, "should complete normally")
			assert.Equal(t, 10*time.Millisecond, time.Since(start))
		})
	})

	t.Run("interrupted by context", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			time.AfterFunc(10*time.Millisecond, cancel)

			start := time.Now()
			completed := SleepWithContext(ctx, time.Second)

			assert.False(t, completed, "should be interrupted")
			assert.Equal(t, 10*time.Millisecond, time.Since(start))
		})
	})
}
