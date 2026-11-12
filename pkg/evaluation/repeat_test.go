package evaluation

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeRepeatDiagnostics(t *testing.T) {
	t.Parallel()
	results := []Result{
		{InputPath: "stable.json", Title: "Same title #1", Cost: 0.01},
		{InputPath: "stable.json", Title: "Same title #2", Cost: 0.01},
		{InputPath: "stable.json", Title: "Same title #3", Cost: 0.01},
		{InputPath: "flaky.json", Title: "Same title #1", Cost: 0.01},
		{InputPath: "flaky.json", Title: "Same title #2", Cost: 0.02, SizeExpected: "M", Size: "S"},
		{InputPath: "flaky.json", Title: "Same title #3", Cost: 0.09},
		{InputPath: "failing.json", Title: "Fails #1", Error: "boom"},
		{InputPath: "failing.json", Title: "Fails #2", Error: "boom"},
		{InputPath: "failing.json", Title: "Fails #3", Error: "boom"},
		{InputPath: "partial.json", Title: "Partial #1"},
		{},
	}
	original := slices.Clone(results)
	evals := computeRepeatDiagnostics(results, 3)
	require.Len(t, evals, 4)
	assert.Equal(t, original, results, "diagnostics must not reorder or modify results")

	flaky := evals[0]
	assert.Equal(t, "flaky.json", flaky.InputPath)
	assert.Equal(t, "flaky", flaky.Status)
	assert.Equal(t, 2, flaky.Passed)
	assert.Equal(t, 3, flaky.Total)
	assert.Zero(t, flaky.Errors)
	assert.Equal(t, []string{"Same title #2"}, flaky.FailedRuns)
	require.NotNil(t, flaky.Cost)
	assert.Equal(t, 3, flaky.Cost.Samples)
	assert.InDelta(t, 0.02, flaky.Cost.Median, 1e-9)
	assert.InDelta(t, 0.01, flaky.Cost.Min, 1e-9)
	assert.InDelta(t, 0.09, flaky.Cost.Max, 1e-9)
	assert.Equal(t, []RepeatCostOutlier{{Title: "Same title #3", Cost: 0.09}}, flaky.Cost.Outliers)

	failing := evals[1]
	assert.Equal(t, "failing", failing.Status)
	assert.Zero(t, failing.Passed)
	assert.Equal(t, 3, failing.Errors)
	assert.Nil(t, failing.Cost)
	assert.Equal(t, []string{"Fails #1", "Fails #2", "Fails #3"}, failing.FailedRuns)

	partial := evals[2]
	assert.Equal(t, "incomplete", partial.Status)
	assert.Equal(t, 1, partial.Passed)
	assert.Equal(t, 1, partial.Total)
	assert.Equal(t, "stable", evals[3].Status)

	slices.Reverse(results)
	assert.Equal(t, evals, computeRepeatDiagnostics(results, 3), "output must be deterministic")
}

func TestComputeRepeatCost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		costs   []float64
		median  float64
		outlier bool
	}{
		{name: "one sample", costs: []float64{0.01}, median: 0.01},
		{name: "two samples", costs: []float64{0.01, 0.09}, median: 0.05},
		{name: "even median", costs: []float64{0.01, 0.02, 0.03, 0.09}, median: 0.025, outlier: true},
		{name: "at threshold", costs: []float64{0.01, 0.01, 0.02}, median: 0.01},
		{name: "above threshold", costs: []float64{0.01, 0.01, 0.021}, median: 0.01, outlier: true},
		{name: "zero costs", costs: []float64{0, 0, 0}, median: 0},
		{name: "zero median", costs: []float64{0, 0, 0.09}, median: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var runs []Result
			for _, cost := range tc.costs {
				runs = append(runs, Result{Cost: cost})
			}
			m := computeRepeatCost(runs)
			require.NotNil(t, m)
			assert.InDelta(t, tc.median, m.Median, 1e-9)
			assert.Equal(t, len(tc.costs), m.Samples)
			assert.Equal(t, tc.outlier, len(m.Outliers) > 0)
		})
	}
	assert.Nil(t, computeRepeatCost(nil))
}

func TestRepeatDiagnosticsExecutionErrors(t *testing.T) {
	t.Parallel()
	m := computeRepeatMetrics([]Result{
		{InputPath: "a.json", Title: "A #1", Cost: 0.01},
		{InputPath: "a.json", Title: "A #2", Cost: 100, Error: "judge failed"},
		{InputPath: "a.json", Title: "A #3", Cost: 0.01},
	}, 3)
	require.NotNil(t, m)
	require.Len(t, m.Evals, 1)
	e := m.Evals[0]
	assert.Equal(t, "flaky", e.Status)
	assert.Equal(t, 2, e.Passed)
	assert.Equal(t, 1, e.Errors)
	assert.Equal(t, []string{"A #2"}, e.FailedRuns)
	require.NotNil(t, e.Cost)
	assert.Equal(t, 2, e.Cost.Samples)
	assert.InDelta(t, 0.01, e.Cost.Max, 1e-9)
	assert.Empty(t, e.Cost.Outliers)
}

func TestRepeatDiagnosticsOrdering(t *testing.T) {
	t.Parallel()
	evals := computeRepeatDiagnostics([]Result{
		{InputPath: "z.json"},
		{InputPath: "a.json"},
		{InputPath: "z.json"},
		{InputPath: "a.json"},
	}, 2)
	require.Len(t, evals, 2)
	assert.Equal(t, "a.json", evals[0].InputPath)
	assert.Equal(t, "z.json", evals[1].InputPath)
	assert.Empty(t, computeRepeatDiagnostics([]Result{{}}, 3))
}

func TestPrintRepeatDiagnostics(t *testing.T) {
	t.Parallel()
	results := []Result{
		{InputPath: "a.json", Title: "A #1", Cost: 0.01},
		{InputPath: "a.json", Title: "A #2", Cost: 0.01, SizeExpected: "M", Size: "S"},
		{InputPath: "a.json", Title: "A #3", Cost: 0.09},
		{InputPath: "b.json", Title: "B #1", Error: "boom"},
		{InputPath: "b.json", Title: "B #2", Error: "boom"},
		{InputPath: "b.json", Title: "B #3", Error: "boom"},
	}
	summary := computeSummary(results)
	summary.RepeatMetrics = computeRepeatMetrics(results, 3)
	var buf bytes.Buffer
	printSummary(&buf, summary, time.Minute)
	output := buf.String()
	assert.Contains(t, output, "pass@3: 50.0%")
	assert.Contains(t, output, "pass^3: 0.0%")
	assert.Contains(t, output, "Stability: 0 stable, 1 flaky, 1 failing, 0 incomplete")
	assert.Contains(t, output, "2/3")
	assert.Contains(t, output, "$0.010000 [$0.010000, $0.090000] (n=3)")
	assert.Contains(t, output, "n/a")
	assert.Contains(t, output, "Failed repetitions (a.json): A #2")
	assert.Contains(t, output, "Cost outliers (>2x per-eval median; at least 3 non-error runs):")
	assert.Contains(t, output, "A #3 (a.json): $0.090000 (9.0x median)")
	assert.Contains(t, output, "Total Cost: $0.110000")
}

func TestPrintRepeatDiagnosticsNoOutliers(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printRepeatDiagnostics(&buf, computeRepeatDiagnostics([]Result{
		{InputPath: "a.json", Cost: 0.01},
		{InputPath: "a.json", Cost: 0.01},
	}, 2))
	assert.Contains(t, buf.String(), "1 stable, 0 flaky, 0 failing, 0 incomplete")
	assert.NotContains(t, buf.String(), "Cost outliers")
	assert.NotContains(t, buf.String(), "Failed repetitions")

	buf.Reset()
	printRepeatDiagnostics(&buf, nil)
	assert.Empty(t, buf.String())
}

func TestRepeatDiagnosticsSavedJSON(t *testing.T) {
	t.Parallel()
	run := &EvalRun{
		Name: "repeat-test",
		Results: []Result{
			{InputPath: "a.json", Title: "A #1"},
			{InputPath: "a.json", Title: "A #2", Error: "boom"},
		},
	}
	run.Summary = computeSummary(run.Results)
	run.Summary.RepeatMetrics = computeRepeatMetrics(run.Results, 2)
	path, err := SaveRunSessionsJSON(run, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var saved RunOutput
	require.NoError(t, json.Unmarshal(data, &saved))
	assert.Equal(t, run.Summary.RepeatMetrics, saved.Summary.RepeatMetrics)
	assert.Contains(t, string(data), `"failed_runs"`)
}
