package tools

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

const (
	startBackoffBase = 15 * time.Second
	startBackoffMax  = 5 * time.Minute
)

// startBackoffRetryable reports whether err warrants pacing the next start
// attempt. Two independent categories arm the gate:
//
//   - lifecycle.ErrCrashLooping: the supervisor itself already judged this a
//     sustained crash loop (see lifecycle.Supervisor's CrashLoop policy) and
//     is reporting it through Start instead of reconnecting immediately.
//     Checked directly via errors.Is — no StatusError involved, because the
//     supervisor already did the one-off-vs-loop judgment; the gate only
//     needs to pace the retry. A bare lifecycle.ErrServerCrashed that hasn't
//     (yet) escalated to ErrCrashLooping does NOT arm: see "deliberately
//     excluded" below.
//   - a retryable HTTP status carried by a *modelerrors.StatusError: 429,
//     408, 500, 502, 503, 504, or 529 (see modelerrors.isRetryableStatusCode).
//     This is a fixed enumeration, not a full 5xx range — codes such as 501,
//     505, or the Cloudflare 520-527 family do NOT arm the gate. Only a
//     *modelerrors.StatusError in the error chain arms this branch; plain
//     network errors and the regex fallback in RetryableHTTPStatus are
//     intentionally excluded so port numbers, PIDs, and chunk counters in
//     plain error text cannot arm the gate. A StatusError wins even when
//     context.DeadlineExceeded is also in the chain.
//
// Deliberately excluded from arming (these must never pace):
//   - A bare lifecycle.ErrServerCrashed not (yet) escalated to
//     ErrCrashLooping: a single crash is the supervisor's own restart
//     policy's job (fast, unpaced reconnect), not this gate's — pacing it
//     here too would double up on the supervisor's own backoff and delay a
//     legitimate one-off recovery.
//   - lifecycle.ErrServerUnavailable: missing binary / process-not-found — fast-retry.
//   - lifecycle.ErrTransport: connection refused / no such host — fast-retry.
//   - lifecycle.ErrAuthRequired / ErrCapabilityMissing: permanent — fail promptly.
//   - lifecycle.ErrInitTimeout, ErrSessionMissing: transient, handled by the
//     supervisor's own reconnect policy without per-turn pacing.
//   - Plain error strings: excluded to avoid false positives on numeric
//     patterns in port numbers or counters.
func startBackoffRetryable(err error) bool {
	if errors.Is(err, lifecycle.ErrCrashLooping) {
		return true
	}
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

// retryAfterHint extracts the Retry-After duration from a *StatusError in the
// chain. Returns 0 if absent or if the error is not a *StatusError.
func retryAfterHint(err error) time.Duration {
	var se *modelerrors.StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}
