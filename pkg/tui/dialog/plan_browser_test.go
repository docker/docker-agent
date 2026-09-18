package dialog

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func letterKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// testPlanListing builds a listing with two shared plans.
func testPlanListing() plans.ListResult {
	now := time.Now().UTC()
	return plans.ListResult{
		Plans: []plans.Plan{
			{
				Scope:     plans.ScopeShared,
				Name:      "release",
				Title:     "Release plan for 2025",
				Author:    "architect",
				Status:    "in-progress",
				Version:   new(3),
				UpdatedAt: now.Add(-2 * time.Hour),
			},
			{
				Scope:     plans.ScopeShared,
				Name:      "db-migration",
				Title:     "Database migration",
				Status:    "draft",
				Version:   new(1),
				UpdatedAt: now.Add(-30 * time.Minute),
			},
		},
	}
}

func newTestPlanBrowser(t *testing.T, result plans.ListResult) *planBrowserDialog {
	t.Helper()
	d := NewPlanBrowserDialog(result).(*planBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	return d
}

func firstMsgOfType[T any](msgs []tea.Msg) (T, bool) {
	for _, msg := range msgs {
		if typed, ok := msg.(T); ok {
			return typed, true
		}
	}
	var zero T
	return zero, false
}

func TestPlanBrowserEmptyList(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, plans.ListResult{Plans: []plans.Plan{}})

	view := d.View()
	assert.Contains(t, view, "No plans yet")
	assert.Contains(t, view, "Plans (0)")

	// Actions on an empty list are safe no-ops.
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)
	_, cmd = d.Update(letterKey('x'))
	assert.Nil(t, cmd)
	_, cmd = d.Update(letterKey('d'))
	assert.Nil(t, cmd)
}

func TestPlanBrowserRendersScopeIdentityStatusVersionTimeTitle(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	view := d.View()
	assert.Contains(t, view, "shared", "scope column must name the shared scope")
	assert.Contains(t, view, "release")
	assert.Contains(t, view, "in-progress")
	assert.Contains(t, view, "v3", "plan version must be shown")
	assert.Contains(t, view, "2h ago", "updated time must be shown")
	assert.Contains(t, view, "Release plan", "title must be shown")
}

func TestPlanBrowserRowsTruncateSafely(t *testing.T) {
	t.Parallel()
	long := plans.Plan{
		Scope:     plans.ScopeShared,
		Name:      strings.Repeat("very-long-name-", 10),
		Title:     strings.Repeat("An extremely long title that cannot fit ", 20),
		Status:    strings.Repeat("status", 10),
		Version:   new(123456),
		UpdatedAt: time.Now(),
	}
	d := newTestPlanBrowser(t, plans.ListResult{Plans: []plans.Plan{long}})

	const width = 80
	row := d.renderPlan(long, false, width)
	assert.LessOrEqual(t, lipgloss.Width(row), width, "row must never overflow the dialog")

	for line := range strings.SplitSeq(d.View(), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 120, "no dialog line may exceed the dialog width")
	}
}

func TestPlanBrowserNavigation(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	require.Equal(t, 0, d.selected)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 1, d.selected)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 1, d.selected, "selection stays at the end of the list")
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, 0, d.selected)
}

func TestPlanBrowserFilter(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	// "/" enters filter mode; typing narrows the list.
	d.Update(letterKey('/'))
	require.True(t, d.filtering)
	d.Update(letterKey('r'))
	d.Update(letterKey('e'))
	d.Update(letterKey('l'))
	require.Len(t, d.filtered, 1)
	assert.Equal(t, "release", d.filtered[0].Name)

	// Esc leaves filter mode without closing the dialog.
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.False(t, d.filtering)
	assert.Nil(t, cmd)

	// Outside filter mode, esc closes.
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	msgs := collectMsgs(cmd)
	_, ok := firstMsgOfType[CloseDialogMsg](msgs)
	assert.True(t, ok)
}

func TestPlanBrowserFilterNoMatches(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	d.Update(letterKey('/'))
	d.Update(letterKey('z'))
	d.Update(letterKey('z'))
	require.Empty(t, d.filtered)
	assert.Contains(t, d.View(), "No plans match the filter")
}

func TestPlanBrowserEnterOpensDetail(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msg, ok := firstMsgOfType[messages.OpenPlanDetailMsg](collectMsgs(cmd))
	require.True(t, ok, "enter must open the detail dialog, not invoke agent tools")
	assert.Equal(t, plans.SharedRef("release"), msg.Ref)

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msg, ok = firstMsgOfType[messages.OpenPlanDetailMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("db-migration"), msg.Ref)
}

func TestPlanBrowserRefreshAndExportKeys(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	_, cmd := d.Update(letterKey('r'))
	_, ok := firstMsgOfType[messages.RefreshPlansMsg](collectMsgs(cmd))
	assert.True(t, ok, "r must request a refresh")

	_, cmd = d.Update(letterKey('x'))
	exportMsg, ok := firstMsgOfType[messages.ExportPlanMsg](collectMsgs(cmd))
	require.True(t, ok, "x must request an export")
	assert.Equal(t, plans.SharedRef("release"), exportMsg.Ref)
}

func TestPlanBrowserStatusFlow(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing()) // release, v3, selected

	_, cmd := d.Update(letterKey('s'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "s must open the status input dialog")
	statusDialog, ok := openMsg.Model.(*planStatusDialog)
	require.True(t, ok)
	assert.Equal(t, 3, statusDialog.version, "the displayed version guards the write")
	assert.Contains(t, statusDialog.View(), "release")
	assert.Contains(t, statusDialog.View(), "v3")

	// The input is prefilled with the current status; replace it.
	statusDialog.input.SetValue("done")
	_, cmd = statusDialog.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	_, closed := firstMsgOfType[CloseDialogMsg](msgs)
	assert.True(t, closed)
	statusMsg, ok := firstMsgOfType[messages.SetPlanStatusMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), statusMsg.Ref)
	assert.Equal(t, "done", statusMsg.Status)
	assert.Equal(t, 3, statusMsg.ExpectedVersion)
}

func TestPlanBrowserStatusEmptyRejected(t *testing.T) {
	t.Parallel()
	sd := newPlanStatusDialog("release", "", 3).(*planStatusDialog)
	sd.input.SetValue("   ")

	_, cmd := sd.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	_, gotStatus := firstMsgOfType[messages.SetPlanStatusMsg](msgs)
	assert.False(t, gotStatus, "an empty status must not be submitted")
	note, ok := firstMsgOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
}

// TestPlanBrowserVersionlessMutationRefused proves a plan without a known
// version (which the service always provides) is refused rather than
// mutated unguarded.
func TestPlanBrowserVersionlessMutationRefused(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, plans.ListResult{Plans: []plans.Plan{
		{Scope: plans.ScopeShared, Name: "release"},
	}})

	for _, r := range []rune{'s', 'd', 'e'} {
		_, cmd := d.Update(letterKey(r))
		msgs := collectMsgs(cmd)
		note, ok := firstMsgOfType[notification.ShowMsg](msgs)
		require.True(t, ok, "%c on a versionless plan must show an explanatory notification", r)
		assert.Contains(t, note.Text, "no version is known")
		_, opened := firstMsgOfType[OpenDialogMsg](msgs)
		assert.False(t, opened, "%c must not open an action dialog for a versionless plan", r)
		_, statusEmitted := firstMsgOfType[messages.SetPlanStatusMsg](msgs)
		assert.False(t, statusEmitted)
		_, deleteEmitted := firstMsgOfType[messages.DeletePlanMsg](msgs)
		assert.False(t, deleteEmitted)
		_, editEmitted := firstMsgOfType[messages.EditPlanMsg](msgs)
		assert.False(t, editEmitted)
	}
}

func TestPlanBrowserDeleteFlow(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing()) // release, v3, selected

	_, cmd := d.Update(letterKey('d'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "d must open a confirmation dialog")
	confirm, ok := openMsg.Model.(*planDeleteConfirmDialog)
	require.True(t, ok)

	view := confirm.View()
	assert.Contains(t, view, "release", "the confirmation must name the plan")
	assert.Contains(t, view, "version 3", "the confirmation must name the version")

	// N cancels without emitting a delete.
	_, cmd = confirm.Update(letterKey('n'))
	msgs := collectMsgs(cmd)
	_, deleted := firstMsgOfType[messages.DeletePlanMsg](msgs)
	assert.False(t, deleted)
	_, closed := firstMsgOfType[CloseDialogMsg](msgs)
	assert.True(t, closed)

	// Y confirms with the displayed version as the guard.
	_, cmd = confirm.Update(letterKey('y'))
	msgs = collectMsgs(cmd)
	deleteMsg, ok := firstMsgOfType[messages.DeletePlanMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), deleteMsg.Ref)
	assert.Equal(t, 3, deleteMsg.ExpectedVersion)
}

func TestPlanBrowserNewFlow(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	_, cmd := d.Update(letterKey('n'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "n must open the name dialog")
	nameDialog, ok := openMsg.Model.(*planNameDialog)
	require.True(t, ok)

	for _, r := range "my-plan" {
		nameDialog.Update(letterKey(r))
	}
	_, cmd = nameDialog.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	createMsg, ok := firstMsgOfType[messages.CreatePlanMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, "my-plan", createMsg.Name)
}

func TestPlanBrowserEditKey(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing()) // release, v3, selected

	_, cmd := d.Update(letterKey('e'))
	editMsg, ok := firstMsgOfType[messages.EditPlanMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), editMsg.Ref)
	assert.Equal(t, 3, editMsg.ExpectedVersion)
}

func TestPlanBrowserDataMsgReplacesRowsAndKeepsSelection(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing()) // release selected

	updated := plans.ListResult{Plans: []plans.Plan{
		{Scope: plans.ScopeShared, Name: "another", Version: new(1)},
		{Scope: plans.ScopeShared, Name: "release", Status: "done", Version: new(4)},
	}}
	d.Update(PlanBrowserDataMsg{Result: updated})

	require.Len(t, d.filtered, 2)
	p, ok := d.selectedPlan()
	require.True(t, ok)
	assert.Equal(t, "release", p.Name, "selection follows the plan identity across refreshes")
	assert.Equal(t, 4, *p.Version)
	assert.NotContains(t, d.View(), "db-migration", "removed rows must disappear")
}

// manyPlansListing builds n shared plans named plan-00, plan-01, … so tests
// can scroll a list taller than the viewport.
func manyPlansListing(n int) plans.ListResult {
	result := plans.ListResult{Plans: make([]plans.Plan, 0, n)}
	for i := range n {
		result.Plans = append(result.Plans, plans.Plan{
			Scope:   plans.ScopeShared,
			Name:    fmt.Sprintf("plan-%02d", i),
			Version: new(1),
		})
	}
	return result
}

// TestPlanBrowserDataMsgPreservesScrollOffset proves a live data refresh
// never yanks a scrolled viewport back to the top while the selected plan
// still exists — independent of how the offset came about (wheel, scrollbar,
// or keys).
func TestPlanBrowserDataMsgPreservesScrollOffset(t *testing.T) {
	t.Parallel()
	listing := manyPlansListing(40)
	d := newTestPlanBrowser(t, listing)
	viewport := d.scrollview.VisibleHeight()
	require.Greater(t, len(listing.Plans), viewport, "the list must be taller than the viewport")

	// Scroll down and select a row inside the scrolled window.
	d.selected = 30
	d.ensureSelectedVisible()
	offset := d.scrollview.ScrollOffset()
	require.Positive(t, offset)

	d.Update(PlanBrowserDataMsg{Result: manyPlansListing(40)})

	assert.Equal(t, offset, d.scrollview.ScrollOffset(), "a same-shape refresh must keep the viewport position")
	p, ok := d.selectedPlan()
	require.True(t, ok)
	assert.Equal(t, "plan-30", p.Name)
}

// TestPlanBrowserDataMsgClampsScrollOffsetWhenRowsShrink proves a refresh
// that removes rows clamps the preserved offset instead of scrolling past
// the end of the shorter list.
func TestPlanBrowserDataMsgClampsScrollOffsetWhenRowsShrink(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, manyPlansListing(40))
	viewport := d.scrollview.VisibleHeight()

	d.selected = 19 // survives the shrink below
	d.scrollview.SetScrollOffset(13)
	require.Equal(t, 13, d.scrollview.ScrollOffset())

	shrunk := manyPlansListing(20)
	d.Update(PlanBrowserDataMsg{Result: shrunk})

	wantMax := max(0, len(shrunk.Plans)-viewport)
	assert.LessOrEqual(t, d.scrollview.ScrollOffset(), wantMax, "the offset must clamp to the shorter list")
	p, ok := d.selectedPlan()
	require.True(t, ok)
	assert.Equal(t, "plan-19", p.Name)
	// The selected row is still within the visible window.
	assert.GreaterOrEqual(t, d.selected, d.scrollview.ScrollOffset())
	assert.Less(t, d.selected, d.scrollview.ScrollOffset()+viewport)
}

// TestPlanBrowserFilterResetsScrollOffset pins that user-initiated filtering
// still starts from the top: only live data refreshes preserve the offset.
func TestPlanBrowserFilterResetsScrollOffset(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, manyPlansListing(40))
	d.scrollview.SetScrollOffset(13)

	d.Update(letterKey('/'))
	d.Update(letterKey('p'))
	assert.Zero(t, d.scrollview.ScrollOffset(), "filtering starts from the top of the matches")
}

// TestPlanBrowserUpdatedAgesAdvance proves relative "updated" ages are
// rendered against the current time, not the time the dialog was opened, so
// a plan can never stay "0s ago" forever.
func TestPlanBrowserUpdatedAgesAdvance(t *testing.T) {
	t.Parallel()
	base := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	listing := plans.ListResult{Plans: []plans.Plan{
		{Scope: plans.ScopeShared, Name: "release", Version: new(1), UpdatedAt: base},
	}}
	d := newTestPlanBrowser(t, listing)

	d.now = func() time.Time { return base.Add(30 * time.Second) }
	assert.Contains(t, d.View(), "30s ago")

	d.now = func() time.Time { return base.Add(3 * time.Minute) }
	assert.Contains(t, d.View(), "3m ago", "the age must advance with the clock, without any data message")
}

func TestPlanBrowserWarningsShown(t *testing.T) {
	t.Parallel()
	result := testPlanListing()
	result.Warnings = []string{`skipped "broken": corrupt`}
	d := newTestPlanBrowser(t, result)

	assert.Contains(t, d.View(), "could not be read")
}

// TestPlanDialogMarkers pins the marker interfaces both /plans dialogs
// implement; the interfaces live in plan_browser.go, the detail-only tests
// in plan_detail_test.go.
func TestPlanDialogMarkers(t *testing.T) {
	t.Parallel()

	_, isPlanDialog := NewPlanBrowserDialog(plans.ListResult{}).(PlanDialog)
	assert.True(t, isPlanDialog)
	detail := NewPlanDetailDialog(sharedDetailPlan())
	_, isPlanDialog = detail.(PlanDialog)
	assert.True(t, isPlanDialog)

	viewer, ok := detail.(PlanDetailViewer)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), viewer.PlanRef())
}
