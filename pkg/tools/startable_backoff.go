package tools

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

const (
	startBackoffBase = 15 * time.Second
	startBackoffMax  = 5 * time.Minute
)

// startBackoffRetryable reports whether err carries a retryable HTTP status
// (429, 408, or 5xx). Requires a *modelerrors.StatusError anywhere in the
// chain; plain network errors and the regex fallback in RetryableHTTPStatus
// are intentionally excluded to avoid arming the gate on port numbers or
// chunk counters that match the \b[45]\d{2}\b pattern.
// A StatusError wins even when context.DeadlineExceeded is also in the chain.
func startBackoffRetryable(err error) bool {
	var se *modelerrors.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return modelerrors.RetryableHTTPStatus(se)
}

// computeStartBackoff returns the exponential backoff delay for attempt
// (1-indexed), capped at startBackoffMax with additive 0–20% jitter.
// jitterFn overrides the default additiveJitter when non-nil.
func computeStartBackoff(attempt int, jitterFn func(time.Duration) time.Duration) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}

	// Double until cap; overflow-safe because we stop at startBackoffMax.
	nominal := startBackoffBase
	for i := 1; i < attempt; i++ {
		nominal *= 2
		if nominal >= startBackoffMax {
			nominal = startBackoffMax
			break
		}
	}

	if jitterFn != nil {
		return jitterFn(nominal)
	}
	return additiveJitter(nominal)
}

// additiveJitter returns d + rand.N([0, d/5]), giving delay ∈ [d, 1.2d].
func additiveJitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.N(int64(d/5)+1))
}
