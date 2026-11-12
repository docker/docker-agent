package dialog

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func newTestPlanDetail(t *testing.T, p plans.Plan) *planDetailDialog {
	t.Helper()
	d := NewPlanDetailDialog(p).(*planDetailDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return d
}

func sharedDetailPlan() plans.Plan {
	return plans.Plan{
		Scope:     plans.ScopeShared,
		Name:      "release",
		Title:     "Release plan",
		Author:    "architect",
		Status:    "in-progress",
		Content:   "# Release\n\nStep one.",
		Version:   new(3),
		UpdatedAt: time.Now().Add(-time.Hour),
	}
}

func TestPlanDetailRendersSharedMetadata(t *testing.T) {
	t.Parallel()
	d := newTestPlanDetail(t, sharedDetailPlan())

	view := d.View()
	assert.Contains(t, view, "shared — collaborative, versioned")
	assert.Contains(t, view, "release")
	assert.Contains(t, view, "in-progress")
	assert.Contains(t, view, "v3")
	assert.Contains(t, view, "architect")
	assert.Contains(t, view, "1h ago")
	assert.Contains(t, view, "Step one.")
}

func TestPlanDetailScrollsLongContent(t *testing.T) {
	t.Parallel()
	p := sharedDetailPlan()
	var body strings.Builder
	for i := range 200 {
		body.WriteString("line ")
		body.WriteString(strings.Repeat("x", i%10))
		body.WriteString("\n")
	}
	p.Content = body.String()
	d := newTestPlanDetail(t, p)

	before := d.View()
	require.Equal(t, 0, d.scrollview.ScrollOffset())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 1, d.scrollview.ScrollOffset(), "down must scroll the content")
	d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	assert.Greater(t, d.scrollview.ScrollOffset(), 1)

	after := d.View()
	assert.Equal(t, lipgloss.Height(before), lipgloss.Height(after), "dialog height must stay stable while scrolling")
}

func TestPlanDetailKeysEmitIntents(t *testing.T) {
	t.Parallel()
	d := newTestPlanDetail(t, sharedDetailPlan())

	_, cmd := d.Update(letterKey('r'))
	_, ok := firstMsgOfType[messages.RefreshPlansMsg](collectMsgs(cmd))
	assert.True(t, ok)

	_, cmd = d.Update(letterKey('x'))
	exportMsg, ok := firstMsgOfType[messages.ExportPlanMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), exportMsg.Ref)

	_, cmd = d.Update(letterKey('e'))
	editMsg, ok := firstMsgOfType[messages.EditPlanMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, 3, editMsg.ExpectedVersion)

	_, cmd = d.Update(letterKey('s'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, isStatus := openMsg.Model.(*planStatusDialog)
	assert.True(t, isStatus)

	_, cmd = d.Update(letterKey('d'))
	openMsg, ok = firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, isConfirm := openMsg.Model.(*planDeleteConfirmDialog)
	assert.True(t, isConfirm)

	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	_, closed := firstMsgOfType[CloseDialogMsg](collectMsgs(cmd))
	assert.True(t, closed)
}

func TestPlanDetailDataMsgAppliesOnlyMatchingPlan(t *testing.T) {
	t.Parallel()
	d := newTestPlanDetail(t, sharedDetailPlan())

	other := sharedDetailPlan()
	other.Name = "other"
	other.Status = "done"
	d.Update(PlanDetailDataMsg{Plan: other})
	assert.Contains(t, d.View(), "in-progress", "data for another plan must be ignored")

	updated := sharedDetailPlan()
	updated.Status = "done"
	updated.Version = new(4)
	updated.Content = "new content"
	d.Update(PlanDetailDataMsg{Plan: updated})
	view := d.View()
	assert.Contains(t, view, "done")
	assert.Contains(t, view, "v4")
	assert.Contains(t, view, "new content")
}

// TestPlanDetailUpdatedAgeAdvances proves the detail's relative "updated"
// age is rendered against the current time, so it advances without a data
// refresh.
func TestPlanDetailUpdatedAgeAdvances(t *testing.T) {
	t.Parallel()
	p := sharedDetailPlan()
	base := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	p.UpdatedAt = base
	d := newTestPlanDetail(t, p)

	d.now = func() time.Time { return base.Add(45 * time.Minute) }
	assert.Contains(t, d.View(), "45m ago")

	d.now = func() time.Time { return base.Add(2 * time.Hour) }
	assert.Contains(t, d.View(), "2h ago", "the age must advance with the clock, without any data message")
}
