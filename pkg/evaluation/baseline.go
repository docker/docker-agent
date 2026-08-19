package evaluation

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
)

// Metrics is the comparable shape of an evaluation run: the rates a regression
// gate can be built on, derived from the same [Summary] the run prints.
//
// Rates are 0 when their denominator is 0, and the corresponding Has… flag says
// whether the rate means anything. Without that distinction "no size
// expectations declared" and "every size expectation failed" would both read as
// 0.0 and a gate could not tell them apart.
type Metrics struct {
	TotalEvals  int     `json:"total_evals"`
	FailedEvals int     `json:"failed_evals"`
	FailureRate float64 `json:"failure_rate"`

	SizePassRate float64 `json:"size_pass_rate"`
	HasSizes     bool    `json:"has_sizes"`

	ToolsF1Mean float64 `json:"tools_f1_mean"`
	HasTools    bool    `json:"has_tools"`

	RelevanceRate float64 `json:"relevance_rate"`
	HasRelevance  bool    `json:"has_relevance"`

	TotalCost float64 `json:"total_cost"`
}

// metricsOfSummary derives [Metrics] from a run summary.
func metricsOfSummary(s Summary) Metrics {
	m := Metrics{
		TotalEvals:  s.TotalEvals,
		FailedEvals: s.FailedEvals,
		TotalCost:   s.TotalCost,
	}
	if s.TotalEvals > 0 {
		m.FailureRate = float64(s.FailedEvals) / float64(s.TotalEvals)
	}
	if s.SizesTotal > 0 {
		m.HasSizes = true
		m.SizePassRate = float64(s.SizesPassed) / float64(s.SizesTotal)
	}
	if s.ToolsCount > 0 {
		m.HasTools = true
		m.ToolsF1Mean = s.ToolsF1Sum / float64(s.ToolsCount)
	}
	if s.RelevanceTotal > 0 {
		m.HasRelevance = true
		m.RelevanceRate = s.RelevancePassed / s.RelevanceTotal
	}
	return m
}

// MetricsOf derives [Metrics] from a run's results.
func MetricsOf(run *EvalRun) Metrics {
	if run == nil {
		return Metrics{}
	}
	return metricsOfSummary(computeSummary(run.Results))
}

// Baseline is a previously saved run, reduced to what a regression gate needs.
//
// It is loaded from the JSON the eval command actually writes — a [RunOutput]
// from SaveRunSessionsJSON — rather than from [EvalRun], which has no producer
// outside tests and whose Duration field is typed incompatibly.
type Baseline struct {
	Name    string
	Summary Summary
	// Passed maps an evaluation's key (see evalKey) to whether it passed.
	Passed map[string]bool
}

// MetricDelta is one metric's movement between two runs. Higher is better for
// quality rates and worse for FailureRate, so Regressed — not the sign of Delta
// — is what a gate reads.
type MetricDelta struct {
	Name      string  `json:"name"`
	Baseline  float64 `json:"baseline"`
	Current   float64 `json:"current"`
	Delta     float64 `json:"delta"`
	Regressed bool    `json:"regressed"`
	// Informational marks a metric that is reported but never gates, so a
	// reviewer can see it moved without the build failing over it.
	Informational bool `json:"informational,omitempty"`
}

// EvalChange records one evaluation's pass/fail transition between runs.
type EvalChange struct {
	Eval string `json:"eval"`
	// Was and Now are "pass", "fail", or "absent".
	Was       string `json:"was"`
	Now       string `json:"now"`
	Regressed bool   `json:"regressed"`
}

// Comparison is the result of checking a run against a baseline.
type Comparison struct {
	Baseline  Metrics       `json:"baseline"`
	Current   Metrics       `json:"current"`
	Tolerance float64       `json:"tolerance"`
	Deltas    []MetricDelta `json:"deltas"`
	Changes   []EvalChange  `json:"changes"`
	// Regressed is true when any gating metric moved beyond the tolerance, or
	// an evaluation that passed in the baseline now fails.
	Regressed bool `json:"regressed"`
}

// MaxTolerance is the largest accepted --regression-tolerance. A rate cannot
// move by more than 1.0, so anything above it silently disables the aggregate
// gate — better rejected at startup than discovered when a regression sails
// through.
const MaxTolerance = 1.0

// ErrNoBaselineEvals reports a baseline carrying no evaluations. A gate built on
// one would compare against all-zero metrics and pass unconditionally.
var ErrNoBaselineEvals = errors.New("baseline contains no evaluations")

// ErrNoCurrentEvals reports a run that produced no evaluations — an --only
// pattern that matched nothing, for instance. There is nothing to gate on.
var ErrNoCurrentEvals = errors.New("run produced no evaluations to compare")

// ErrNothingComparable reports a baseline and run with no metric and no
// evaluation in common, so the comparison could only ever pass.
var ErrNothingComparable = errors.New("baseline and run share no metric or evaluation to compare")

// LoadBaseline reads the run JSON the eval command writes.
//
// A file that parses but carries no evaluations is rejected rather than treated
// as an empty baseline: every rate would be skipped by the has-flag guards and
// the gate would report success while every evaluation in the current run
// failed. A gate must fail closed.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading baseline %q: %w", path, err)
	}

	var output RunOutput
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, fmt.Errorf("parsing baseline %q: %w", path, err)
	}

	baseline := &Baseline{
		Name:    output.Name,
		Summary: output.Summary,
		Passed:  make(map[string]bool, len(output.Sessions)),
	}
	for _, sess := range output.Sessions {
		if sess == nil || sess.EvalResult == nil {
			continue
		}
		baseline.Passed[sessionEvalKey(sess.InputID, sess.Title)] = sess.EvalResult.Passed
	}

	if baseline.Summary.TotalEvals == 0 && len(baseline.Passed) == 0 {
		return nil, fmt.Errorf("%w: %q is not an evaluation run written by `docker agent eval`", ErrNoBaselineEvals, path)
	}
	return baseline, nil
}

// Compare checks current against baseline.
//
// tolerance is the amount an aggregate quality rate may fall (or the failure
// rate may climb) before it counts as a regression, so judge variance does not
// fail a build on noise. A tolerance of 0 means any movement in the wrong
// direction regresses; a negative value is clamped to 0.
//
// The tolerance governs aggregate rates ONLY. An evaluation that passed in the
// baseline and now fails gates regardless of tolerance — that transition is the
// exact signal this check exists to catch, and absorbing it would defeat the
// point. Consequently a large tolerance still cannot hide an outright breakage.
//
// Note the corollary: adding a new FAILING evaluation lowers the aggregate rate
// and therefore gates, even though no existing evaluation regressed. That is
// intended — a suite that got worse should say so — but it means "add a known-
// failing eval as a TODO" needs an explicit tolerance bump or a fix.
//
// Cost is reported but never gates: a cost increase is not a quality regression,
// and gating on it would make the check fire on provider price changes.
//
// A metric absent from either run is skipped rather than treated as 0 — adding
// the first size expectation to a suite must not look like a regression. If that
// leaves nothing to gate on and no evaluation in common, an error is returned:
// a comparison that can only ever pass is worse than no comparison.
func Compare(baseline *Baseline, current *EvalRun, tolerance float64) (Comparison, error) {
	if baseline == nil {
		return Comparison{}, ErrNoBaselineEvals
	}
	if current == nil || len(current.Results) == 0 {
		return Comparison{}, ErrNoCurrentEvals
	}
	tolerance = max(tolerance, 0)

	c := Comparison{
		Baseline:  metricsOfSummary(baseline.Summary),
		Current:   MetricsOf(current),
		Tolerance: tolerance,
	}

	gating := 0
	for _, q := range []struct {
		name            string
		base, cur       float64
		hasBase, hasCur bool
	}{
		{"size pass rate", c.Baseline.SizePassRate, c.Current.SizePassRate, c.Baseline.HasSizes, c.Current.HasSizes},
		{"tool F1 mean", c.Baseline.ToolsF1Mean, c.Current.ToolsF1Mean, c.Baseline.HasTools, c.Current.HasTools},
		{"relevance rate", c.Baseline.RelevanceRate, c.Current.RelevanceRate, c.Baseline.HasRelevance, c.Current.HasRelevance},
	} {
		if !q.hasBase || !q.hasCur {
			continue
		}
		d := MetricDelta{Name: q.name, Baseline: q.base, Current: q.cur, Delta: q.cur - q.base}
		d.Regressed = q.cur < q.base-tolerance
		c.Deltas = append(c.Deltas, d)
		gating++
	}

	if c.Baseline.TotalEvals > 0 && c.Current.TotalEvals > 0 {
		d := MetricDelta{
			Name:     "failure rate",
			Baseline: c.Baseline.FailureRate,
			Current:  c.Current.FailureRate,
			Delta:    c.Current.FailureRate - c.Baseline.FailureRate,
		}
		d.Regressed = c.Current.FailureRate > c.Baseline.FailureRate+tolerance
		c.Deltas = append(c.Deltas, d)
		gating++
	}

	c.Changes = compareEvals(baseline, current)

	// Nothing comparable: no shared metric and no shared evaluation. Reporting
	// "no regression" here would be a gate that cannot fail.
	if gating == 0 && !anyShared(baseline, current) {
		return Comparison{}, ErrNothingComparable
	}

	c.Deltas = append(c.Deltas, MetricDelta{
		Name:          "total cost",
		Baseline:      c.Baseline.TotalCost,
		Current:       c.Current.TotalCost,
		Delta:         c.Current.TotalCost - c.Baseline.TotalCost,
		Informational: true,
	})

	for _, d := range c.Deltas {
		if d.Regressed && !d.Informational {
			c.Regressed = true
		}
	}
	for _, ch := range c.Changes {
		if ch.Regressed {
			c.Regressed = true
		}
	}
	return c, nil
}

// anyShared reports whether the two runs have an evaluation in common.
func anyShared(baseline *Baseline, current *EvalRun) bool {
	for _, r := range current.Results {
		if _, ok := baseline.Passed[resultEvalKey(r)]; ok {
			return true
		}
	}
	return false
}

// compareEvals pairs evaluations by key and reports transitions. Only pass →
// fail is a regression; appearing and disappearing evaluations are reported so a
// reviewer notices a suite change, but do not gate.
func compareEvals(baseline *Baseline, current *EvalRun) []EvalChange {
	now := map[string]bool{}
	for _, r := range current.Results {
		key := resultEvalKey(r)
		// Repeated runs of the same evaluation collapse pessimistically: if any
		// repetition failed, it counts as failed. A flaky pass is not a pass.
		if prev, seen := now[key]; seen {
			now[key] = prev && resultPassed(r)
			continue
		}
		now[key] = resultPassed(r)
	}

	keys := make(map[string]struct{}, len(baseline.Passed)+len(now))
	for k := range baseline.Passed {
		keys[k] = struct{}{}
	}
	for k := range now {
		keys[k] = struct{}{}
	}

	changes := make([]EvalChange, 0, len(keys))
	for k := range keys {
		bs, inBase := baseline.Passed[k]
		cs, inCur := now[k]
		change := EvalChange{Eval: k, Was: passLabel(bs, inBase), Now: passLabel(cs, inCur)}
		if change.Was == change.Now {
			continue
		}
		change.Regressed = inBase && bs && inCur && !cs
		changes = append(changes, change)
	}

	slices.SortFunc(changes, func(a, b EvalChange) int {
		// Regressions first, then alphabetical, so the important lines lead.
		if a.Regressed != b.Regressed {
			if a.Regressed {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Eval, b.Eval)
	})
	return changes
}

// resultEvalKey identifies an evaluation the same way [sessionEvalKey] does for
// the baseline side, so the two runs pair up. The session's InputID is preferred
// because it is the caller-supplied correlation ID; the display title is the
// fallback and carries the "#N" suffix that distinguishes repetitions.
func resultEvalKey(r Result) string {
	var inputID string
	if r.Session != nil {
		inputID = r.Session.InputID
	}
	return sessionEvalKey(inputID, cmp.Or(r.Title, r.InputPath))
}

func sessionEvalKey(inputID, title string) string {
	return cmp.Or(inputID, title)
}

func passLabel(passed, present bool) string {
	switch {
	case !present:
		return "absent"
	case passed:
		return "pass"
	default:
		return "fail"
	}
}

// resultPassed reports whether one result met every expectation declared for it.
//
// It delegates to [Result.checkResults] — the same function that decides the
// PASS/FAIL the eval command prints and the `passed` flag written into the saved
// run. A second definition here would let the gate drift from the product: an
// evaluation whose printed status flipped would be recorded as no change.
func resultPassed(r Result) bool {
	_, failures := r.checkResults()
	return len(failures) == 0
}

// PrintComparison writes a human-readable comparison.
func PrintComparison(out io.Writer, c Comparison) {
	fmt.Fprintf(out, "\nBaseline comparison (tolerance %.3f)\n", c.Tolerance)

	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tBASELINE\tCURRENT\tDELTA\t")
	for _, d := range c.Deltas {
		marker := "  "
		switch {
		case d.Regressed:
			marker = "! "
		case d.Informational:
			marker = "· "
		}
		fmt.Fprintf(tw, "%s%s\t%.3f\t%.3f\t%+.3f\t\n", marker, d.Name, d.Baseline, d.Current, d.Delta)
	}
	_ = tw.Flush()

	if len(c.Changes) > 0 {
		fmt.Fprintln(out, "\nChanged evaluations")
		ctw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		for _, ch := range c.Changes {
			marker := "  "
			if ch.Regressed {
				marker = "! "
			}
			fmt.Fprintf(ctw, "%s%s\t%s → %s\t\n", marker, ch.Eval, ch.Was, ch.Now)
		}
		_ = ctw.Flush()
	}

	if c.Regressed {
		fmt.Fprintln(out, "\n❌ Regression against baseline")
		return
	}
	fmt.Fprintln(out, "\n✅ No regression against baseline")
}
