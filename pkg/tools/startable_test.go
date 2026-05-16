package tools_test

import (
	"context"
	"errors"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools"
)

// stubDescriber implements ToolSet and Describer.
type stubDescriber struct{ desc string }

func (s *stubDescriber) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *stubDescriber) Describe() string                            { return s.desc }

// stubToolSet implements ToolSet only (no Describer).
type stubToolSet struct{}

func (s *stubToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

// flappyToolSet implements ToolSet + Startable with a scripted sequence of errors.
// Each call to Start() consumes the next error from errs; nil means success.
type flappyToolSet struct {
	errs     []error
	callIdx  int
	startups int // number of successful Start() calls
}

func (f *flappyToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "flappy_tool"}}, nil
}

func (f *flappyToolSet) Start(_ context.Context) error {
	if f.callIdx < len(f.errs) {
		err := f.errs[f.callIdx]
		f.callIdx++
		if err != nil {
			return err
		}
	}
	f.startups++
	return nil
}

func (f *flappyToolSet) Stop(_ context.Context) error {
	return nil
}

func TestDescribeToolSet_UsesDescriber(t *testing.T) {
	t.Parallel()

	ts := &stubDescriber{desc: "mcp(ref=docker:github-official)"}
	assert.Check(t, is.Equal(tools.DescribeToolSet(ts), "mcp(ref=docker:github-official)"))
}

func TestDescribeToolSet_UnwrapsStartableAndUsesDescriber(t *testing.T) {
	t.Parallel()

	inner := &stubDescriber{desc: "mcp(stdio cmd=python args=-m,srv)"}
	wrapped := tools.NewStartable(inner)
	assert.Check(t, is.Equal(tools.DescribeToolSet(wrapped), "mcp(stdio cmd=python args=-m,srv)"))
}

func TestDescribeToolSet_FallsBackToTypeName(t *testing.T) {
	t.Parallel()

	ts := &stubToolSet{}
	assert.Check(t, is.Equal(tools.DescribeToolSet(ts), "*tools_test.stubToolSet"))
}

func TestDescribeToolSet_FallsBackToTypeNameWhenDescribeEmpty(t *testing.T) {
	t.Parallel()

	ts := &stubDescriber{desc: ""}
	assert.Check(t, is.Equal(tools.DescribeToolSet(ts), "*tools_test.stubDescriber"))
}

func TestDescribeToolSet_UnwrapsStartableAndFallsBackToTypeName(t *testing.T) {
	t.Parallel()

	inner := &stubToolSet{}
	wrapped := tools.NewStartable(inner)
	assert.Check(t, is.Equal(tools.DescribeToolSet(wrapped), "*tools_test.stubToolSet"))
}

// TestStartableToolSet_ShouldReportFailure_OncePerStreak verifies that
// ShouldReportFailure returns true exactly once per failure streak,
// suppressing duplicate warnings on repeated retries.
func TestStartableToolSet_ShouldReportFailure_OncePerStreak(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, errBoom, nil}}
	s := tools.NewStartable(f)

	// Turn 1: first failure — should report.
	assert.Check(t, s.Start(t.Context()) != nil, "expected error on turn 1")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "turn 1: first failure should be reported")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "turn 1: second call must return false")

	// Turn 2: second failure in same streak — must NOT report again.
	assert.Check(t, s.Start(t.Context()) != nil, "expected error on turn 2")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "turn 2: duplicate failure must not report")

	// Turn 3: success — silent recovery, no caller-visible event.
	assert.Check(t, s.Start(t.Context()) == nil, "expected success on turn 3")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "turn 3: success must not report a failure")
}

// TestStartableToolSet_RecoveryResetsStreak verifies that a successful
// Start() implicitly resets the failure streak: after a fail → succeed
// cycle, a fresh failure on the *next* streak is reported again.
func TestStartableToolSet_RecoveryResetsStreak(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, nil, errBoom}}
	s := tools.NewStartable(f)

	// Cycle 1: fail then recover.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true))

	assert.Check(t, s.Start(t.Context()) == nil)

	// Stop so we can attempt to start again — a successful Start() marks
	// the toolset as started, so subsequent Start() calls short-circuit.
	assert.Check(t, s.Stop(t.Context()) == nil)

	// Cycle 2: new failure must warn again, proving the recovery reset
	// the streak even though no caller signalled it.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "fresh failure after recovery must warn")
}

// TestStartableToolSet_StopResetsFailureState verifies that after a failure streak,
// an explicit Stop() clears all tracking so the next failure warns again.
func TestStartableToolSet_StopResetsFailureState(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, errBoom}}
	s := tools.NewStartable(f)

	// First failure: consume the warning.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true))

	// Stop resets state.
	assert.Check(t, s.Stop(t.Context()) == nil)

	// Second failure after Stop: must warn again.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "failure after Stop must produce fresh warning")
}
