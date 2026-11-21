package sidebar

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// newAgentPanelSidebar builds a sidebar whose current agent is "root" and whose
// roster is set, ready to render the Agents panel at the given outer width.
// The panel is pinned to the detailed card mode, which most panel tests
// target; compact-roster tests build their own sidebar (see
// agent_info_mode_test.go). The transfer-box animation is always stopped on
// cleanup so tests that start it (via SetAgentSwitching) cannot leak a
// registration on the global animation coordinator into parallel tests.
func newAgentPanelSidebar(t *testing.T, width int, agents ...runtime.AgentDetails) *model {
	t.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	m := New(animation.NewRuntimeWithScheduler(&immediateScheduler{now: time.Unix(1, 0)}), t.Context(), ss).(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "root"
	m.availableAgents = agents
	m.width = width
	m.height = 200
	m.SetAgentInfoMode(AgentInfoDetailed)
	t.Cleanup(m.transferAnimation.Stop)
	return m
}

// renderAgentPanel returns the ANSI-stripped lines of the Agents panel body.
func renderAgentPanel(m *model) []string {
	out := ansi.Strip(m.agentInfo(m.contentWidth(false)))
	return strings.Split(out, "\n")
}

const tabHeaderLines = 2 // tab title + TabStyle top padding before the body

// agentBody returns the ANSI-stripped panel body lines aligned 1:1 with
// m.agentLineOwners (populated as a side effect of rendering).
func agentBody(m *model) (body []string) {
	lines := renderAgentPanel(m)
	return lines[tabHeaderLines : tabHeaderLines+len(m.agentLineOwners)]
}

// agentCard returns all ANSI-stripped content lines owned by the named agent:
// the name line, the model line, and its labeled metric line(s).
func agentCard(m *model, name string) []string {
	body := agentBody(m)
	var lines []string
	for j, owner := range m.agentLineOwners {
		if owner == name {
			lines = append(lines, body[j])
		}
	}
	return lines
}

// agentLines returns the first two ANSI-stripped content lines owned by the
// named agent: line1 (name + shortcut) and line2 (provider/model).
func agentLines(m *model, name string) (line1, line2 string) {
	card := agentCard(m, name)
	switch len(card) {
	case 0:
		return "", ""
	case 1:
		return card[0], ""
	}
	return card[0], card[1]
}

// agentMetrics returns the joined ANSI-stripped metric line(s) of the named
// agent's card (everything after the name and model lines).
func agentMetrics(m *model, name string) string {
	card := agentCard(m, name)
	if len(card) <= 2 {
		return ""
	}
	return strings.Join(card[2:], "\n")
}

func TestClassifyThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		label    string
		wantKind thinkingKind
		wantTok  int64
	}{
		{"", thinkingNone, 0},
		{"off", thinkingOff, 0},
		{"adaptive", thinkingAdaptive, 0},
		{"8192", thinkingTokens, 8192},
		{"high", thinkingLevel, 0},
		{"minimal", thinkingLevel, 0},
	}
	for _, c := range cases {
		kind, tok := classifyThinking(c.label)
		assert.Equalf(t, c.wantKind, kind, "kind for %q", c.label)
		assert.Equalf(t, c.wantTok, tok, "tokens for %q", c.label)
	}
}

// TestAgentEntryLayout verifies an agent renders as a labeled mini-card:
// line 1 carries the name and "^N" shortcut (no description), line 2 the
// provider/model, and the metric lines the labeled effort gauge, context and
// cost. Rendered wide enough (56) for the full metric vocabulary.
func TestAgentEntryLayout(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 56,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Description: "Executive assistant", Thinking: "high"},
	)

	line1, line2 := agentLines(m, "root")
	require.NotEmpty(t, line1)
	assert.Contains(t, line1, "root", "line 1 shows the agent name")
	assert.Contains(t, line1, "^1", "line 1 shows the switch shortcut")
	assert.NotContains(t, line1, "Executive assistant", "description is not shown")
	assert.Contains(t, line2, "anthropic/claude-opus-4-8", "line 2 shows the provider/model")

	metrics := agentMetrics(m, "root")
	assert.Contains(t, metrics, "Effort "+gaugePattern(4)+" high", "metrics carry the labeled full effort gauge with its value")
	assert.Contains(t, metrics, "Context —", "metrics carry the labeled context (unknown before any run)")
	assert.Contains(t, metrics, "Cost —", "metrics carry the labeled cost (unknown before any run)")

	body := strings.Join(agentBody(m), "\n")
	assert.NotContains(t, body, "Executive assistant", "description is not shown anywhere")
}

// TestCurrentAgentMarker verifies the current agent is marked with ▶ while the
// other agents are not, and that each agent owns exactly its card's content
// lines (separators are unowned).
func TestCurrentAgentMarker(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "first", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
		runtime.AgentDetails{Name: "last", Provider: "google", Model: "gemini-flash", Thinking: "off"},
	)

	rootLine1, _ := agentLines(m, "root")
	firstLine1, _ := agentLines(m, "first")
	require.NotEmpty(t, rootLine1)
	require.NotEmpty(t, firstLine1)
	assert.Contains(t, rootLine1, "▶", "current agent is marked with ▶")
	assert.NotContains(t, firstLine1, "▶", "non-current agents have no marker")

	// Each agent owns exactly its card's content lines; separators are unowned.
	counts := map[string]int{}
	blanks := 0
	for _, owner := range m.agentLineOwners {
		if owner == "" {
			blanks++
			continue
		}
		counts[owner]++
	}
	for _, name := range []string{"first", "root", "last"} {
		assert.Equalf(t, len(agentCard(m, name)), counts[name], "agent %q owns exactly its card lines", name)
		assert.GreaterOrEqualf(t, counts[name], 3, "agent %q renders at least name, model and one metric line", name)
	}
	assert.Positive(t, blanks, "entries are separated by blank, unowned lines")
}

// TestShortcutAtRightmost verifies the "^N" shortcut is the last visible content
// on the name line: nothing is rendered to the right of it.
func TestShortcutAtRightmost(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "8192"},
	)

	for _, name := range []string{"root", "alpha"} {
		line1, _ := agentLines(m, name)
		line := strings.TrimRight(line1, " ")
		require.NotEmpty(t, line)
		assert.Truef(t, strings.HasSuffix(line, "^1") || strings.HasSuffix(line, "^2"),
			"line for %q must end with its shortcut, got %q", name, line)
	}
}

// TestShortcutColumnAlignment verifies the shortcuts align at a single right
// column across name lines regardless of name length or badge width.
func TestShortcutColumnAlignment(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "a", Provider: "openai", Model: "gpt-4o", Thinking: "off"},
		runtime.AgentDetails{Name: "longer-name", Provider: "openai", Model: "gpt-5.4", Thinking: "8192"},
	)

	end := -1
	for _, name := range []string{"root", "a", "longer-name"} {
		line1, _ := agentLines(m, name)
		line := strings.TrimRight(line1, " ")
		w := len([]rune(line))
		if end == -1 {
			end = w
		} else {
			assert.Equalf(t, end, w, "shortcuts for %q must end in a single column", name)
		}
	}
}

// TestEffortVocabularyOnCard verifies the effort vocabulary renders on the
// card's labeled metric line: effort levels keep the full six-cell gauge with
// the level word, token budgets keep the token glyph with the budget,
// adaptive reads "auto", and off shows an empty gauge with the word "off".
// Rendered wide enough (56) for the full metric vocabulary.
func TestEffortVocabularyOnCard(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 56,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
		runtime.AgentDetails{Name: "gamma", Provider: "openai", Model: "gpt-4o", Thinking: "8192"},
		runtime.AgentDetails{Name: "delta", Provider: "google", Model: "gemini", Thinking: "adaptive"},
		runtime.AgentDetails{Name: "plain", Provider: "openai", Model: "gpt-4o"},
	)

	want := map[string]string{
		"alpha": "Effort " + gaugePattern(0) + " off",
		"beta":  "Effort " + gaugePattern(4) + " high",
		"gamma": "Effort " + styles.TokenGlyph + " 8.2K",
		"delta": "Effort auto",
	}
	for name, effortText := range want {
		metrics := agentMetrics(m, name)
		require.NotEmptyf(t, metrics, "card for %q should render metrics", name)
		assert.Containsf(t, metrics, effortText, "card %q should show %q", name, effortText)
	}

	assert.NotContains(t, agentMetrics(m, "plain"), "Effort",
		"a model with no thinking configuration has no effort metric")
}

// TestModelLineLeftTruncated verifies the provider/model on line 2 keeps its
// informative tail (left-truncation with a leading ellipsis) when it overflows.
func TestModelLineLeftTruncated(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 28,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "agent2", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "off"},
	)

	_, line2 := agentLines(m, "agent2")
	require.NotEmpty(t, line2)
	assert.Contains(t, line2, "…", "overflowing model is left-truncated with an ellipsis")
	assert.Contains(t, line2, "-4-6", "informative model tail survives left-truncation")
}

// TestMoreThanNineAgentsNoShortcutBeyond9 verifies agents past the 9th get no
// "^N" shortcut hint.
func TestMoreThanNineAgentsNoShortcutBeyond9(t *testing.T) {
	t.Parallel()

	agents := []runtime.AgentDetails{
		{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
	}
	for i := 2; i <= 12; i++ {
		agents = append(agents, runtime.AgentDetails{
			Name:     "agent" + string(rune('a'+i)),
			Provider: "openai",
			Model:    "gpt-4o",
			Thinking: "off",
		})
	}
	m := newAgentPanelSidebar(t, 40, agents...)

	body := strings.Join(renderAgentPanel(m), "\n")
	assert.Contains(t, body, "^9", "the 9th agent keeps its shortcut")
	assert.NotContains(t, body, "^10", "agents beyond the 9th have no shortcut")
}

// TestDetailedCardWidthThreshold pins the adaptive compact/wide boundary of
// the detailed card: the wide vocabulary renders exactly when every agent's
// preferred joined metric line fits the metric width (the content width
// minus the two-column card indent). This roster's widest such line is
// root's "Effort <gauge> high · Context 30% · Cost $0.13" at 45 display
// columns, so with the one-column outer padding and the card indent, outer
// width 48 (metric width 45) is the first wide width and outer width 47
// (metric width 44) renders compact — the boundary is the roster's own
// widest line, not a fixed layout constant. On both sides every card keeps
// exactly one metric line: the transition trades vocabulary, never vertical
// height.
func TestDetailedCardWidthThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		width int // outer width; one column of left padding
		// metricCols is the width the metric lines get: the outer width minus
		// the outer padding and the two-column card indent.
		metricCols int
		wide       bool
		// wantMetrics are the exact indented metric lines of each card:
		// grouping and line counts guard the vertical density.
		wantMetrics map[string][]string
		notWant     []string
	}{
		{
			name:       "compact below the roster threshold",
			width:      47,
			metricCols: 44, // root's 45-column joined line no longer fits
			wantMetrics: map[string][]string{
				"root":  {"  Eff high · Ctx 30% · Cost $0.13"},
				"scout": {"  Eff off · Ctx — · Cost —"},
				"probe": {"  Eff minimal · Ctx — · Cost —"},
			},
			notWant: []string{"Effort", "Context", styles.GaugeFilled, styles.GaugeEmpty},
		},
		{
			name:       "wide at the roster threshold",
			width:      48,
			metricCols: 45, // every agent's joined line fits
			wide:       true,
			wantMetrics: map[string][]string{
				"root":  {"  Effort " + gaugePattern(4) + " high · Context 30% · Cost $0.13"},
				"scout": {"  Effort " + gaugePattern(0) + " off · Context — · Cost —"},
				"probe": {"  Effort " + gaugePattern(1) + " minimal · Context — · Cost —"},
			},
			notWant: []string{"Eff ", "Ctx"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newAgentPanelSidebar(t, tt.width,
				runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
				runtime.AgentDetails{Name: "scout", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
				runtime.AgentDetails{Name: "probe", Provider: "openai", Model: "gpt-5.4", Thinking: "minimal"},
			)
			recordAgentUsageWithCost(m, "session-root", "root", 30_000, 100_000, 0.13)

			// The roster's own threshold: its widest preferred joined line.
			widest := 0
			for _, agent := range m.availableAgents {
				widest = max(widest, lipgloss.Width(joinSegments(m.metricSegments(agent, false))))
			}
			require.Equal(t, 45, widest, "root's full joined metric line sets this roster's threshold")

			contentCols := m.contentWidth(false)
			metricCols := contentCols - agentMarkerWidth
			require.Equal(t, tt.metricCols, metricCols, "outer width must map to the intended metric columns")
			if tt.wide {
				require.GreaterOrEqual(t, metricCols, widest, "the wide side must fit the widest joined line")
			} else {
				require.Less(t, metricCols, widest, "the compact side must not fit the widest joined line")
			}

			for name, want := range tt.wantMetrics {
				card := agentCard(m, name)
				require.GreaterOrEqualf(t, len(card), 2, "card for %q renders its name and model lines", name)
				got := make([]string, 0, len(card)-2)
				for _, line := range card[2:] {
					got = append(got, strings.TrimRight(line, " "))
				}
				assert.Equalf(t, want, got, "metric lines for %q at %d metric columns", name, metricCols)

				metrics := strings.Join(got, "\n")
				for _, notWant := range tt.notWant {
					assert.NotContainsf(t, metrics, notWant, "metrics for %q at %d metric columns", name, metricCols)
				}
				for _, line := range card {
					assert.LessOrEqualf(t, lipgloss.Width(line), contentCols,
						"no card line for %q exceeds the content width: %q", name, line)
				}
			}

			line1, line2 := agentLines(m, "root")
			assert.Contains(t, line1, "root", "the name line stays readable")
			assert.Contains(t, line2, "claude-opus-4-8", "the provider/model tail stays readable")
		})
	}
}

// TestDetailedCardResizeAcrossThreshold drives SetSize/View across the
// roster's adaptive threshold through the realistic default width —
// DefaultWidth (40, metric width 37: root's 45-column joined line overflows,
// compact) → outer width 48 (metric width 45: every joined line fits, wide)
// → DefaultWidth again — and verifies each rendering carries only its own
// vocabulary at one joined metric line per three-line card, no line
// overflows the sidebar width, and returning to the default width
// reproduces the exact default rendering: nothing stale survives.
func TestDetailedCardResizeAcrossThreshold(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, DefaultWidth,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
		runtime.AgentDetails{Name: "scout", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
	)
	recordAgentUsageWithCost(m, "session-root", "root", 30_000, 100_000, 0.13)
	m.workingDirectory = "" // keep the full-view render environment-independent

	render := func(width int) []string {
		m.SetSize(width, 200)
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		for i, line := range lines {
			lines[i] = strings.TrimRight(line, " ")
		}
		return lines
	}
	assertFits := func(lines []string, width int) {
		t.Helper()
		for _, line := range lines {
			assert.LessOrEqualf(t, lipgloss.Width(line), width, "line must fit the sidebar width: %q", line)
		}
	}
	cardLines := func() map[string]int {
		counts := map[string]int{}
		for _, owner := range m.agentLineOwners {
			if owner != "" {
				counts[owner]++
			}
		}
		return counts
	}

	defaultBefore := render(DefaultWidth)
	compact := strings.Join(defaultBefore, "\n")
	require.Contains(t, compact, "Eff high · Ctx 30% · Cost $0.13",
		"the default width joins root's compact metrics on one line")
	require.Contains(t, compact, "Eff off · Ctx — · Cost —",
		"the default width joins scout's compact metrics on one line")
	require.NotContains(t, compact, "Effort", "the default width drops the full effort label")
	require.NotContains(t, compact, "Context", "the default width drops the full context label")
	require.NotContains(t, compact, styles.GaugeFilled, "the default width omits the decorative gauge")
	require.NotContains(t, compact, styles.GaugeEmpty, "the default width omits the decorative gauge")
	require.Equal(t, map[string]int{"root": 3, "scout": 3}, cardLines(),
		"compact cards are three lines: name, model, one joined metric line")
	assertFits(defaultBefore, DefaultWidth)

	wideLines := render(48) // metric width 45: this roster's widest joined line fits exactly
	wide := strings.Join(wideLines, "\n")
	assert.Contains(t, wide, "Effort "+gaugePattern(4)+" high · Context 30% · Cost $0.13",
		"widening restores the full labels and gauge on one joined line")
	assert.Contains(t, wide, "Effort "+gaugePattern(0)+" off · Context — · Cost —")
	assert.NotContains(t, wide, "Eff ", "no compact effort label survives the widening")
	assert.NotContains(t, wide, "Ctx", "no compact context label survives the widening")
	assert.Equal(t, map[string]int{"root": 3, "scout": 3}, cardLines(),
		"wide cards keep the same three-line height: the transition adds no lines")
	assertFits(wideLines, 48)

	defaultAfter := render(DefaultWidth)
	assert.Equal(t, defaultBefore, defaultAfter,
		"returning to the default width reproduces the exact default render — nothing stale survives")
}

// TestClickZonesEveryLine verifies that clicking any rendered agent line (either
// the name line or the model line) resolves to the correct agent.
func TestClickZonesEveryLine(t *testing.T) {
	t.Parallel()

	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	sb := New(animation.NewRuntime(), t.Context(), ss)
	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "root"
	m.availableAgents = []runtime.AgentDetails{
		{Name: "first", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
	}
	m.width = 40
	m.height = 200

	_ = sb.View()

	paddingLeft := m.layoutCfg.PaddingLeft
	foundCurrent := false
	foundOther := false
	for y := range len(m.cachedLines) {
		result, name := sb.HandleClickType(paddingLeft+2, y)
		if result != ClickAgent {
			continue
		}
		if name == "root" {
			foundCurrent = true
		}
		if name == "first" {
			foundOther = true
		}
	}
	assert.True(t, foundCurrent, "clicking the current agent's line switches to it")
	assert.True(t, foundOther, "clicking another agent's line switches to it")
}

// TestRosterSeparatesAgentsWithBlankLine verifies a blank separator line is
// inserted between agent cards and that the separator carries an empty owner,
// so each agent owns exactly its card's content lines and click zones stay
// aligned.
func TestRosterSeparatesAgentsWithBlankLine(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
	)

	_ = renderAgentPanel(m) // populates agentLineOwners

	counts := map[string]int{}
	blanks := 0
	for _, owner := range m.agentLineOwners {
		if owner == "" {
			blanks++
			continue
		}
		counts[owner]++
	}
	for _, name := range []string{"root", "alpha", "beta"} {
		assert.Equalf(t, len(agentCard(m, name)), counts[name],
			"agent %q owns exactly its card lines, not the separator", name)
	}
	assert.Positive(t, blanks, "agents are separated by blank, unowned lines")

	// The panel does not start with a separator, and a blank separator precedes
	// the alpha entry.
	require.NotEmpty(t, m.agentLineOwners)
	assert.NotEmpty(t, m.agentLineOwners[0], "the panel does not start with a separator")
	alphaStart := -1
	for i, owner := range m.agentLineOwners {
		if owner == "alpha" {
			alphaStart = i
			break
		}
	}
	require.Positive(t, alphaStart, "alpha should own lines after root")
	assert.Empty(t, m.agentLineOwners[alphaStart-1], "a blank separator precedes the alpha entry")
}
