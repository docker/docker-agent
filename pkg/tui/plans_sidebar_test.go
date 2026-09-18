package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestPlanSidebar_DisabledDoesNotLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		show, lean, hidden bool
	}{
		{name: "default"},
		{name: "lean", show: true, lean: true},
		{name: "hidden", show: true, hidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newPlansTestModel(t)
			m.layoutSettings.ShowPlans, m.leanMode, m.hideSidebar = tc.show, tc.lean, tc.hidden
			assert.Nil(t, m.refreshPlanSidebarCmd())
			assert.False(t, m.planRefreshInFlight)
			_, cmd := m.handleEditSidebarPlan(messages.EditSidebarPlanMsg{Ref: plans.SharedRef("p"), ExpectedVersion: 1})
			assert.Nil(t, cmd)
		})
	}
}

func TestPlanSidebar_EditIntentFromAnotherTabIsIgnored(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "content")
	m.supervisor = supervisor.New(nil)
	activeID := m.supervisor.AddSession(t.Context(), nil, session.New(), "", nil)
	otherID := m.supervisor.AddSession(t.Context(), nil, session.New(), "", nil)
	require.Equal(t, activeID, m.supervisor.ActiveID())
	_, cmd := m.Update(messages.EditSidebarPlanMsg{
		TabID: otherID, Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version,
	})
	assert.Nil(t, cmd)
	assert.False(t, m.sidebarPlanEditInFlight)
}

func TestPlanSidebar_RefreshWithoutDialogs(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "first")
	cmd := m.refreshPlanSidebarCmd()
	assert.True(t, m.planSidebarData.Loading)
	drainPlanFlow(t, m, cmd)
	assert.False(t, m.planSidebarData.Loading)
	require.Len(t, m.planSidebarData.Result.Plans, 1)
	assert.Equal(t, p.Name, m.planSidebarData.Result.Plans[0].Name)
	assert.False(t, m.planDialogOpen(), "loading a sidebar must not open the browser")

	_, err := svc.SetStatus(t.Context(), plans.SetStatusRequest{Ref: plans.SharedRef(p.Name), Status: "awaiting-special-review", ExpectedVersion: p.Version})
	require.NoError(t, err)
	runPlanFlow(t, m, runtime.PlanChanged("shared", p.Name, "status", 2, ""))
	assert.Equal(t, "awaiting-special-review", m.planSidebarData.Result.Plans[0].Status)
}

func TestPlanSidebar_RefreshFailureRetainsRowsAndRecovers(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	mustCreatePlan(t, svc, "release", "first")
	drainPlanFlow(t, m, m.refreshPlanSidebarCmd())
	_, cmd := m.handlePlanRefreshed(planRefreshedMsg{listErr: errors.New("offline")})
	assert.Empty(t, notificationTexts(collectMsgs(cmd)), "background errors live in the section, not repeated toasts")
	require.Len(t, m.planSidebarData.Result.Plans, 1)
	require.Error(t, m.planSidebarData.Err)
	_, cmd = m.handlePlanRefreshed(planRefreshedMsg{listErr: errors.New("offline"), notifyWarnings: true})
	assert.NotEmpty(t, notificationTexts(collectMsgs(cmd)), "explicit refresh failures notify")
	drainPlanFlow(t, m, m.refreshPlanSidebarCmd())
	assert.NoError(t, m.planSidebarData.Err)
}

func TestPlanSidebar_EditOpensWithoutBrowserAndDeduplicates(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "draft content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)
	msg := messages.EditSidebarPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version}
	_, cmd := m.Update(msg)
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.readsStarted.Load())
	_, duplicate := m.Update(msg)
	assert.Nil(t, duplicate)
	close(blocking.release)
	ready, ok := cmd().(planEditReadyMsg)
	require.True(t, ok)
	require.NoError(t, ready.err)
	require.NoError(t, ready.draftErr)
	t.Cleanup(func() { _ = os.Remove(ready.draftPath) })
	content, err := os.ReadFile(ready.draftPath)
	require.NoError(t, err)
	assert.Equal(t, "draft content", string(content))
	_, editorCmd := m.Update(ready)
	require.NotNil(t, editorCmd, "a valid sidebar request must launch the existing editor")
	assert.Empty(t, notificationTexts(collectMsgs(editorCmd)))
	assert.False(t, m.planDialogOpen())
	_, duplicate = m.Update(msg)
	assert.Nil(t, duplicate, "keep duplicate guard until editor closes")
	runPlanFlow(t, m, planEditorClosedMsg{ref: ready.ref, expectedVersion: ready.expectedVersion, path: ready.draftPath, sidebarRequest: ready.sidebarRequest})
	assert.False(t, m.sidebarPlanEditInFlight)
}

func TestPlanSidebar_CancelledEditDoesNotTakeOverTerminal(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"escape", "disabled", "modal", "generation"} {
		t.Run(cause, func(t *testing.T) {
			t.Parallel()
			m, svc := newPlansTestModel(t)
			m.layoutSettings.ShowPlans = true
			p := mustCreatePlan(t, svc, "release", "content")
			_, cmd := m.Update(messages.EditSidebarPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version})
			ready := cmd().(planEditReadyMsg)
			require.NotEmpty(t, ready.draftPath)
			t.Cleanup(func() { _ = os.Remove(ready.draftPath) })
			switch cause {
			case "escape":
				m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "disabled":
				m.layoutSettings.ShowPlans = false
				m.cancelSidebarPlanEdit()
			case "modal":
				m.Update(dialog.OpenDialogMsg{Model: dialog.NewPlanBrowserDialog(plans.ListResult{})})
			case "generation":
				m.cancelSidebarPlanEdit()
			}
			_, cmd = m.Update(ready)
			assert.Nil(t, cmd)
			_, err := os.Stat(ready.draftPath)
			assert.True(t, os.IsNotExist(err), "an unused seeded draft must be removed")
		})
	}
}

func TestPlanSidebar_StaleRevisionAndMissingPlan(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef(p.Name), ExpectedVersion: p.Version, Content: "v2"})
	require.NoError(t, err)
	msgs := runPlanFlow(t, m, messages.EditSidebarPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version})
	assert.Contains(t, strings.Join(notificationTexts(msgs), "\n"), "click the plan again")
	require.Len(t, m.planSidebarData.Result.Plans, 1)
	assert.Equal(t, 2, *m.planSidebarData.Result.Plans[0].Version)
	assert.False(t, m.sidebarPlanEditInFlight)
	msgs = runPlanFlow(t, m, messages.EditSidebarPlanMsg{Ref: plans.SharedRef("deleted"), ExpectedVersion: 1})
	assert.Contains(t, strings.Join(notificationTexts(msgs), "\n"), "deleted")
	assert.False(t, m.sidebarPlanEditInFlight)
}

func TestPlanSidebar_DisabledDuringRefreshDropsResult(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	mustCreatePlan(t, svc, "release", "content")
	cmd := m.refreshPlanSidebarCmd()
	assert.Nil(t, m.refreshPlanSidebarCmd(), "coalesce repeated requests")
	m.layoutSettings.ShowPlans = false
	msgs := drainPlanFlow(t, m, cmd)
	assert.Empty(t, msgs)
	assert.False(t, m.planRefreshInFlight)
	assert.False(t, m.planRefreshQueued)
	assert.False(t, m.planSidebarData.Loading)
	assert.Empty(t, m.planSidebarData.Result.Plans)
}

type planSidebarTestPage struct {
	mockChatPage

	data            messages.PlanSidebarDataMsg
	snapshotEffects chat.Effects
	eventEffects    chat.Effects
	snapshotUpdates int
}

func (p *planSidebarTestPage) UpdateEffects(msg tea.Msg) (chat.Page, chat.Effects) {
	if data, ok := msg.(messages.PlanSidebarDataMsg); ok {
		p.data = data
		p.snapshotUpdates++
		return p, p.snapshotEffects
	}
	return p, p.eventEffects
}

func TestPlanSidebar_SnapshotRespectsTabEffectVisibility(t *testing.T) {
	t.Parallel()
	m, _ := newPlansTestModel(t)
	active := &planSidebarTestPage{snapshotEffects: chat.Effects{
		Visible: func() tea.Msg { return "active-visible" },
	}}
	background := &planSidebarTestPage{snapshotEffects: chat.Effects{
		Local:   func() tea.Msg { return "background-local" },
		Global:  func() tea.Msg { return "background-global" },
		Visible: func() tea.Msg { return "background-visible" },
	}}
	m.ensureTab("active").chatPage = active
	m.ensureTab("background").chatPage = background
	unloaded := m.ensureTab("unloaded")
	m.activeTab = m.tabs["active"]
	data := messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{{Name: "release"}}}}
	msgs := collectMsgs(m.updatePlanSidebar(data))
	assert.ElementsMatch(t, []tea.Msg{"active-visible", "background-local", "background-global"}, msgs)
	assert.Equal(t, data, active.data)
	assert.Equal(t, data, background.data)
	assert.Equal(t, 1, active.snapshotUpdates)
	assert.Equal(t, 1, background.snapshotUpdates)
	assert.Nil(t, unloaded.chatPage, "refresh must not initialize a restored tab")
	assert.Same(t, active, m.activeTab.chatPage)
}

func TestPlanSidebar_TabLifecycleCancelsPendingEdit(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"switch", "close", "recreate"} {
		t.Run(cause, func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			svc := plans.NewService(plan.NewFilesystemStorage(t.TempDir()))
			WithPlansService(svc)(m)
			firstID := m.supervisor.ActiveID()
			_, _ = m.handleSpawnSession("/second")
			secondID := m.supervisor.ActiveID()
			_, _ = m.handleSwitchTab(firstID)
			m.layoutSettings.ShowPlans = true
			p := mustCreatePlan(t, svc, "release", "content")
			_, cmd := m.Update(messages.EditSidebarPlanMsg{
				TabID: firstID, Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version,
			})
			require.NotNil(t, cmd)
			ready := cmd().(planEditReadyMsg)
			require.NoError(t, ready.err)
			require.NoError(t, ready.draftErr)
			require.NotEmpty(t, ready.draftPath)
			t.Cleanup(func() { _ = os.Remove(ready.draftPath) })

			switch cause {
			case "switch":
				_, _ = m.handleSwitchTab(secondID)
			case "close":
				_, _ = m.handleCloseTab(firstID)
			case "recreate":
				m.initSessionComponents(firstID, m.application, m.application.Session())
			}

			assert.False(t, m.sidebarPlanEditInFlight)
			_, cmd = m.Update(ready)
			assert.Nil(t, cmd, "an old page must not start an editor on its replacement")
			_, err := os.Stat(ready.draftPath)
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestPlanSidebar_RecreatedPageInheritsSharedSnapshot(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.layoutSettings.ShowPlans = true
	m.planSidebarData = messages.PlanSidebarDataMsg{Result: plans.ListResult{
		Plans: []plans.Plan{{Name: "fresh-plan", Version: new(1)}},
	}}
	id := m.supervisor.ActiveID()
	oldPage := m.activeTab.chatPage
	m.initSessionComponents(id, m.application, m.application.Session())
	_ = m.activeTab.chatPage.SetSize(160, 40)
	assert.NotSame(t, oldPage, m.activeTab.chatPage)
	assert.Same(t, m.tabs[id], m.activeTab)
	assert.Contains(t, m.activeTab.chatPage.View(), "fresh-plan")
}

func TestPlanSidebar_BackgroundEventsUpdateEveryPage(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "content")
	sv := supervisor.New(nil)
	activeID := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	backgroundID := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	m.supervisor = sv
	active, background := &planSidebarTestPage{}, &planSidebarTestPage{}
	background.eventEffects = chat.Effects{
		Local:   func() tea.Msg { return timerMarkerMsg{} },
		Global:  func() tea.Msg { return globalMarkerMsg{} },
		Visible: func() tea.Msg { return uiMarkerMsg{} },
	}
	m.ensureTab(activeID).chatPage = active
	m.ensureTab(backgroundID).chatPage = background
	m.activeTab = m.tabs[activeID]
	msgs := runPlanFlow(t, m, messages.RoutedMsg{
		SessionID: backgroundID,
		Inner:     runtime.PlanChanged("shared", p.Name, "write", *p.Version, ""),
	})
	assert.True(t, hasMsg[timerMarkerMsg](msgs), "keep background tab work while refreshing")
	assert.True(t, hasMsg[globalMarkerMsg](msgs), "keep existing global effects")
	assert.False(t, hasMsg[uiMarkerMsg](msgs), "do not dispatch hidden-page UI effects")
	for _, page := range []*planSidebarTestPage{active, background} {
		require.Len(t, page.data.Result.Plans, 1)
		assert.Equal(t, p.Name, page.data.Result.Plans[0].Name)
		assert.False(t, page.data.Loading)
	}
	assert.Same(t, active, m.activeTab.chatPage)
	assert.False(t, m.planDialogOpen())
}

func TestPlanSidebar_CancelledBrowserEditStaysCancelled(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{})
	_, cmd := m.Update(messages.EditPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version})
	ready := cmd().(planEditReadyMsg)
	require.Zero(t, ready.sidebarRequest)
	t.Cleanup(func() { _ = os.Remove(ready.draftPath) })
	m.Update(dialog.CloseDialogMsg{})
	_, cmd = m.Update(ready)
	assert.Nil(t, cmd, "sidebar visibility must not revive an edit requested by the closed browser")
	_, err := os.Stat(ready.draftPath)
	assert.True(t, os.IsNotExist(err))
	_, cmd = m.Update(planDetailLoadedMsg{ref: plans.SharedRef(p.Name), plan: p})
	assert.Nil(t, cmd, "sidebar visibility must not revive a detail requested by the closed browser")
}
