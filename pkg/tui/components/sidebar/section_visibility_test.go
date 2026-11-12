package sidebar

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	todo "github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
)

// newVisibilityTestSidebar builds a sidebar with data in every optional
// section so hiding each one is observable.
func newVisibilityTestSidebar(tb testing.TB) *testSidebar {
	tb.Helper()
	s := newTestSidebar(tb)
	s.sessionState.SetCurrentAgentName("root")
	s.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "openai", Model: "gpt-4"}})
	s.SetToolsetInfo(12, false)
	s.recordUsageTokens("session-1", "root", 500, 500)
	return s
}

func renderedSections(s *testSidebar) string {
	return ansi.Strip(strings.Join(s.renderSections(40), "\n"))
}

func TestRenderSections_AllVisibleByDefault(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	out := renderedSections(s)

	assert.Contains(t, out, "Token Usage")
	assert.Contains(t, out, "Agent")
	assert.Contains(t, out, "Tools")
	assert.Contains(t, out, "12 tools available")
}

func TestRenderSections_HideUsage(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	s.SetSectionVisibility(SectionVisibility{HideUsage: true})
	out := renderedSections(s)

	assert.NotContains(t, out, "Token Usage")
	assert.Contains(t, out, "Tools", "other sections stay visible")
}

func TestRenderSections_HideTools(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	s.SetSectionVisibility(SectionVisibility{HideTools: true})
	out := renderedSections(s)

	assert.NotContains(t, out, "12 tools available")
	assert.Contains(t, out, "Token Usage")
}

func TestRenderSections_HideSessionPath(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	s.workingDirectory = "~/projects/myapp"
	s.gitBranchName = "main"

	out := renderedSections(s)
	require.Contains(t, out, "~/projects/myapp (main)")
	visibleLines := len(s.renderSections(40))

	s.SetSectionVisibility(SectionVisibility{HideSessionPath: true})
	out = renderedSections(s)

	assert.NotContains(t, out, "~/projects/myapp")
	assert.NotContains(t, out, "(main)", "the branch suffix goes with the path")
	assert.Contains(t, out, "Session", "the session tab and title stay visible")
	assert.Contains(t, out, "Token Usage", "other sections stay visible")

	hiddenLines := len(s.renderSections(40))
	assert.Equal(t, visibleLines-2, hiddenLines,
		"hiding the path drops its line and separator without leaving a blank row")
}

func TestRenderSections_HideAgentsClearsClickZones(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)

	s.renderSections(40)
	assert.NotEmpty(t, s.agentClickZones, "visible agents register click zones")

	s.SetSectionVisibility(SectionVisibility{HideAgents: true})
	out := renderedSections(s)

	assert.NotContains(t, out, "openai/gpt-4")
	assert.Empty(t, s.agentClickZones, "hidden agents must not keep stale click zones")
}

func TestCollapsedViewModel_HideUsage(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)

	vm := s.computeCollapsedViewModel(60)
	assert.NotEmpty(t, vm.UsageSummary)

	s.SetSectionVisibility(SectionVisibility{HideUsage: true})
	vm = s.computeCollapsedViewModel(60)
	assert.Empty(t, vm.UsageSummary, "collapsed band omits usage when hidden")
}

func TestCollapsedViewModel_HideSessionPath(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	s.workingDirectory = "~/projects/myapp"

	vm := s.computeCollapsedViewModel(60)
	require.NotEmpty(t, vm.WorkingDir)

	s.SetSectionVisibility(SectionVisibility{HideSessionPath: true})
	vm = s.computeCollapsedViewModel(60)
	assert.Empty(t, vm.WorkingDir, "collapsed band omits the path when hidden")
	require.NotEmpty(t, vm.UsageSummary)

	rendered := ansi.Strip(RenderCollapsedView(vm))
	assert.NotContains(t, rendered, "myapp")
	assert.Contains(t, rendered, ansi.Strip(vm.UsageSummary), "usage keeps its band row")
}

func TestCollapsedView_HiddenPathLeavesNoBlankRow(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	s.workingDirectory = "~/projects/myapp"
	s.gitBranchName = ""

	withPath := s.computeCollapsedViewModel(60).LineCount()

	s.SetSectionVisibility(SectionVisibility{HideSessionPath: true, HideUsage: true})
	vm := s.computeCollapsedViewModel(60)
	assert.Equal(t, withPath-1, vm.LineCount(), "the path+usage row disappears entirely")

	lines := strings.Split(RenderCollapsedView(vm), "\n")
	assert.Len(t, lines, vm.LineCount()-1, "LineCount includes the divider, the render does not")
	for _, line := range lines {
		assert.NotEmpty(t, strings.TrimSpace(ansi.Strip(line)), "no blank metadata row may remain")
	}
}

func TestCollapsedInfoLine_ShowsAgentsToolsTodos(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	require.NoError(t, s.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{
		{Description: "first", Status: "completed"},
		{Description: "second", Status: "pending"},
	}}))

	info := ansi.Strip(s.collapsedInfoLine(60))
	assert.Contains(t, info, "▶ root")
	assert.Contains(t, info, "12 tools")
	assert.Contains(t, info, "1/2 todos")

	vm := s.computeCollapsedViewModel(60)
	assert.NotEmpty(t, vm.InfoLine, "band view model carries the info line")
}

// TestCollapsedInfoLine_ShowsTeamRoster verifies the band lists every team
// agent by name (not a bare "+N" count), so a team run keeps its roster
// visible when the sidebar sits at the top or bottom.
func TestCollapsedInfoLine_ShowsTeamRoster(t *testing.T) {
	t.Parallel()

	s := newTestSidebar(t)
	s.sessionState.SetCurrentAgentName("root")
	s.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4"},
		{Name: "planner", Provider: "openai", Model: "gpt-4"},
		{Name: "reviewer", Provider: "openai", Model: "gpt-4"},
	})
	_ = s.SetAgentInfo("root", "openai/gpt-4", "", 0, "", 0)

	info := ansi.Strip(s.collapsedInfoLine(120))
	assert.Contains(t, info, "▶ root openai/gpt-4", "current agent leads with its model")
	assert.Contains(t, info, "planner")
	assert.Contains(t, info, "reviewer")
	assert.NotContains(t, info, "+2", "roster names replace the bare count")
}

func TestCollapsedInfoLine_HonorsVisibility(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	require.NoError(t, s.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{
		{Description: "first", Status: "pending"},
	}}))

	s.SetSectionVisibility(SectionVisibility{HideAgents: true, HideTodos: true})
	info := ansi.Strip(s.collapsedInfoLine(60))
	assert.NotContains(t, info, "▶ root")
	assert.NotContains(t, info, "todos")
	assert.Contains(t, info, "12 tools", "tools stay visible")

	s.SetSectionVisibility(SectionVisibility{HideAgents: true, HideTools: true, HideTodos: true})
	assert.Empty(t, s.collapsedInfoLine(60), "hiding every section removes the line")
}

func TestCollapsedLineCount_GrowsWithInfoLine(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)

	withInfo := s.computeCollapsedViewModel(60).LineCount()
	s.SetSectionVisibility(SectionVisibility{HideAgents: true, HideTools: true, HideTodos: true})
	withoutInfo := s.computeCollapsedViewModel(60).LineCount()

	assert.Equal(t, withoutInfo+1, withInfo, "the info line adds one band line")
}

// sectionTitleGap returns the number of consecutive blank lines immediately
// above the rendered line containing title, or -1 when the title is absent.
func sectionTitleGap(lines []string, title string) int {
	for i, line := range lines {
		if !strings.Contains(ansi.Strip(line), title) {
			continue
		}
		gap := 0
		for j := i - 1; j >= 0 && strings.TrimSpace(ansi.Strip(lines[j])) == ""; j-- {
			gap++
		}
		return gap
	}
	return -1
}

func TestRenderSections_SectionGap(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)

	for _, gap := range []int{1, 2, 3} {
		s.SetSectionGap(gap)
		lines := s.renderSections(40)
		assert.Equal(t, gap, sectionTitleGap(lines, "Token Usage"), "gap %d before Token Usage", gap)
		assert.Equal(t, gap, sectionTitleGap(lines, "Tools"), "gap %d before Tools", gap)
	}
}

func TestRenderSections_SectionGapKeepsAgentClickZones(t *testing.T) {
	t.Parallel()

	s := newVisibilityTestSidebar(t)
	s.SetSectionGap(3)
	lines := s.renderSections(40)

	require.NotEmpty(t, s.agentClickZones)
	zonesByAgent := map[string][]int{}
	for lineIdx, agentName := range s.agentClickZones {
		require.Less(t, lineIdx, len(lines))
		zonesByAgent[agentName] = append(zonesByAgent[agentName], lineIdx)
	}
	for agentName, zone := range zonesByAgent {
		slices.Sort(zone)
		assert.Containsf(t, ansi.Strip(lines[zone[0]]), agentName,
			"the first zone line of agent %q must carry its name", agentName)
		for i := 1; i < len(zone); i++ {
			assert.Equalf(t, zone[0]+i, zone[i],
				"agent %q's card zones must be contiguous", agentName)
		}
	}
}

func TestSetSectionGap_NoopWhenUnchanged(t *testing.T) {
	t.Parallel()

	s := newTestSidebar(t)
	s.renderSections(40)
	s.cacheDirty = false

	s.SetSectionGap(defaultSectionGap)
	assert.False(t, s.cacheDirty, "identical gap must not invalidate the cache")

	s.SetSectionGap(1)
	assert.True(t, s.cacheDirty)
}

func TestSetSectionVisibility_NoopWhenUnchanged(t *testing.T) {
	t.Parallel()

	s := newTestSidebar(t)
	s.renderSections(40)
	s.cacheDirty = false

	s.SetSectionVisibility(SectionVisibility{})
	assert.False(t, s.cacheDirty, "identical visibility must not invalidate the cache")

	s.SetSectionVisibility(SectionVisibility{HideTodos: true})
	assert.True(t, s.cacheDirty)
}

func TestSetMirroredPadding_SwapsEdgePadding(t *testing.T) {
	t.Parallel()

	s := newTestSidebar(t)
	defaults := DefaultLayoutConfig()

	s.SetMirroredPadding(true)
	assert.Equal(t, defaults.PaddingRight, s.layoutCfg.PaddingLeft, "left padding moves to the chat side")
	assert.Equal(t, defaults.PaddingLeft, s.layoutCfg.PaddingRight)

	s.cacheDirty = false
	s.SetMirroredPadding(true)
	assert.False(t, s.cacheDirty, "reapplying the same padding must not invalidate the cache")

	s.SetMirroredPadding(false)
	assert.Equal(t, defaults, s.layoutCfg)
}
