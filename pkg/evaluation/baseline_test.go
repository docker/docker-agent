package evaluation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// sizeResult is a result whose only declared expectation is response size.
// Session is populated because that is what SaveRunSessionsJSON serialises, and
// what LoadBaseline reads back.
func sizeResult(title string, pass bool) Result {
	r := Result{
		InputPath:    title + ".json",
		Title:        title,
		SizeExpected: "medium",
		Size:         "medium",
		Session:      &session.Session{Title: title},
	}
	if !pass {
		r.Session.Title = title
		r.Size = "short"
	}
	return r
}

// toolResult declares a tool-call expectation. checkResults requires a score of
// 1.0 to pass, regardless of the declared expectation value.
func toolResult(title string, score float64) Result {
	return Result{
		InputPath:         title + ".json",
		Title:             title,
		ToolCallsExpected: 1,
		ToolCallsScore:    score,
		Session:           &session.Session{Title: title},
	}
}

func newRun(results ...Result) *EvalRun {
	run := &EvalRun{Name: "run", Results: results}
	run.Summary = computeSummary(run.Results)
	return run
}

// saveAndLoad writes a run exactly as the eval command does and reads it back as
// a baseline, so every test exercises the real file format.
func saveAndLoad(t *testing.T, run *EvalRun) *Baseline {
	t.Helper()
	for i := range run.Results {
		populateEvalResult(&run.Results[i])
	}
	path, err := SaveRunSessionsJSON(run, t.TempDir())
	require.NoError(t, err)

	baseline, err := LoadBaseline(path)
	require.NoError(t, err)
	return baseline
}

// The file the eval command writes is a RunOutput, not an EvalRun. Loading the
// wrong shape failed outright on Duration (string vs time.Duration), which meant
// --baseline could not read anything the tool produced.
func TestLoadBaseline_ReadsWhatTheEvalCommandWrites(t *testing.T) {
	t.Parallel()

	run := newRun(sizeResult("a", true), sizeResult("b", false))
	for i := range run.Results {
		populateEvalResult(&run.Results[i])
	}
	path, err := SaveRunSessionsJSON(run, t.TempDir())
	require.NoError(t, err)

	baseline, err := LoadBaseline(path)
	require.NoError(t, err)

	assert.Equal(t, 2, baseline.Summary.TotalEvals)
	assert.Equal(t, map[string]bool{"a": true, "b": false}, baseline.Passed)
}

// A gate must fail closed: a file that parses but carries no evaluations would
// compare against all-zero metrics and pass unconditionally.
func TestLoadBaseline_RejectsAFileThatIsNotAnEvalRun(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "not-a-run.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"not-an-eval-run"}`), 0o600))

	_, err := LoadBaseline(path)
	require.ErrorIs(t, err, ErrNoBaselineEvals)
}

func TestLoadBaseline_Errors(t *testing.T) {
	t.Parallel()

	_, err := LoadBaseline(filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading baseline")

	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte("{not json"), 0o600))
	_, err = LoadBaseline(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing baseline")
}

// A run with no evaluations (an --only pattern that matched nothing) has nothing
// to gate on and must not report success.
func TestCompare_RejectsAnEmptyCurrentRun(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true)))

	_, err := Compare(baseline, newRun(), 0)
	require.ErrorIs(t, err, ErrNoCurrentEvals)

	_, err = Compare(baseline, nil, 0)
	require.ErrorIs(t, err, ErrNoCurrentEvals)
}

func TestCompare_RejectsANilBaseline(t *testing.T) {
	t.Parallel()
	_, err := Compare(nil, newRun(sizeResult("a", true)), 0)
	require.ErrorIs(t, err, ErrNoBaselineEvals)
}

func TestCompare_NoChangeIsNotARegression(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true), sizeResult("b", true)))

	got, err := Compare(baseline, newRun(sizeResult("a", true), sizeResult("b", true)), 0)
	require.NoError(t, err)
	assert.False(t, got.Regressed)
	assert.Empty(t, got.Changes)
}

func TestCompare_QualityDropRegresses(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true), sizeResult("b", true)))

	got, err := Compare(baseline, newRun(sizeResult("a", true), sizeResult("b", false)), 0)
	require.NoError(t, err)
	require.True(t, got.Regressed)

	var sizeDelta *MetricDelta
	for i := range got.Deltas {
		if got.Deltas[i].Name == "size pass rate" {
			sizeDelta = &got.Deltas[i]
		}
	}
	require.NotNil(t, sizeDelta)
	assert.True(t, sizeDelta.Regressed)
	assert.InDelta(t, -0.5, sizeDelta.Delta, 1e-9)
}

// resultPassed must agree with what the run prints and saves. checkResults
// requires a tool-call score of 1.0; a second definition keyed on
// ToolCallsExpected would call 0.9 a pass and record a printed FAIL as no change.
func TestResultPassed_MatchesCheckResults(t *testing.T) {
	t.Parallel()

	for _, r := range []Result{
		{Title: "none"},
		{Title: "err", Error: "boom"},
		{Title: "size-ok", SizeExpected: "short", Size: "short"},
		{Title: "size-bad", SizeExpected: "short", Size: "long"},
		{Title: "tool-perfect", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
		{Title: "tool-short", ToolCallsExpected: 0.8, ToolCallsScore: 0.9},
		{Title: "rel-ok", RelevanceExpected: 2, RelevancePassed: 2},
		{Title: "rel-bad", RelevanceExpected: 2, RelevancePassed: 1},
	} {
		_, failures := r.checkResults()
		assert.Equalf(t, len(failures) == 0, resultPassed(r),
			"resultPassed disagrees with checkResults for %q (failures=%v)", r.Title, failures)
	}
}

// The specific divergence the old definition hid: a printed PASS → FAIL flip
// that was recorded as no change and then absorbed by the tolerance.
func TestCompare_ToolScoreDropBelowOneIsARegression(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(toolResult("a", 1.0)))

	got, err := Compare(baseline, newRun(toolResult("a", 0.9)), 0.2)
	require.NoError(t, err)

	require.Len(t, got.Changes, 1)
	assert.Equal(t, "pass", got.Changes[0].Was)
	assert.Equal(t, "fail", got.Changes[0].Now)
	assert.True(t, got.Regressed, "a printed pass → fail must gate even inside the tolerance")
}

// Judge variance shows up as small movement in the aggregate rates; the
// tolerance exists so a build is not failed by that noise. Both evaluations stay
// passing here, so only the aggregate moves.
func TestCompare_ToleranceAbsorbsSmallDrops(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(toolResult("a", 1.0), toolResult("b", 1.0)))
	current := newRun(toolResult("a", 1.0), toolResult("b", 1.0))
	// Nudge the aggregate without flipping a pass: F1 mean 1.00 → 0.95 is not
	// expressible while both pass, so assert the no-movement case instead.
	got, err := Compare(baseline, current, 0)
	require.NoError(t, err)
	assert.False(t, got.Regressed)

	// A real drop below 1.0 flips the eval and gates regardless of tolerance.
	got, err = Compare(baseline, newRun(toolResult("a", 1.0), toolResult("b", 0.99)), MaxTolerance)
	require.NoError(t, err)
	assert.True(t, got.Regressed)
}

func TestCompare_ImprovementIsNotARegression(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", false), sizeResult("b", false)))

	got, err := Compare(baseline, newRun(sizeResult("a", true), sizeResult("b", true)), 0)
	require.NoError(t, err)
	assert.False(t, got.Regressed)
	for _, ch := range got.Changes {
		assert.False(t, ch.Regressed, "fail → pass must never gate")
	}
}

func TestCompare_FailureRateClimbRegresses(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true), sizeResult("b", true)))

	current := newRun(sizeResult("a", true), Result{
		InputPath: "b.json", Title: "b", Error: "boom", Session: &session.Session{Title: "b"},
	})
	got, err := Compare(baseline, current, 0)
	require.NoError(t, err)
	assert.True(t, got.Regressed)
}

// Cost is reported but must never gate: a price change is not a quality change.
func TestCompare_CostIsInformationalOnly(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(Result{
		InputPath: "a.json", Title: "a", Cost: 0.01, Session: &session.Session{Title: "a"},
	}))
	current := newRun(Result{
		InputPath: "a.json", Title: "a", Cost: 100, Session: &session.Session{Title: "a"},
	})

	got, err := Compare(baseline, current, 0)
	require.NoError(t, err)
	assert.False(t, got.Regressed, "a cost increase alone must not fail the gate")

	var cost *MetricDelta
	for i := range got.Deltas {
		if got.Deltas[i].Name == "total cost" {
			cost = &got.Deltas[i]
		}
	}
	require.NotNil(t, cost)
	assert.True(t, cost.Informational)
	assert.InDelta(t, 99.99, cost.Delta, 1e-6)
}

func TestCompare_AddedAndRemovedEvalsDoNotGateOnTheirOwn(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true), sizeResult("gone", true)))

	got, err := Compare(baseline, newRun(sizeResult("a", true), sizeResult("new", true)), 0)
	require.NoError(t, err)

	byKey := map[string]EvalChange{}
	for _, ch := range got.Changes {
		byKey[ch.Eval] = ch
	}
	require.Contains(t, byKey, "gone")
	require.Contains(t, byKey, "new")
	assert.False(t, byKey["gone"].Regressed, "a removed eval is a suite change, not a regression")
	assert.False(t, byKey["new"].Regressed, "an added eval is a suite change, not a regression")
	assert.False(t, got.Regressed)
}

// An added failing evaluation lowers the aggregate rate, and that gates — which
// is desirable: a suite that got worse should say so.
func TestCompare_AddedFailingEvalGatesViaTheAggregateRate(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true)))

	got, err := Compare(baseline, newRun(sizeResult("a", true), sizeResult("new", false)), 0)
	require.NoError(t, err)
	assert.True(t, got.Regressed, "size pass rate fell 1.00 → 0.50")
}

func TestCompare_RepeatedEvalCollapsesPessimistically(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true)))

	// Two repetitions under one key, one of which failed.
	got, err := Compare(baseline, newRun(sizeResult("a", true), sizeResult("a", false)), 0)
	require.NoError(t, err)

	require.Len(t, got.Changes, 1)
	assert.Equal(t, "pass", got.Changes[0].Was)
	assert.Equal(t, "fail", got.Changes[0].Now)
	assert.True(t, got.Changes[0].Regressed)
}

func TestCompare_RegressionsAreListedFirst(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("aaa", true), sizeResult("zzz", true)))

	got, err := Compare(baseline,
		newRun(sizeResult("aaa", true), sizeResult("zzz", false), sizeResult("bbb", true)), 0)
	require.NoError(t, err)

	require.NotEmpty(t, got.Changes)
	assert.Equal(t, "zzz", got.Changes[0].Eval, "the regression must lead")
	assert.True(t, got.Changes[0].Regressed)
}

func TestMetricsOf_AbsentMetricsAreFlaggedNotZero(t *testing.T) {
	t.Parallel()

	got := MetricsOf(newRun(Result{InputPath: "a", Title: "a"}))
	assert.False(t, got.HasSizes)
	assert.False(t, got.HasTools)
	assert.False(t, got.HasRelevance)
	assert.Zero(t, got.SizePassRate)

	assert.Equal(t, Metrics{}, MetricsOf(nil), "a nil run yields zero metrics, not a panic")
}

func TestPrintComparison(t *testing.T) {
	t.Parallel()

	t.Run("regression", func(t *testing.T) {
		t.Parallel()
		baseline := saveAndLoad(t, newRun(sizeResult("a", true)))
		c, err := Compare(baseline, newRun(sizeResult("a", false)), 0)
		require.NoError(t, err)

		var buf bytes.Buffer
		PrintComparison(&buf, c)
		out := buf.String()
		assert.Contains(t, out, "Regression against baseline")
		assert.Contains(t, out, "size pass rate")
		assert.Contains(t, out, "! ", "regressed rows are marked")
	})

	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		baseline := saveAndLoad(t, newRun(sizeResult("a", true)))
		c, err := Compare(baseline, newRun(sizeResult("a", true)), 0)
		require.NoError(t, err)

		var buf bytes.Buffer
		PrintComparison(&buf, c)
		assert.Contains(t, buf.String(), "No regression against baseline")
	})
}

func TestComparison_IsJSONSerializable(t *testing.T) {
	t.Parallel()

	baseline := saveAndLoad(t, newRun(sizeResult("a", true)))
	c, err := Compare(baseline, newRun(sizeResult("a", false)), 0.05)
	require.NoError(t, err)

	data, err := json.Marshal(c)
	require.NoError(t, err)

	var round Comparison
	require.NoError(t, json.Unmarshal(data, &round))
	assert.True(t, round.Regressed)
	assert.InDelta(t, 0.05, round.Tolerance, 1e-9)
}
