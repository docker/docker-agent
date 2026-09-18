package ui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// StatusModel is the snapshot of run state shown in the footer.
type StatusModel struct {
	WorkingDir string
	Branch     string

	Agent    string
	Model    string
	Thinking string

	ContextLength int64
	ContextLimit  int64
	Tokens        int64 // input + output tokens used so far
	Cost          float64
	CostKnown     bool

	// CompactionThreshold is the auto-compaction trigger fraction for the
	// active agent (0 when unknown); the context percentage colors against it.
	CompactionThreshold float64
	// Compacting is true while a session compaction runs.
	Compacting bool
}

// RenderStatus builds the two-line footer:
//
//	<working dir>  ⎇ <branch>                          <agent>
//	<pct> · <tokens> · <cost>                 <model> · <effort>
func RenderStatus(d StatusModel, width int) []string {
	dir := StSecondary().Render(Truncate(pathx.ShortenHome(d.WorkingDir), max(10, width/2)))
	left1 := dir
	if d.Branch != "" {
		left1 += StMuted().Render("  ⎇ " + d.Branch)
	}

	right1 := ""
	if d.Agent != "" {
		right1 = StAccent().Render(d.Agent)
	}

	left2 := RenderContext(d)

	var rightParts []string
	if d.Model != "" {
		rightParts = append(rightParts, d.Model)
	}
	if d.Thinking != "" {
		rightParts = append(rightParts, d.Thinking)
	}
	right2 := StMuted().Render(strings.Join(rightParts, " · "))

	return []string{
		ComposeLine(left1, right1, width),
		ComposeLine(left2, right2, width),
	}
}

// RenderContext renders the context and cost portion of the status.
func RenderContext(d StatusModel) string {
	cost := renderCostSuffix(d)
	if d.ContextLimit <= 0 {
		if d.Compacting {
			return StWarning().Render("compacting…") + cost
		}
		if d.Tokens > 0 {
			return StMuted().Render(FormatTokens(d.Tokens)+" tokens") + cost
		}
		return StMuted().Render("0% · 0/0") + cost
	}

	pct := float64(d.ContextLength) / float64(d.ContextLimit)
	if pct > 1 {
		pct = 1
	}
	tokens := fmt.Sprintf(" · %s/%s",
		FormatTokens(d.ContextLength),
		FormatTokens(d.ContextLimit),
	)
	if d.Compacting {
		return StWarning().Render("compacting…") + StMuted().Render(tokens) + cost
	}
	label := fmt.Sprintf("%d%%", int(pct*100+0.5)) + tokens
	return contextStyle(pct, d.CompactionThreshold).Render(label) + cost
}

func renderCostSuffix(d StatusModel) string {
	if !d.CostKnown {
		return ""
	}
	return StMuted().Render(" · ") + StAccent().Render(toolcommon.FormatCostUSD(d.Cost))
}

// contextStyle escalates the context usage color as usage approaches the
// auto-compaction threshold (0 uses the package default).
func contextStyle(pct, threshold float64) lipgloss.Style {
	switch styles.ContextGaugeLevelFor(pct, threshold) {
	case styles.ContextGaugeCritical:
		return StError()
	case styles.ContextGaugeWarning:
		return StWarning()
	default:
		return StMuted()
	}
}

// ComposeLine right-aligns right within width, truncating left if necessary.
func ComposeLine(left, right string, width int) string {
	lw := DisplayWidth(left)
	rw := DisplayWidth(right)
	if rw > width {
		return Truncate(right, width)
	}
	if lw+rw+1 > width {
		left = Truncate(left, max(0, width-rw-1))
		lw = DisplayWidth(left)
	}
	gap := max(1, width-lw-rw)
	return left + strings.Repeat(" ", gap) + right
}

// FormatTokens formats a token count for compact status display.
func FormatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}
