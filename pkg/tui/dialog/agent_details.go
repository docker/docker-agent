package dialog

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/inspection"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Single-width, non-emoji status glyphs for the toolset section. They mirror
// the lifecycle buckets in runtime.ToolsetState: serving, not running, broken.
const (
	toolsetGlyphStarted = "●"
	toolsetGlyphStopped = "○"
	toolsetGlyphError   = "⚠"
)

type agentDetailsDialog struct {
	readOnlyScrollDialog

	agent runtime.AgentDetails
	cfg   inspection.AgentConfigInfo
	usage *runtime.Usage
	cost  *float64
}

// NewAgentDetailsDialog creates the read-only Agent Inspector: an agent's
// static configuration combined with live state. It shows the description, a
// "current agent" line when live, model/fallback/thinking, the agent's latest
// known context usage, the sub-agent, handoff and skill lists, configured
// limits and enabled option flags, every toolset with a status marker and its
// tools (live names when started, otherwise the declared allow-list), and the
// slash commands it defines. The instruction/system prompt is deliberately
// omitted. cfg carries the inspector dataset resolved by the caller (pass the
// zero value to omit those sections, e.g. for remote runtimes); usage carries
// the agent's latest token-usage snapshot (pass nil when the agent has not
// run). The dialog is opened by right-clicking (or Ctrl+clicking) an agent in
// the sidebar. cost carries the agent's cumulative attributed cost with the
// same semantics as the sidebar roster (see service.SessionState AgentCost);
// pass nil when no cost is attributable.
func NewAgentDetailsDialog(a runtime.AgentDetails, cfg inspection.AgentConfigInfo, usage *runtime.Usage, cost *float64) Dialog {
	d := &agentDetailsDialog{agent: a, cfg: cfg, usage: usage, cost: cost}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(
		readOnlyScrollDialogSize{widthPercent: 70, minWidth: 50, maxWidth: 100, heightPercent: 80, heightMax: 40},
		d.renderLines,
	)
	return d
}

func (d *agentDetailsDialog) renderLines(contentWidth, _ int) []string {
	// The title is rendered in the agent's own accent color so it matches the
	// sidebar roster/card, while keeping the dialog title's bold centering.
	titleStyle := styles.DialogTitleStyle.Foreground(styles.AgentAccentStyleFor(d.agent.Name).GetForeground())
	lines := []string{
		RenderTitle(d.agent.Name, contentWidth, titleStyle),
		RenderSeparator(contentWidth),
		"",
	}

	if desc := strings.TrimSpace(d.agent.Description); desc != "" {
		for _, l := range toolcommon.WrapLinesWords(desc, contentWidth) {
			lines = append(lines, styles.MutedStyle.Render(l))
		}
		lines = append(lines, "")
	}

	if d.cfg.IsCurrent {
		marker := styles.SuccessStyle.Render(toolsetGlyphStarted)
		lines = append(lines, marker+" "+styles.MutedStyle.Render("current agent"), "")
	}

	lines = append(lines, d.executionLines(contentWidth)...)
	lines = append(lines, detailField("Model", d.modelText()))
	if len(d.cfg.Fallbacks) > 0 {
		lines = append(lines, detailField("Fallback", strings.Join(d.cfg.Fallbacks, ", ")))
	}
	if gv := toolcommon.ThinkingGaugeValue(d.agent.Thinking); gv != "" {
		lines = append(lines, styles.BoldStyle.Render("Thinking:")+" "+gv)
	}
	if l := d.contextLine(); l != "" {
		lines = append(lines, l)
	}
	if d.cost != nil {
		lines = append(lines, detailField("Cost", toolcommon.FormatCostPrecise(*d.cost)))
	}

	lines = append(lines, d.inlineList(contentWidth, "Sub-agents", d.cfg.SubAgents)...)
	lines = append(lines, d.inlineList(contentWidth, "Handoffs", d.cfg.Handoffs)...)
	lines = append(lines, d.inlineList(contentWidth, "Skills", d.cfg.Skills)...)

	if l := d.limitsLine(); l != "" {
		lines = append(lines, l)
	}
	if l := d.optionsLine(); l != "" {
		lines = append(lines, l)
	}

	lines = append(lines, d.toolsetLines(contentWidth)...)
	lines = append(lines, d.commandLines(contentWidth)...)

	return lines
}

func (d *agentDetailsDialog) executionLines(contentWidth int) []string {
	capabilities := d.cfg.ExecutionCapabilities
	if capabilities == nil {
		capabilities = d.agent.ExecutionCapabilities
	}
	if capabilities == nil {
		return nil
	}
	lines := []string{
		detailField("Execution", capabilities.Mode),
		detailField("Tool execution", capabilities.ToolExecution),
		detailField("Tool approval", capabilities.ToolApproval),
	}
	for _, feature := range []struct {
		label     string
		supported bool
	}{
		{"Budgets", capabilities.BudgetSupported},
		{"Native compaction", capabilities.CompactionSupported},
		{"Mid-turn steering", capabilities.MidTurnSteeringSupported},
		{"Prompt-hook context", capabilities.PromptHookContextSupported},
	} {
		value := "unsupported"
		if feature.supported {
			value = "supported"
			if feature.label == "Budgets" {
				value += "; requires runtime configuration"
			}
		}
		lines = append(lines, detailField(feature.label, value))
	}
	if capabilities.Mode == inspection.ExecutionModeHarness {
		lines = append(lines, toolcommon.WrapLinesWords("External tool events report actions; docker-agent does not approve them.", contentWidth)...)
	}
	return append(lines, "")
}

func (d *agentDetailsDialog) modelText() string {
	if d.agent.Provider != "" && d.agent.Model != "" {
		return d.agent.Provider + "/" + d.agent.Model
	}
	if d.agent.Model != "" {
		return d.agent.Model
	}
	return d.agent.Provider
}

// contextLine renders the agent's latest known context usage as
// "Context: 12.3K of 128.0K tokens (10%)", falling back to a bare token count
// when the context limit is unknown (e.g. harness-backed agents). It returns
// "" when no usage has been recorded yet — the agent has not run in this
// session, as a delegated sub-agent, or as a background agent task.
func (d *agentDetailsDialog) contextLine() string {
	if d.usage == nil || d.usage.ContextLength <= 0 {
		return ""
	}
	tokens := toolcommon.FormatTokenCount(d.usage.ContextLength)
	if d.usage.ContextLimit <= 0 {
		return detailField("Context", tokens+" tokens")
	}
	percent := float64(d.usage.ContextLength) / float64(d.usage.ContextLimit) * 100
	return detailField("Context", fmt.Sprintf("%s of %s tokens (%.0f%%)",
		tokens, toolcommon.FormatTokenCount(d.usage.ContextLimit), percent))
}

// inlineList renders a compact "Label (N): a, b, c" summary wrapped to the
// dialog width, with the bold "Label (N):" prefix. It returns nil when items is
// empty so the caller omits the line entirely. It is used for the sub-agents,
// handoffs and skills sections.
func (d *agentDetailsDialog) inlineList(contentWidth int, label string, items []string) []string {
	if len(items) == 0 {
		return nil
	}
	prefix := fmt.Sprintf("%s (%d):", label, len(items))
	full := prefix + " " + strings.Join(items, ", ")
	wrapped := toolcommon.WrapLinesWords(full, contentWidth)
	out := make([]string, 0, len(wrapped))
	for i, l := range wrapped {
		if i == 0 && strings.HasPrefix(l, prefix) {
			out = append(out, styles.BoldStyle.Render(prefix)+styles.MutedStyle.Render(strings.TrimPrefix(l, prefix)))
			continue
		}
		out = append(out, styles.MutedStyle.Render(l))
	}
	return out
}

// limitsLine renders the configured per-agent limits as "Limits: max-iter 50 ·
// history 40 · max-tool-calls 5", including only the limits that are set
// (non-zero). It returns "" when none are configured.
func (d *agentDetailsDialog) limitsLine() string {
	var parts []string
	if d.cfg.MaxIterations > 0 {
		parts = append(parts, fmt.Sprintf("max-iter %d", d.cfg.MaxIterations))
	}
	if d.cfg.NumHistoryItems > 0 {
		parts = append(parts, fmt.Sprintf("history %d", d.cfg.NumHistoryItems))
	}
	if d.cfg.MaxConsecutiveToolCalls > 0 {
		parts = append(parts, fmt.Sprintf("max-tool-calls %d", d.cfg.MaxConsecutiveToolCalls))
	}
	if len(parts) == 0 {
		return ""
	}
	return detailField("Limits", strings.Join(parts, " · "))
}

// optionsLine renders the agent's enabled option flags as "Options: add-date ·
// redact-secrets · …". It returns "" when no flags are enabled.
func (d *agentDetailsDialog) optionsLine() string {
	if len(d.cfg.Options) == 0 {
		return ""
	}
	return detailField("Options", strings.Join(d.cfg.Options, " · "))
}

// toolsetLines renders the Toolsets section: one header line per toolset with a
// status marker (● started / ○ stopped / ⚠ error), the name, "(kind)" and a
// tool count, followed by the indented, wrapped tool names — live names when
// the toolset is started, or "declared: …" from the allow-list otherwise.
func (d *agentDetailsDialog) toolsetLines(contentWidth int) []string {
	if len(d.cfg.Toolsets) == 0 {
		return nil
	}
	lines := []string{"", sectionHeading(fmt.Sprintf("Toolsets (%d)", len(d.cfg.Toolsets)))}
	for _, ts := range d.cfg.Toolsets {
		lines = append(lines, toolsetHeaderLine(ts))
		lines = append(lines, toolsetToolLines(contentWidth, ts)...)
	}
	return lines
}

func toolsetHeaderLine(ts inspection.ToolsetDetail) string {
	header := toolsetStateGlyph(ts.State) + " " + ts.Name +
		styles.MutedStyle.Render(" ("+toolsetKindLabel(ts.Kind)+")")
	if n := len(ts.Tools); n > 0 {
		header += styles.MutedStyle.Render(" · " + countLabel(n, "tool"))
	}
	return header
}

func toolsetToolLines(contentWidth int, ts inspection.ToolsetDetail) []string {
	if len(ts.Tools) == 0 {
		return nil
	}
	body := strings.Join(ts.Tools, ", ")
	if ts.State != inspection.ToolsetStarted {
		body = "declared: " + body
	}
	const indent = "    "
	avail := contentWidth - len(indent)
	if avail <= 0 {
		return nil
	}
	wrapped := toolcommon.WrapLinesWords(body, avail)
	out := make([]string, 0, len(wrapped))
	for _, l := range wrapped {
		out = append(out, indent+styles.MutedStyle.Render(l))
	}
	return out
}

// toolsetStateGlyph returns the colored status marker for a toolset's lifecycle
// bucket, reusing the success/warning/muted palette.
func toolsetStateGlyph(state inspection.ToolsetState) string {
	switch state {
	case inspection.ToolsetError:
		return styles.WarningStyle.Render(toolsetGlyphError)
	case inspection.ToolsetStopped:
		return styles.MutedStyle.Render(toolsetGlyphStopped)
	default:
		return styles.SuccessStyle.Render(toolsetGlyphStarted)
	}
}

func (d *agentDetailsDialog) commandLines(contentWidth int) []string {
	if len(d.agent.Commands) == 0 {
		return nil
	}

	names := make([]string, 0, len(d.agent.Commands))
	for name := range d.agent.Commands {
		names = append(names, name)
	}
	slices.Sort(names)

	lines := []string{"", sectionHeading(fmt.Sprintf("Commands (%d)", len(names)))}
	for _, name := range names {
		label := lipgloss.NewStyle().Foreground(styles.Highlight).Render("  /" + name)
		lines = append(lines, label)
		if desc := strings.TrimSpace(d.agent.Commands[name].DisplayText()); desc != "" {
			indent := "    "
			availableWidth := contentWidth - lipgloss.Width(indent)
			if availableWidth > 0 {
				lines = append(lines, indent+styles.MutedStyle.Render(toolcommon.TruncateText(desc, availableWidth)))
			}
		}
	}
	return lines
}

// countLabel renders "1 tool" / "3 tools" with naive pluralization (the nouns
// used here are all regular).
func countLabel(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// detailField renders a "Label: value" body line with a bold label.
func detailField(label, value string) string {
	return styles.BoldStyle.Render(label+":") + " " + styles.MutedStyle.Render(value)
}

func sectionHeading(title string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(styles.TextSecondary).Render(title)
}
