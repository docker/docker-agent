package dialog

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/inspection"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// renderAgentDetails returns the ANSI-stripped rendered lines of the dialog
// body. cfg populates the inline config sections (toolsets, sub-agents,
// handoffs, fallbacks).
func renderAgentDetails(a runtime.AgentDetails, cfg inspection.AgentConfigInfo) string {
	return renderAgentDetailsUsage(a, cfg, nil)
}

// renderAgentDetailsUsage is renderAgentDetails with a token-usage snapshot
// for the context-usage line.
func renderAgentDetailsUsage(a runtime.AgentDetails, cfg inspection.AgentConfigInfo, usage *runtime.Usage) string {
	return renderAgentDetailsCost(a, cfg, usage, nil)
}

// renderAgentDetailsCost is renderAgentDetailsUsage with the agent's
// cumulative attributed cost for the cost line (nil when unattributable).
func renderAgentDetailsCost(a runtime.AgentDetails, cfg inspection.AgentConfigInfo, usage *runtime.Usage, cost *float64) string {
	d := NewAgentDetailsDialog(a, cfg, usage, cost).(*agentDetailsDialog)
	return ansi.Strip(strings.Join(d.renderLines(80, 24), "\n"))
}

func TestAgentDetailsDialog_RendersCoreFields(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{
		Name:        "root",
		Description: "Executive assistant that routes work",
		Provider:    "anthropic",
		Model:       "claude-opus-4-8",
		Thinking:    "high",
	}, inspection.AgentConfigInfo{})

	assert.Contains(t, out, "root")
	assert.Contains(t, out, "Executive assistant that routes work")
	assert.Contains(t, out, "Model: anthropic/claude-opus-4-8")
	// The thinking line shows the same six-cell gauge + value as the sidebar.
	assert.Contains(t, out, "Thinking: "+ansi.Strip(toolcommon.ThinkingGaugeValue("high")))
}

// TestAgentDetailsDialog_ThinkingVocabulary covers every thinking label kind,
// including the empty case (no thinking line). Each non-empty line carries the
// shared gauge + value rendering.
func TestAgentDetailsDialog_ThinkingVocabulary(t *testing.T) {
	t.Parallel()

	for _, thinking := range []string{"high", "adaptive", "8192", "off", ""} {
		t.Run(thinking, func(t *testing.T) {
			t.Parallel()
			out := renderAgentDetails(runtime.AgentDetails{
				Name:     "agent",
				Provider: "openai",
				Model:    "gpt-5.4",
				Thinking: thinking,
			}, inspection.AgentConfigInfo{})
			if thinking == "" {
				assert.NotContains(t, out, "Thinking:", "empty thinking label must omit the line")
				return
			}
			assert.Contains(t, out, "Thinking: "+ansi.Strip(toolcommon.ThinkingGaugeValue(thinking)))
		})
	}
}

// TestAgentDetailsDialog_RendersConfigSections verifies the inline compact
// config summaries — sub-agents, handoffs, skills and the fallback model —
// render with their counts, and that each section is omitted when its slice is
// empty.
func TestAgentDetailsDialog_RendersConfigSections(t *testing.T) {
	t.Parallel()

	withCfg := renderAgentDetails(runtime.AgentDetails{
		Name: "root", Provider: "openai", Model: "gpt-5.4", Thinking: "high",
	}, inspection.AgentConfigInfo{
		SubAgents: []string{"coder", "reviewer"},
		Handoffs:  []string{"planner"},
		Skills:    []string{"debugging", "refactor"},
		Fallbacks: []string{"anthropic/claude-opus-4-8"},
	})
	assert.Contains(t, withCfg, "Sub-agents (2): coder, reviewer")
	assert.Contains(t, withCfg, "Handoffs (1): planner")
	assert.Contains(t, withCfg, "Skills (2): debugging, refactor")
	assert.Contains(t, withCfg, "Fallback: anthropic/claude-opus-4-8")

	empty := renderAgentDetails(runtime.AgentDetails{Name: "root", Model: "gpt-5.4"}, inspection.AgentConfigInfo{})
	assert.NotContains(t, empty, "Sub-agents (", "no sub-agents section when none configured")
	assert.NotContains(t, empty, "Handoffs (", "no handoffs section when none configured")
	assert.NotContains(t, empty, "Skills (", "no skills section when none configured")
	assert.NotContains(t, empty, "Toolsets (", "no toolsets section when none configured")
	assert.NotContains(t, empty, "Limits:", "no limits line when none configured")
	assert.NotContains(t, empty, "Options:", "no options line when none configured")
	assert.NotContains(t, empty, "Fallback:", "no fallback line when none configured")
	assert.NotContains(t, empty, "current agent", "no live line when not current")
}

// TestAgentDetailsDialog_TitleUsesAgentAccentColor verifies the title is
// rendered in the agent's accent color (matching the sidebar), so two agents
// produce differently-colored titles. Not parallel: it mutates the global
// agent-order registry.
func TestAgentDetailsDialog_TitleUsesAgentAccentColor(t *testing.T) {
	styles.SetAgentOrder([]string{"root", "helper"})
	t.Cleanup(func() { styles.SetAgentOrder(nil) })

	titleOf := func(name string) string {
		d := NewAgentDetailsDialog(runtime.AgentDetails{Name: name, Model: "gpt"}, inspection.AgentConfigInfo{}, nil, nil).(*agentDetailsDialog)
		return d.renderLines(80, 24)[0] // raw title line, with ANSI styling
	}

	root := titleOf("root")
	helper := titleOf("helper")

	assert.Equal(t, "root", strings.TrimSpace(ansi.Strip(root)))
	assert.Equal(t, "helper", strings.TrimSpace(ansi.Strip(helper)))
	assert.NotEqual(t, root, helper, "each agent's title is rendered in its own accent color")

	// The title matches DialogTitleStyle recolored with the agent's accent.
	want := RenderTitle("root", 80, styles.DialogTitleStyle.Foreground(styles.AgentAccentStyleFor("root").GetForeground()))
	assert.Equal(t, want, root)
}

func TestAgentDetailsDialog_RendersCommands(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{
		Name:     "root",
		Provider: "anthropic",
		Model:    "opus",
		Thinking: "high",
		Commands: types.Commands{
			"plan":     {Description: "Hand off to the planner", Agent: "planner"},
			"fix-lint": {Description: "Fix linting errors"},
		},
	}, inspection.AgentConfigInfo{})

	assert.Contains(t, out, "Commands (2)")
	assert.Contains(t, out, "/fix-lint")
	assert.Contains(t, out, "Fix linting errors")
	assert.Contains(t, out, "/plan")
	assert.Contains(t, out, "Hand off to the planner")
}

func TestAgentDetailsDialog_HarnessNoThinkingNoCommands(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{
		Name:        "slack",
		Description: "Slack agent",
		Model:       "claude-code",
	}, inspection.AgentConfigInfo{})

	assert.Contains(t, out, "slack")
	assert.Contains(t, out, "Model: claude-code")
	assert.NotContains(t, out, "Thinking:", "harness agent has no thinking line")
	assert.NotContains(t, out, "Commands", "no commands section without commands")
}

// TestAgentDetailsDialog_RendersToolsets verifies the Toolsets section: a
// status marker per toolset (● started / ○ stopped / ⚠ error), the name, kind
// and tool count, and the tools rendered as live names when started but as a
// "declared:" allow-list when stopped.
func TestAgentDetailsDialog_RendersToolsets(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{Name: "root", Model: "gpt-5.4"}, inspection.AgentConfigInfo{
		Toolsets: []inspection.ToolsetDetail{
			{Name: "filesystem", State: inspection.ToolsetStarted, Tools: []string{"read_file", "write_file"}},
			{Name: "git", State: inspection.ToolsetStopped, Tools: []string{"status", "commit"}},
			{Name: "search", Kind: "MCP", State: inspection.ToolsetError},
		},
	})

	assert.Contains(t, out, "Toolsets (3)")
	// Started: green marker, Built-in kind, live tool names (no "declared:").
	assert.Contains(t, out, "● filesystem (Built-in) · 2 tools")
	assert.Contains(t, out, "read_file, write_file")
	assert.NotContains(t, out, "declared: read_file")
	// Stopped: hollow marker, declared allow-list.
	assert.Contains(t, out, "○ git (Built-in) · 2 tools")
	assert.Contains(t, out, "declared: status, commit")
	// Error: warning marker, Kind label, no tools sub-line.
	assert.Contains(t, out, "⚠ search (MCP)")
}

// TestAgentDetailsDialog_RendersLiveStateLimitsOptionsSkills verifies the live
// "current agent" line, the limits line (only set values), the options line
// (only enabled flags), and the compact skills list.
func TestAgentDetailsDialog_RendersLiveStateLimitsOptionsSkills(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{Name: "root", Model: "gpt-5.4"}, inspection.AgentConfigInfo{
		IsCurrent:               true,
		MaxIterations:           50,
		NumHistoryItems:         40,
		MaxConsecutiveToolCalls: 5,
		Options:                 []string{"add-date", "redact-secrets"},
		Skills:                  []string{"debugging", "refactor"},
	})

	assert.Contains(t, out, "● current agent")
	assert.Contains(t, out, "Limits: max-iter 50 · history 40 · max-tool-calls 5")
	assert.Contains(t, out, "Options: add-date · redact-secrets")
	assert.Contains(t, out, "Skills (2): debugging, refactor")
}

// TestAgentDetailsDialog_LimitsOmitsUnsetValues verifies the limits line only
// lists the limits that are actually set (non-zero).
func TestAgentDetailsDialog_LimitsOmitsUnsetValues(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{Name: "root", Model: "gpt"}, inspection.AgentConfigInfo{
		NumHistoryItems: 40,
	})
	assert.Contains(t, out, "Limits: history 40")
	assert.NotContains(t, out, "max-iter", "unset max-iter is omitted")
	assert.NotContains(t, out, "max-tool-calls", "unset max-tool-calls is omitted")
}

// TestAgentDetailsDialog_ContextUsage covers the per-agent context line: full
// "tokens of limit (percent)" form when the context limit is known, bare token
// count when it is not (e.g. harness-backed agents), and omission when the
// agent has no recorded usage or an empty snapshot.
func TestAgentDetailsDialog_ContextUsage(t *testing.T) {
	t.Parallel()

	agent := runtime.AgentDetails{Name: "root", Provider: "openai", Model: "gpt-5.4"}

	withLimit := renderAgentDetailsUsage(agent, inspection.AgentConfigInfo{}, &runtime.Usage{
		ContextLength: 12_800,
		ContextLimit:  128_000,
	})
	assert.Contains(t, withLimit, "Context: 12.8K of 128.0K tokens (10%)")

	noLimit := renderAgentDetailsUsage(agent, inspection.AgentConfigInfo{}, &runtime.Usage{
		ContextLength: 8_200,
	})
	assert.Contains(t, noLimit, "Context: 8.2K tokens")
	assert.NotContains(t, noLimit, "%)", "no percent without a context limit")

	noUsage := renderAgentDetailsUsage(agent, inspection.AgentConfigInfo{}, nil)
	assert.NotContains(t, noUsage, "Context:", "no context line when the agent has not run")

	emptyUsage := renderAgentDetailsUsage(agent, inspection.AgentConfigInfo{}, &runtime.Usage{})
	assert.NotContains(t, emptyUsage, "Context:", "no context line for an empty snapshot")
}

// TestAgentDetailsDialog_Cost covers the per-agent cost line: the precise
// shared formatting (four decimals below one cent, two at or above), the
// explicit $0.00 for an agent that ran at zero cost, and omission when no
// cost is attributable to the agent.
func TestAgentDetailsDialog_Cost(t *testing.T) {
	t.Parallel()

	agent := runtime.AgentDetails{Name: "root", Provider: "openai", Model: "gpt-5.4"}
	costOf := func(c float64) *float64 { return &c }

	cents := renderAgentDetailsCost(agent, inspection.AgentConfigInfo{}, nil, costOf(0.1284))
	assert.Contains(t, cents, "Cost: $0.13", "amounts at or above one cent use two decimals")

	subCent := renderAgentDetailsCost(agent, inspection.AgentConfigInfo{}, nil, costOf(0.0042))
	assert.Contains(t, subCent, "Cost: $0.0042", "non-zero amounts below one cent keep four decimals")

	zero := renderAgentDetailsCost(agent, inspection.AgentConfigInfo{}, nil, costOf(0))
	assert.Contains(t, zero, "Cost: $0.00", "an agent that ran at zero cost reads $0.00")

	noCost := renderAgentDetailsCost(agent, inspection.AgentConfigInfo{}, nil, nil)
	assert.NotContains(t, noCost, "Cost:", "no cost line when no cost is attributable")
}

// TestInlineList_NarrowWidth_NoLabelDuplication verifies that when contentWidth
// is narrower than the prefix string, the guard prevents the bold prefix from
// being concatenated with the (unstripped) first wrapped fragment, which would
// render the label text twice. At narrow width the whole output falls back to
// the muted style so the label appears exactly once. At normal width the bold
// prefix is applied correctly.
func TestInlineList_NarrowWidth_NoLabelDuplication(t *testing.T) {
	t.Parallel()

	d := &agentDetailsDialog{agent: runtime.AgentDetails{Name: "root"}}
	items := []string{"alpha", "beta", "gamma"}

	// contentWidth=10 forces the word wrapper to put just "Sub-agents" on the
	// first line (the full prefix "Sub-agents (3):" is 15 chars, so it splits).
	// Without the HasPrefix guard this produces:
	//   BoldStyle("Sub-agents (3):") + MutedStyle("Sub-agents") → label twice.
	narrow := d.inlineList(10, "Sub-agents", items)
	require.NotNil(t, narrow)
	narrowOut := ansi.Strip(strings.Join(narrow, "\n"))
	assert.Equal(t, 1, strings.Count(narrowOut, "Sub-agents"),
		"label must appear exactly once at narrow width; got: %q", narrowOut)

	// At normal width the first wrapped line starts with the full prefix, so
	// the bold prefix is applied and the content follows on the same line.
	wide := d.inlineList(60, "Sub-agents", items)
	wideOut := ansi.Strip(strings.Join(wide, "\n"))
	assert.Contains(t, wideOut, "Sub-agents (3): alpha, beta, gamma")
}

// TestAgentDetailsDialog_OmitsInstruction documents that the inspector never
// renders the agent's instruction/system prompt: neither AgentDetails nor
// AgentConfigInfo carries it, and no section heading exposes it.
func TestAgentDetailsDialog_OmitsInstruction(t *testing.T) {
	t.Parallel()

	out := renderAgentDetails(runtime.AgentDetails{
		Name:        "root",
		Description: "Routes work to sub-agents",
		Model:       "gpt-5.4",
	}, inspection.AgentConfigInfo{
		SubAgents: []string{"coder"},
	})

	assert.Contains(t, out, "Routes work to sub-agents", "description is shown")
	assert.NotContains(t, out, "Instruction", "the system prompt is never surfaced")
	assert.NotContains(t, out, "System prompt")
}

func TestAgentDetailsDialog_ExecutionCapabilities(t *testing.T) {
	t.Parallel()
	native := &inspection.ExecutionCapabilities{
		Mode: inspection.ExecutionModeNative, ToolExecution: inspection.ToolExecutionDockerAgent,
		ToolApproval: inspection.ToolApprovalDockerAgentPolicy, BudgetSupported: true,
		CompactionSupported: true, MidTurnSteeringSupported: true, PromptHookContextSupported: true,
	}
	external := &inspection.ExecutionCapabilities{
		Mode: inspection.ExecutionModeHarness, ToolExecution: inspection.ToolExecutionHarnessReported,
		ToolApproval: inspection.ToolApprovalExternal,
	}
	out := renderAgentDetails(runtime.AgentDetails{Name: "root", ExecutionCapabilities: native}, inspection.AgentConfigInfo{})
	assert.Contains(t, out, "Execution: native")
	assert.Contains(t, out, "Tool execution: docker-agent")
	assert.Contains(t, out, "Tool approval: docker-agent-policy")
	assert.Contains(t, out, "Budgets: supported; requires runtime configuration")
	assert.Contains(t, out, "Mid-turn steering: supported")

	out = renderAgentDetails(runtime.AgentDetails{Name: "root", ExecutionCapabilities: native}, inspection.AgentConfigInfo{ExecutionCapabilities: external})
	assert.Contains(t, out, "Execution: harness")
	assert.Contains(t, out, "Tool execution: harness-reported")
	assert.Contains(t, out, "Tool approval: external")
	assert.Contains(t, out, "Budgets: unsupported")
	assert.Contains(t, out, "Native compaction: unsupported")
	assert.Contains(t, out, "Prompt-hook context: unsupported")
	assert.Contains(t, out, "docker-agent does not approve them")
	assert.NotContains(t, out, "Execution: native", "local inspection must take precedence over a stale roster")

	out = renderAgentDetails(runtime.AgentDetails{Name: "legacy", Model: "codex"}, inspection.AgentConfigInfo{})
	assert.NotContains(t, out, "Execution:")
	assert.NotContains(t, out, "Budgets:", "unknown metadata must not be inferred from a model label")
}
