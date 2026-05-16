package lifecycle

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
)

// Classify maps a transport-level error (stdio MCP, remote MCP, LSP) to one
// of the typed sentinels in this package, wrapping it so errors.Is matches
// both the sentinel and the original error.
//
// Already-classified errors (any wrapping a sentinel via errors.Is) are
// returned unchanged. Unknown errors are returned as-is so callers can
// decide their own policy.
//
// Substring matching is used as a last resort because some upstream SDKs
// wrap their errors with %v (which drops the chain).
func Classify(err error) error {
	if err == nil {
		return nil
	}
	if isClassified(err) {
		return err
	}

	switch {
	case errors.Is(err, exec.ErrNotFound),
		errors.Is(err, os.ErrNotExist),
		errors.Is(err, io.EOF):
		return wrap(ErrServerUnavailable, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return wrap(ErrTransport, err)
	}

	return classifyByMessage(err)
}

// isClassified reports whether err already wraps one of the package
// sentinels. Used to make Classify idempotent.
func isClassified(err error) bool {
	for _, s := range []error{
		ErrServerUnavailable, ErrServerCrashed, ErrInitTimeout,
		ErrInitNotification, ErrCapabilityMissing, ErrAuthRequired,
		ErrSessionMissing, ErrTransport,
	} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// classifyByMessage matches well-known substrings emitted by upstream SDKs
// that wrap errors with %v (dropping the chain). Returns err unchanged
// when no pattern matches.
func classifyByMessage(err error) error {
	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(lower, "failed to send initialized notification"):
		return wrap(ErrInitNotification, err)
	case strings.Contains(lower, "session missing"),
		strings.Contains(lower, "session not found"):
		return wrap(ErrSessionMissing, err)
	case strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "broken pipe"),
		strings.Contains(msg, "EOF"):
		return wrap(ErrTransport, err)
	}
	return err
}

// IsTransient reports whether err wraps a sentinel that warrants a retry.
func IsTransient(err error) bool {
	for _, s := range []error{
		ErrTransport, ErrServerUnavailable, ErrServerCrashed,
		ErrInitTimeout, ErrInitNotification, ErrSessionMissing,
	} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// IsPermanent reports whether err wraps a sentinel that must NOT be retried
// (currently ErrCapabilityMissing and ErrAuthRequired).
func IsPermanent(err error) bool {
	return errors.Is(err, ErrCapabilityMissing) || errors.Is(err, ErrAuthRequired)
}

// wrap returns an error satisfying errors.Is for both sentinel and
// underlying, using Go 1.20+ multi-%w support.
func wrap(sentinel, underlying error) error {
	return fmt.Errorf("%w: %w", sentinel, underlying)
}
