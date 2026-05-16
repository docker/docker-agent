package tools

import (
	"context"
	"fmt"
	"sync"
)

// Describer can be implemented by a ToolSet to provide a short, user-visible
// description that uniquely identifies the toolset instance (e.g. for use in
// error messages and warnings). The string must never contain secrets.
type Describer interface {
	Describe() string
}

// DescribeToolSet returns a short description for ts suitable for user-visible
// messages. It walks the wrapper chain (e.g. through WithName /
// StartableToolSet) so any inner Describer is reachable; falls back to
// the Go type name when no inner toolset implements Describer.
func DescribeToolSet(ts ToolSet) string {
	if d, ok := As[Describer](ts); ok {
		if desc := d.Describe(); desc != "" {
			return desc
		}
	}
	// Unwrap once for the type-name fallback so wrappers don't show up
	// as e.g. "*tools.namedToolSet".
	if u, ok := ts.(Unwrapper); ok {
		ts = u.Unwrap()
	}
	return fmt.Sprintf("%T", ts)
}

// StartableToolSet wraps a ToolSet with lazy, single-flight start semantics.
// This is the canonical way to manage toolset lifecycle.
//
// It also de-duplicates start-failure warnings: when Start() fails repeatedly
// (e.g. an MCP server is down), only the *first* failure of each streak is
// reported via ShouldReportFailure(). A successful Start() automatically
// clears the streak, so a future failure is again reported as fresh — no
// caller-visible "recovery" event is needed.
type StartableToolSet struct {
	ToolSet

	mu              sync.Mutex
	started         bool
	inFailureStreak bool // true between the first failed Start and the next successful Start (or Stop)
	pendingWarning  bool // true if the current streak's first failure has not yet been reported
}

// NewStartable wraps a ToolSet for lazy initialization.
func NewStartable(ts ToolSet) *StartableToolSet {
	return &StartableToolSet{ToolSet: ts}
}

// IsStarted returns whether the toolset has been successfully started.
// For toolsets that don't implement Startable, this always returns true.
func (s *StartableToolSet) IsStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Start starts the toolset with single-flight semantics.
// Concurrent callers block until the start attempt completes.
// If start fails, a future call will retry.
// If the underlying toolset doesn't implement Startable, this is a no-op.
func (s *StartableToolSet) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	if startable, ok := As[Startable](s.ToolSet); ok {
		if err := startable.Start(ctx); err != nil {
			// Queue a warning ONLY on the first failure of a streak so
			// repeated retries don't re-queue duplicate warnings.
			if !s.inFailureStreak {
				s.inFailureStreak = true
				s.pendingWarning = true
			}
			return err
		}
	}

	// Successful start: clear the streak so any future failure is reported
	// as fresh. This is the recovery path — it is intentionally silent.
	s.started = true
	s.inFailureStreak = false
	s.pendingWarning = false
	return nil
}

// Stop stops the toolset if it implements Startable and resets
// the started flag so that a subsequent Start will re-initialize.
func (s *StartableToolSet) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.started = false
	s.inFailureStreak = false
	s.pendingWarning = false
	if startable, ok := As[Startable](s.ToolSet); ok {
		return startable.Stop(ctx)
	}
	return nil
}

// ShouldReportFailure returns true exactly once per failure streak — after
// the first failed Start() and before the streak ends (a successful
// Start() or Stop()). Subsequent calls return false until a new streak
// begins. Calling it when no failure is pending always returns false.
func (s *StartableToolSet) ShouldReportFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pendingWarning {
		return false
	}
	s.pendingWarning = false
	return true
}

// Unwrap returns the underlying ToolSet.
func (s *StartableToolSet) Unwrap() ToolSet {
	return s.ToolSet
}

// Unwrapper is implemented by toolset wrappers that decorate another ToolSet.
// This allows As to walk the wrapper chain and find inner capabilities.
type Unwrapper interface {
	Unwrap() ToolSet
}

// As performs a type assertion on a ToolSet, walking the wrapper chain if needed.
// It checks the outermost toolset first, then recursively unwraps through any
// Unwrapper implementations (including StartableToolSet and decorator wrappers)
// until it finds a match or reaches the end of the chain.
//
// Example:
//
//	if pp, ok := tools.As[tools.PromptProvider](toolset); ok {
//	    prompts, _ := pp.ListPrompts(ctx)
//	}
func As[T any](ts ToolSet) (T, bool) {
	for ts != nil {
		if result, ok := ts.(T); ok {
			return result, true
		}
		if u, ok := ts.(Unwrapper); ok {
			ts = u.Unwrap()
		} else {
			break
		}
	}
	var zero T
	return zero, false
}
