package tools

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

// failureStreak implements once-per-streak warning de-duplication. A streak
// begins on the first fail() and ends on reset() (a success or a Stop).
// shouldReport() returns true exactly once per streak — for the first failure —
// so repeated failures don't re-queue duplicate warnings.
type failureStreak struct {
	active  bool // true between the first failure and the next reset
	pending bool // true if the current streak's first failure is unreported
}

// startReporter is implemented by toolsets whose live lifecycle state can be
// queried independently of the StartableToolSet wrapper's latched state.
type startReporter interface {
	IsStarted() bool
}

func (f *failureStreak) fail() {
	if !f.active {
		f.active = true
		f.pending = true
	}
}

func (f *failureStreak) reset() {
	f.active = false
	f.pending = false
}

func (f *failureStreak) shouldReport() bool {
	if !f.pending {
		return false
	}
	f.pending = false
	return true
}

// StartableToolSet wraps a ToolSet with lazy, single-flight start semantics.
// This is the canonical way to manage toolset lifecycle.
//
// It also de-duplicates failure warnings: when Start() fails repeatedly
// (e.g. an MCP server is down), only the *first* failure of each streak is
// reported via ShouldReportFailure(). A successful Start() automatically
// clears the streak, so a future failure is again reported as fresh — no
// caller-visible "recovery" event is needed. The same once-per-streak guard
// applies to Tools() listing failures via ShouldReportListFailure(); a remote
// MCP server stuck returning "toolset not started" therefore surfaces a single
// warning per streak instead of one on every conversation turn.
type StartableToolSet struct {
	ToolSet

	mu          sync.Mutex
	started     bool
	startStreak failureStreak // Start() failures
	listStreak  failureStreak // Tools() listing failures
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
func (s *StartableToolSet) Start(ctx context.Context) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	recovering := false
	if s.started {
		if reporter, ok := As[startReporter](s.ToolSet); !ok || reporter.IsStarted() {
			return nil
		}
		s.started = false
		recovering = true
	}

	// Span the toolset startup — MCP handshake, OAuth probes,
	// tool discovery, etc. can take seconds to minutes and the
	// "tools loading…" UI was previously unattributable. Only
	// fires when the toolset has work to do (Restartable on a
	// recovering run, or Startable on a cold start); cheap
	// toolsets without either skip the span entirely.
	//
	// Unwrap once so the kind attribute names the underlying toolset
	// (e.g. *mcp.Toolset, *builtin.ShellTool) instead of the
	// *tools.namedToolSet wrapper that every toolset gets in the
	// registry — same pattern DescribeToolSet uses.
	inner := s.ToolSet
	if u, ok := inner.(Unwrapper); ok {
		inner = u.Unwrap()
	}
	if restarter, hasRestarter := As[Restartable](s.ToolSet); recovering && hasRestarter {
		ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/tools").Start(
			ctx,
			"toolset.start",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String("cagent.toolset.kind", fmt.Sprintf("%T", inner))),
		)
		defer func() {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}()
		if err := restarter.Restart(ctx); err != nil {
			s.startStreak.fail()
			return err
		}
	} else if startable, ok := As[Startable](s.ToolSet); ok {
		ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/tools").Start(
			ctx,
			"toolset.start",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String("cagent.toolset.kind", fmt.Sprintf("%T", inner))),
		)
		defer func() {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}()
		if err := startable.Start(ctx); err != nil {
			s.startStreak.fail()
			return err
		}
	}

	// Successful start: clear the streak so any future failure is reported
	// as fresh. This is the recovery path — it is intentionally silent.
	s.started = true
	s.startStreak.reset()
	return nil
}

// Tools lists the underlying toolset's tools and tracks listing-failure
// streaks so callers can de-duplicate warnings via ShouldReportListFailure().
// A successful listing clears the streak so a future failure is reported as
// fresh.
func (s *StartableToolSet) Tools(ctx context.Context) ([]Tool, error) {
	ta, err := s.ToolSet.Tools(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.listStreak.fail()
		return nil, err
	}

	s.listStreak.reset()
	return ta, nil
}

// Stop stops the toolset if it implements Startable and resets
// the started flag so that a subsequent Start will re-initialize.
func (s *StartableToolSet) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.started = false
	s.startStreak.reset()
	s.listStreak.reset()
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
	return s.startStreak.shouldReport()
}

// ShouldReportListFailure returns true exactly once per Tools() listing-failure
// streak — after the first failed listing and before the streak ends (a
// successful Tools() or Stop()). Subsequent calls return false until a new
// streak begins. Calling it when no failure is pending always returns false.
func (s *StartableToolSet) ShouldReportListFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listStreak.shouldReport()
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
