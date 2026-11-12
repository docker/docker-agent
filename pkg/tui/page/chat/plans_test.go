package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestPlanSidebar_ClickRoutesDirectEdit(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p := newLayoutTestPage(t, position)
			p.SetRoutingID("origin")
			p.SetLayoutSettings(msgtypes.LayoutSettings{
				SidebarPosition: position, ShowPlans: true,
				HideUsage: true, HideAgents: true, HideTools: true, HideTodos: true,
			})
			p.SetSize(160, 40)
			p.Update(msgtypes.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
				{Name: "release", Version: new(7)},
			}}})
			sl := p.computeSidebarLayout()
			lines := strings.Split(ansi.Strip(p.sidebar.View()), "\n")
			found := false
			for y, line := range lines {
				if !strings.Contains(line, "release") {
					continue
				}
				found = true
				x := styles.AppPadding + sl.sidebarStartX + strings.Index(line, "release")
				hit := NewHitTest(p)
				require.Equal(t, TargetSidebarPlan, hit.At(x, y))
				assert.Equal(t, "release", hit.PlanName)
				_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.NotNil(t, cmd)
				msg, ok := cmd().(msgtypes.EditSidebarPlanMsg)
				require.True(t, ok)
				assert.Equal(t, plans.SharedRef("release"), msg.Ref)
				assert.Equal(t, 7, msg.ExpectedVersion)
				assert.Equal(t, "origin", msg.TabID)
				_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
				assert.Nil(t, cmd)
			}
			assert.True(t, found)
		})
	}
}

func TestPlanSidebar_BandClickOpensBrowser(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p := newLayoutTestPage(t, position)
			p.SetLayoutSettings(msgtypes.LayoutSettings{
				SidebarPosition: position, ShowPlans: true, HideAgents: true, HideTools: true, HideTodos: true,
			})
			p.SetSize(90, 40)
			p.Update(msgtypes.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{{Name: "release"}}}})
			sl := p.computeSidebarLayout()
			found := false
			for row, line := range strings.Split(ansi.Strip(p.sidebar.View()), "\n") {
				if !strings.Contains(line, "Plans (1)") {
					continue
				}
				found = true
				x, y := styles.AppPadding+strings.Index(line, "Plans"), sl.bandY()+row
				if sl.bandAtBottom {
					y++
				}
				require.Equal(t, TargetSidebarPlanBrowser, NewHitTest(p).At(x, y))
				_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.NotNil(t, cmd)
				assert.IsType(t, msgtypes.ShowPlanBrowserMsg{}, cmd())
			}
			assert.True(t, found)
		})
	}
}
