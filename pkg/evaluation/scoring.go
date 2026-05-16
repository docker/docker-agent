package evaluation

import (
	"fmt"
	"io"
	"time"
)

// Size thresholds for response classification.
var sizeRanges = map[string][2]int{
	"S":  {0, 500},
	"M":  {500, 1500},
	"L":  {1500, 5000},
	"XL": {5000, 999999999},
}

// getResponseSize classifies a response by character count.
func getResponseSize(response string) string {
	length := len(response)
	for label, r := range sizeRanges {
		if length >= r[0] && length < r[1] {
			return label
		}
	}

	return "XL"
}

// toolCallF1Score calculates the F1 score between expected and actual tool calls.
func toolCallF1Score(expected, actual []string) float64 {
	if len(expected) == 0 && len(actual) == 0 {
		return 1.0
	}
	if len(expected) == 0 || len(actual) == 0 {
		return 0.0
	}

	expectedCounts := countStrings(expected)
	actualCounts := countStrings(actual)

	truePositives := 0
	for name, expectedCount := range expectedCounts {
		if actualCount, exists := actualCounts[name]; exists {
			truePositives += min(expectedCount, actualCount)
		}
	}

	precision := float64(truePositives) / float64(len(actual))
	recall := float64(truePositives) / float64(len(expected))

	if precision+recall == 0 {
		return 0.0
	}
	return 2 * (precision * recall) / (precision + recall)
}

func countStrings(strs []string) map[string]int {
	counts := make(map[string]int)
	for _, s := range strs {
		counts[s]++
	}
	return counts
}

func computeSummary(results []Result) Summary {
	summary := Summary{
		TotalEvals: len(results),
	}

	for _, r := range results {
		summary.TotalCost += r.Cost
		if r.Error != "" {
			summary.FailedEvals += 1
			continue
		}

		if r.SizeExpected != "" {
			summary.SizesTotal++
			if r.SizeExpected == r.Size {
				summary.SizesPassed++
			}
		}

		if r.ToolCallsExpected > 0 {
			summary.ToolsF1Sum += r.ToolCallsScore
			summary.ToolsCount++
		}

		summary.RelevanceTotal += r.RelevanceExpected
		summary.RelevancePassed += r.RelevancePassed
	}

	return summary
}

// printSummary outputs the evaluation summary to the writer.
func printSummary(out io.Writer, summary Summary, duration time.Duration) {
	fmt.Fprintln(out)

	if summary.FailedEvals > 0 {
		fmt.Fprintf(out, "❌         Errors: %d/%d evaluations failed\n", summary.FailedEvals, summary.TotalEvals)
	}

	printMetric(out, "Sizes", summary.SizesPassed, summary.SizesTotal)
	printF1Score(out, "Tool Calls", summary.ToolsF1Sum, summary.ToolsCount)
	printMetric(out, "Relevance", int(summary.RelevancePassed), int(summary.RelevanceTotal))

	fmt.Fprintf(out, "\nTotal Cost: $%.6f\n", summary.TotalCost)
	fmt.Fprintf(out, "Total Time: %s\n", duration.Round(time.Second))
}

func printMetric(out io.Writer, label string, passed, total int) {
	if total == 0 {
		return // Skip metrics with no data
	}
	ratio := float64(passed) / float64(total)
	fmt.Fprintf(out, "%s %14s: %d/%d passed (%.1f%%)\n", statusIcon(ratio), label, passed, total, ratio*100)
}

func printF1Score(out io.Writer, label string, f1Sum float64, count int) {
	if count == 0 {
		return // Skip metrics with no data
	}
	avgF1 := f1Sum / float64(count)
	fmt.Fprintf(out, "%s %14s: %.1f%% avg F1 (%d evals)\n", statusIcon(avgF1), label, avgF1*100, count)
}

func statusIcon(ratio float64) string {
	switch {
	case ratio > 0.75:
		return "✅"
	case ratio > 0.50:
		return "⚠️"
	default:
		return "❌"
	}
}
