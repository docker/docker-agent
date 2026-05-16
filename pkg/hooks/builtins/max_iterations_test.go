package builtins_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestMaxIterationsTripsAfterLimit verifies the happy path: with a
// limit of 3, the first three iterations are no-ops and the fourth
// returns a block decision. The reason carries the configured limit
// so the runtime's user-facing Error event explains why the run
// stopped.
func TestMaxIterationsTripsAfterLimit(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.MaxIterations)
	args := []string{"3"}

	for i := 1; i <= 3; i++ {
		out, err := fn(t.Context(), &hooks.Input{Iteration: i}, args)
		require.NoErrorf(t, err, "iteration %d must not error", i)
		require.Nilf(t, out, "iteration %d (within limit) must not trip", i)
	}

	out, err := fn(t.Context(), &hooks.Input{Iteration: 4}, args)
	require.NoError(t, err)
	require.NotNil(t, out, "iteration 4 (over limit) must trip")
	assert.Equal(t, hooks.DecisionBlockValue, out.Decision)
	assert.Contains(t, out.Reason, "3", "reason must include the configured limit")
}

// TestMaxIterationsNoOpWithoutValidLimit documents the lenient-args
// contract: a missing, non-integer, zero, or negative limit makes
// the builtin a no-op rather than tripping (the safer default for a
// misconfigured YAML).
func TestMaxIterationsNoOpWithoutValidLimit(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		nil,
		{},
		{"abc"},
		{"0"},
		{"-1"},
	}
	for _, args := range cases {
		fn := lookup(t, builtins.MaxIterations)
		// Drive 50 iterations — if the builtin were tripping erroneously,
		// at least one of these would return a non-nil Output.
		for i := 1; i <= 50; i++ {
			out, err := fn(t.Context(), &hooks.Input{Iteration: i}, args)
			require.NoError(t, err)
			require.Nilf(t, out, "args=%v: must never trip", args)
		}
	}
}

// TestMaxIterationsIgnoresIncompleteInput pins the defensive guard:
// missing or non-positive Iteration produces no output. This protects
// against future dispatch changes where an edge case might fire
// before_llm_call without that field populated.
func TestMaxIterationsIgnoresIncompleteInput(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.MaxIterations)

	out, err := fn(t.Context(), nil, []string{"1"})
	require.NoError(t, err)
	assert.Nil(t, out)

	// Iteration=0 (zero value) means "not populated" — the runtime
	// always supplies a 1-based counter on before_llm_call.
	out, err = fn(t.Context(), &hooks.Input{}, []string{"1"})
	require.NoError(t, err)
	assert.Nil(t, out)
}
