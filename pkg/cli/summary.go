package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// PrintSessionSummary writes a compact end-of-run recap: an interaction
// summary (duration, tool calls, success rate), a per-model usage table, and
// token/cost totals with cache savings. It is a no-op when the session
// recorded no measurable activity. Callers skip it in JSON output mode.
func (p *Printer) PrintSessionSummary(stats session.Stats) {
	if !stats.HasActivity() {
		return
	}

	p.Println()
	p.Println(bold("─── Session summary ───"))

	var rows [][2]string
	if stats.ID != "" {
		rows = append(rows, [2]string{"Session", stats.ID})
	}
	if stats.Duration > 0 {
		rows = append(rows, [2]string{"Duration", formatDuration(stats.Duration)})
	}
	if stats.Requests > 0 {
		rows = append(rows, [2]string{"Requests", strconv.Itoa(stats.Requests)})
	}
	if stats.ToolCalls > 0 {
		rows = append(rows,
			[2]string{"Tool calls", fmt.Sprintf("%d (%d ok, %d failed)", stats.ToolCalls, stats.ToolSuccesses(), stats.ToolErrors)},
			[2]string{"Success rate", fmt.Sprintf("%.1f%%", stats.SuccessRate())},
		)
	}
	p.printLabeledRows(rows)

	if len(stats.Models) > 0 {
		p.Println()
		p.Println(bold("Model usage"))
		p.printModelTable(stats.Models)
	}

	p.Println()
	totals := [][2]string{
		{"Tokens", fmt.Sprintf("%s in / %s out", groupDigits(stats.TotalInput()), groupDigits(stats.OutputTokens))},
		{"Cost", formatCost(stats.Cost)},
	}
	if stats.CachedInput > 0 {
		totals = append(totals, [2]string{"Cache", fmt.Sprintf("%s input tokens from cache (%.1f%%)", groupDigits(stats.CachedInput), stats.CacheHitRate())})
	}
	p.printLabeledRows(totals)
}

// printLabeledRows prints "label: value" pairs with the colon-terminated
// labels padded to a common width so the values line up. Padding is applied
// outside the color escape so it stays correct regardless of TTY coloring.
func (p *Printer) printLabeledRows(rows [][2]string) {
	labelWidth := 0
	for _, r := range rows {
		if l := len(r[0]) + 1; l > labelWidth {
			labelWidth = l
		}
	}
	for _, r := range rows {
		label := r[0] + ":"
		p.Printf("%s%s  %s\n", bold(label), strings.Repeat(" ", labelWidth-len(label)), r[1])
	}
}

// printModelTable renders the per-model usage breakdown as an aligned table.
func (p *Printer) printModelTable(models []session.ModelStats) {
	headers := []string{"MODEL", "REQS", "INPUT", "CACHE READ", "OUTPUT"}
	rows := make([][]string, 0, len(models))
	for _, m := range models {
		rows = append(rows, []string{
			m.Model,
			strconv.Itoa(m.Requests),
			groupDigits(m.InputTokens),
			groupDigits(m.CachedInput),
			groupDigits(m.OutputTokens),
		})
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}

	p.Println(bold(formatTableRow(headers, widths)))
	for _, row := range rows {
		p.Println(formatTableRow(row, widths))
	}
}

// formatTableRow left-aligns the first column (model name) and right-aligns the
// numeric columns, separating them with a fixed gutter.
func formatTableRow(cols []string, widths []int) string {
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteString("   ")
		}
		pad := strings.Repeat(" ", widths[i]-len(c))
		if i == 0 {
			b.WriteString(c)
			b.WriteString(pad)
		} else {
			b.WriteString(pad)
			b.WriteString(c)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// groupDigits formats an integer with thousands separators (e.g. 70637 ->
// "70,637").
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// formatCost renders a dollar amount, widening the precision for sub-cent
// values so small costs are not all displayed as "$0.00".
func formatCost(cost float64) string {
	if cost < 0 {
		return "-" + formatCost(-cost)
	}
	switch {
	case cost < 0.0001:
		return "$0.00"
	case cost < 0.01:
		return fmt.Sprintf("$%.4f", cost)
	default:
		return fmt.Sprintf("$%.2f", cost)
	}
}

// formatDuration renders a duration in a compact, human-readable form.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
