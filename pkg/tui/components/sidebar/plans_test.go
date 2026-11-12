package sidebar

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newPlanSidebar(t *testing.T) *model {
	t.Helper()
	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)
	m.sessionTitle = "T"
	m.workingDirectory = ""
	m.SetSize(40, 80)
	m.SetSectionVisibility(SectionVisibility{
		ShowPlans: true, HideUsage: true, HideAgents: true, HideTools: true, HideTodos: true,
	})
	return m
}

func TestPlanSidebar_RecentOrderAndLimit(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1, 5, 8} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			m := newPlanSidebar(t)
			var input []plans.Plan
			for i := range count {
				input = append(input, plans.Plan{
					Name: fmt.Sprintf("plan-%d", i), UpdatedAt: time.Unix(int64(i+1), 0), Version: new(i + 1),
				})
			}
			m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: input}})
			require.Len(t, m.recentPlans, min(count, recentPlanLimit))
			for i, p := range m.recentPlans {
				assert.Equal(t, fmt.Sprintf("plan-%d", count-i-1), p.Name)
			}
			for i, p := range input {
				assert.Equal(t, fmt.Sprintf("plan-%d", i), p.Name, "do not reorder the service/browser snapshot")
			}
			view := ansi.Strip(m.View())
			assert.Contains(t, view, fmt.Sprintf("All plans (%d)", count))
			if count == 0 {
				assert.Contains(t, view, "No plans")
			}
		})
	}
}

func TestPlanSidebar_TiesAndUnknownDates(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	stamp := time.Unix(100, 0)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "unknown"}, {Name: "z", UpdatedAt: stamp}, {Name: "a", UpdatedAt: stamp},
	}}})
	require.Len(t, m.recentPlans, 3)
	assert.Equal(t, "a", m.recentPlans[0].Name)
	assert.Equal(t, "z", m.recentPlans[1].Name)
	assert.Equal(t, "unknown", m.recentPlans[2].Name)
}

func TestPlanSidebar_ClicksTrackCanonicalPlanAndVersion(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "first", Title: "Same title", Status: "awaiting-special-review", Version: new(4)},
		{Name: "second", Title: "Same title", Version: new(8)},
	}}})
	view := ansi.Strip(m.View())
	assert.Contains(t, view, "awaiting-special-review")
	found := map[ClickResult]int{}
	for y, line := range strings.Split(view, "\n") {
		kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft+1, y)
		if kind == ClickNone {
			continue
		}
		found[kind]++
		if kind == ClickPlan {
			msg, ok := m.EditPlan(name, "tab")().(messages.EditSidebarPlanMsg)
			require.True(t, ok)
			assert.Equal(t, plans.SharedRef(name), msg.Ref)
			assert.Equal(t, "tab", msg.TabID)
			if name == "first" {
				assert.Equal(t, 4, msg.ExpectedVersion)
			} else {
				assert.Equal(t, "second", name)
				assert.Equal(t, 8, msg.ExpectedVersion)
			}
		}
		assert.NotEmpty(t, strings.TrimSpace(line))
	}
	assert.Equal(t, 5, found[ClickPlan], "all name/title/status lines belong to their plan")
	assert.Equal(t, 1, found[ClickPlanBrowser])
	assert.Equal(t, 1, found[ClickPlanRefresh])
	before := m.VisualGeneration()
	m.SetSectionVisibility(SectionVisibility{})
	assert.Greater(t, m.VisualGeneration(), before)
	for y := range strings.Split(view, "\n") {
		kind, _ := m.HandleClickType(m.layoutCfg.PaddingLeft+1, y)
		assert.NotEqual(t, ClickPlan, kind, "hiding must immediately disable old click zones")
	}
	assert.NotContains(t, ansi.Strip(m.View()), "All plans")
}

func TestPlanSidebar_StateAndCompactGeometry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data messages.PlanSidebarDataMsg
		text string
	}{
		{"loading", messages.PlanSidebarDataMsg{Loading: true}, "Loading plans"},
		{"empty", messages.PlanSidebarDataMsg{}, "No plans"},
		{"error", messages.PlanSidebarDataMsg{Err: errors.New("unavailable")}, "Plans unavailable"},
		{"warning", messages.PlanSidebarDataMsg{Result: plans.ListResult{Warnings: []string{"corrupt"}}}, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newPlanSidebar(t)
			m.setPlans(tc.data)
			assert.Contains(t, ansi.Strip(m.View()), tc.text)
			m.SetMode(ModeCollapsed)
			for _, width := range []int{10, 40, 100} {
				m.SetSize(width, 80)
				view := ansi.Strip(m.View())
				lines := strings.Split(view, "\n")
				assert.Equal(t, len(lines)+1, m.CollapsedHeight(width))
				kind, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, len(lines)-1)
				assert.Equal(t, ClickPlanBrowser, kind)
			}
		})
	}
}

func TestPlanSidebar_ScrollbarAndScrolledRows(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.SetSize(30, 6)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "one", Version: new(1)}, {Name: "two", Version: new(2)},
	}}})
	m.View()
	require.True(t, m.cachedNeedsScrollbar)
	for contentY, zone := range m.planClickZones {
		if zone.kind != ClickPlan {
			continue
		}

		m.scrollview.SetScrollOffset(contentY)
		viewportY := contentY - m.scrollview.ScrollOffset()
		kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft, viewportY)
		assert.Equal(t, ClickPlan, kind)
		assert.Equal(t, zone.name, name)
		kind, _ = m.HandleClickType(m.layoutCfg.PaddingLeft+m.contentWidth(true), viewportY)
		assert.Equal(t, ClickNone, kind, "scrollbar must not edit plans")
	}
}

func TestPlanSidebar_MultilineMetadataKeepsClickGeometry(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "release", Title: "first\nsecond\tthird", Status: "pending\r\nreview", Version: new(1)},
	}}})
	view := ansi.Strip(m.View())
	assert.Contains(t, view, "first second third")
	assert.Contains(t, view, "pending review")
	for y, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "first second") || strings.Contains(line, "pending review") {
			kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft, y)
			assert.Equal(t, ClickPlan, kind)
			assert.Equal(t, "release", name)
		}
	}
}
