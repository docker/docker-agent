package root

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/evaluation"
	"github.com/docker/docker-agent/pkg/session"
)

// sizeRun builds a run in the shape the eval command produces, including the
// session that carries the saved pass/fail flag.
func sizeRun(pass bool) *evaluation.EvalRun {
	r := evaluation.Result{
		InputPath:    "a.json",
		Title:        "a",
		SizeExpected: "medium",
		Size:         "medium",
		Session:      &session.Session{Title: "a"},
	}
	if !pass {
		r.Size = "short"
	}
	return &evaluation.EvalRun{Name: "run", Results: []evaluation.Result{r}}
}

// saveBaseline writes a run exactly as `docker agent eval` does.
func saveBaseline(t *testing.T, run *evaluation.EvalRun) string {
	t.Helper()
	path, err := evaluation.SaveRunSessionsJSON(run, t.TempDir())
	require.NoError(t, err)
	return path
}

func TestCheckBaseline_NoBaselineIsANoOp(t *testing.T) {
	t.Parallel()

	f := &evalFlags{}
	var buf bytes.Buffer
	require.NoError(t, f.checkBaseline(&buf, sizeRun(false)))
	assert.Empty(t, buf.String(), "without --baseline nothing is compared or printed")
}

func TestCheckBaseline_RegressionReturnsAnError(t *testing.T) {
	t.Parallel()

	f := &evalFlags{baseline: saveBaseline(t, sizeRun(true))}
	var buf bytes.Buffer
	err := f.checkBaseline(&buf, sizeRun(false))

	require.Error(t, err, "a regression must surface as a non-zero exit")
	assert.Contains(t, err.Error(), "regressed against baseline")
	assert.Contains(t, buf.String(), "Regression against baseline")
}

func TestCheckBaseline_NoRegressionSucceeds(t *testing.T) {
	t.Parallel()

	f := &evalFlags{baseline: saveBaseline(t, sizeRun(true))}
	var buf bytes.Buffer
	require.NoError(t, f.checkBaseline(&buf, sizeRun(true)))
	assert.Contains(t, buf.String(), "No regression against baseline")
}

// The gate must fail closed rather than reporting success against a baseline it
// cannot actually compare with.
func TestCheckBaseline_FailsClosedOnAnUnusableBaseline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "not-a-run.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"not-an-eval-run"}`), 0o600))

	f := &evalFlags{baseline: path}
	var buf bytes.Buffer
	err := f.checkBaseline(&buf, sizeRun(false))

	require.ErrorIs(t, err, evaluation.ErrNoBaselineEvals)
	assert.NotContains(t, buf.String(), "No regression",
		"an unusable baseline must never print a reassuring verdict")
}

func TestCheckBaseline_MissingBaselineFileIsAnError(t *testing.T) {
	t.Parallel()

	f := &evalFlags{baseline: filepath.Join(t.TempDir(), "nope.json")}
	var buf bytes.Buffer
	err := f.checkBaseline(&buf, sizeRun(true))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading baseline",
		"a bad --baseline path must fail loudly rather than silently skipping the gate")
}

func TestEvalCmd_BaselineFlagsAreRegistered(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NotNil(t, cmd.Flags().Lookup("baseline"))

	tolerance := cmd.Flags().Lookup("regression-tolerance")
	require.NotNil(t, tolerance)
	assert.Equal(t, "0", tolerance.DefValue, "the default gates any drop")
}

// A tolerance above 1 cannot be met by any rate movement, so it silently
// disables the aggregate gate. Rejecting it is a startup error, not a surprise
// discovered when a regression sails through.
func TestEvalCmd_RejectsTooLargeTolerance(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Set("regression-tolerance", "10"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Args(cmd, []string{"agent.yaml"})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"agent.yaml"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--regression-tolerance must be between 0 and 1")
}
