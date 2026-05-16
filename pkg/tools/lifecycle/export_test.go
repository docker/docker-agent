package lifecycle

import "time"

// ExportedBackoffDelay exposes Backoff.delay for tests in the _test
// package.
func ExportedBackoffDelay(b Backoff, attempt int, randFloat func() float64) time.Duration {
	return b.delay(attempt, randFloat)
}
