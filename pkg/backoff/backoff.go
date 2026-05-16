// Package backoff provides exponential backoff calculation and
// context-aware sleep utilities.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Configuration constants for exponential backoff.
const (
	baseDelay = 200 * time.Millisecond
	maxDelay  = 2 * time.Second
	factor    = 2.0
	jitter    = 0.1

	// MaxRetryAfterWait caps how long we'll honor a Retry-After header to prevent
	// a misbehaving server from blocking the agent for an unreasonable amount of time.
	MaxRetryAfterWait = 60 * time.Second
)

// Calculate returns the backoff duration for a given attempt (0-indexed).
// Uses exponential backoff with jitter.
func Calculate(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	// Calculate exponential delay
	delay := float64(baseDelay)
	for range attempt {
		delay *= factor
	}

	// Cap at max delay
	if delay > float64(maxDelay) {
		delay = float64(maxDelay)
	}

	// Add jitter (±10%)
	j := delay * jitter * (2*rand.Float64() - 1)
	delay += j

	return time.Duration(delay)
}

// SleepWithContext sleeps for the specified duration, returning early if context is cancelled.
// Returns true if the sleep completed, false if it was interrupted by context cancellation.
func SleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
