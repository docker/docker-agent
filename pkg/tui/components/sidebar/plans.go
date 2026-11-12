package sidebar

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const recentPlanLimit = 5

type planClickZone struct {
	kind ClickResult
	name string
}

func (m *model) setPlans(data messages.PlanSidebarDataMsg) {
	m.planData = data
	m.recentPlans = slices.Clone(data.Result.Plans)
	slices.SortFunc(m.recentPlans, func(a, b plans.Plan) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.Name, b.Name)
	})
	m.recentPlans = m.recentPlans[:min(len(m.recentPlans), recentPlanLimit)]
	m.invalidateCache()
}

func (m *model) EditPlan(name, tabID string) tea.Cmd {
	for _, p := range m.recentPlans {
		if p.Name == name && p.Version != nil {
			return core.CmdHandler(messages.EditSidebarPlanMsg{
				TabID: tabID, Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version,
			})
		}
	}
	return notification.WarningCmd("Plan is no longer available for editing. Refresh plans and retry.")
}

func (m *model) plansSection(width int) (string, []planClickZone) {
	var lines []string
	var zones []planClickZone
	add := func(text string, zone planClickZone) {
		text = strings.Join(strings.Fields(text), " ")
		lines = append(lines, toolcommon.TruncateText(text, max(1, width)))
		zones = append(zones, zone)
	}
	switch {
	case m.planData.Err != nil:
		label := "Plans unavailable; refresh to retry"
		if len(m.recentPlans) > 0 {
			label = "Plans unavailable; showing stale data"
		}
		add(styles.MutedStyle.Render(label), planClickZone{})
	case m.planData.Loading:
		add(styles.MutedStyle.Render("Loading plans"), planClickZone{})
	case len(m.recentPlans) == 0:
		add(styles.MutedStyle.Render("No plans"), planClickZone{})
	}
	if len(m.planData.Result.Warnings) > 0 {
		add(styles.MutedStyle.Render(fmt.Sprintf("%d plan(s) could not be read", len(m.planData.Result.Warnings))), planClickZone{})
	}
	for _, p := range m.recentPlans {
		zone := planClickZone{kind: ClickPlan, name: p.Name}
		add(styles.BaseStyle.Render(p.Name), zone)
		if p.Title != "" {
			add(styles.MutedStyle.Render(p.Title), zone)
		}
		if p.Status != "" {
			add(styles.MutedStyle.Render(p.Status), zone)
		}
	}
	add(styles.MutedStyle.Render(fmt.Sprintf("All plans (%d)", len(m.planData.Result.Plans))), planClickZone{kind: ClickPlanBrowser})
	add(styles.MutedStyle.Render("Refresh plans"), planClickZone{kind: ClickPlanRefresh})
	return m.renderTab("Plans", strings.Join(lines, "\n"), width), zones
}

func (m *model) plansSummary() string {
	if !m.sectionVisibility.ShowPlans {
		return ""
	}
	label := fmt.Sprintf("Plans (%d) - open /plans", len(m.planData.Result.Plans))
	switch {
	case m.planData.Err != nil:
		label = "Plans unavailable - open /plans"
	case m.planData.Loading:
		label = "Loading plans - open /plans"
	case len(m.planData.Result.Warnings) > 0:
		label += " (warnings)"
	}
	return styles.MutedStyle.Render(label)
}
