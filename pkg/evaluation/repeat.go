package evaluation

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

func computeRepeatDiagnostics(results []Result, k int) []RepeatEvalMetrics {
	byEval := make(map[string][]Result)
	for _, r := range results {
		// Cancelled runs leave zero-value slots for evaluations never started.
		if r.InputPath == "" {
			continue
		}
		byEval[r.InputPath] = append(byEval[r.InputPath], r)
	}

	var evals []RepeatEvalMetrics
	for path, runs := range byEval {
		m := RepeatEvalMetrics{InputPath: path, Total: len(runs)}
		var costs []Result
		for _, r := range runs {
			_, failures := r.checkResults()
			if len(failures) == 0 {
				m.Passed++
			} else {
				m.FailedRuns = append(m.FailedRuns, cmp.Or(r.Title, path))
			}
			if r.Error != "" {
				m.Errors++
				continue
			}
			costs = append(costs, r)
		}
		slices.Sort(m.FailedRuns)
		m.Cost = computeRepeatCost(costs)
		switch {
		case m.Total < k:
			m.Status = "incomplete"
		case m.Passed == m.Total:
			m.Status = "stable"
		case m.Passed == 0:
			m.Status = "failing"
		default:
			m.Status = "flaky"
		}
		evals = append(evals, m)
	}

	slices.SortFunc(evals, func(a, b RepeatEvalMetrics) int {
		return cmp.Or(cmp.Compare(repeatStatusRank(a.Status), repeatStatusRank(b.Status)), cmp.Compare(a.InputPath, b.InputPath))
	})
	return evals
}

func repeatStatusRank(status string) int {
	switch status {
	case "flaky":
		return 0
	case "failing":
		return 1
	case "incomplete":
		return 2
	default:
		return 3
	}
}

func computeRepeatCost(runs []Result) *RepeatCostMetrics {
	if len(runs) == 0 {
		return nil
	}
	slices.SortFunc(runs, func(a, b Result) int {
		return cmp.Or(cmp.Compare(a.Cost, b.Cost), cmp.Compare(a.Title, b.Title))
	})
	mid := len(runs) / 2
	median := runs[mid].Cost
	if len(runs)%2 == 0 {
		median = runs[mid-1].Cost/2 + median/2
	}
	m := &RepeatCostMetrics{
		Samples: len(runs),
		Median:  median,
		Min:     runs[0].Cost,
		Max:     runs[len(runs)-1].Cost,
	}
	// Avoid labelling one of two observations, or unpriced runs, as outliers.
	if len(runs) >= 3 && median > 0 {
		for _, r := range runs {
			if r.Cost > 2*median {
				m.Outliers = append(m.Outliers, RepeatCostOutlier{Title: r.Title, Cost: r.Cost})
			}
		}
	}
	return m
}

func printRepeatDiagnostics(out io.Writer, evals []RepeatEvalMetrics) {
	if len(evals) == 0 {
		return
	}
	counts := make(map[string]int)
	for _, e := range evals {
		counts[e.Status]++
	}
	fmt.Fprintf(out, "\n  Stability: %d stable, %d flaky, %d failing, %d incomplete\n", counts["stable"], counts["flaky"], counts["failing"], counts["incomplete"])
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "  EVAL\tSTATUS\tPASSED\tERRORS\tCOST MEDIAN [MIN, MAX]\t")
	for _, e := range evals {
		cost := "n/a"
		if e.Cost != nil {
			cost = fmt.Sprintf("$%.6f [$%.6f, $%.6f] (n=%d)", e.Cost.Median, e.Cost.Min, e.Cost.Max, e.Cost.Samples)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d/%d\t%d\t%s\t\n", e.InputPath, e.Status, e.Passed, e.Total, e.Errors, cost)
	}
	_ = tw.Flush()

	for _, e := range evals {
		if len(e.FailedRuns) > 0 {
			fmt.Fprintf(out, "  Failed repetitions (%s): %s\n", e.InputPath, strings.Join(e.FailedRuns, ", "))
		}
	}

	hasOutliers := false
	for _, e := range evals {
		if e.Cost == nil {
			continue
		}
		for _, o := range e.Cost.Outliers {
			if !hasOutliers {
				fmt.Fprintln(out, "\n  Cost outliers (>2x per-eval median; at least 3 non-error runs):")
				hasOutliers = true
			}
			fmt.Fprintf(out, "   %s (%s): $%.6f (%.1fx median)\n", o.Title, e.InputPath, o.Cost, o.Cost/e.Cost.Median)
		}
	}
}
