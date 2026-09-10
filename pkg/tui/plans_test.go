package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// newPlansTestModel wires an appModel around a temp-backed plans service and
// a real session, returning the model and the service.
func newPlansTestModel(t *testing.T) (*appModel, plans.Service) {
	t.Helper()
	m, _ := newTestModel(t)

	svc := plans.NewService(plan.NewFilesystemStorage(t.TempDir()))
	WithPlansService(svc)(m)

	sess := session.New()
	m.application = app.New(t.Context(), stubRuntime{}, sess)
	m.sessionState = service.NewSessionState(sess)
	return m, svc
}

func mustCreatePlan(t *testing.T, svc plans.Service, name, content string) plans.Plan {
	t.Helper()
	p, err := svc.Create(t.Context(), plans.CreateRequest{Ref: plans.SharedRef(name), Content: content})
	require.NoError(t, err)
	return p
}

// openPlanBrowser puts a plan browser dialog on the model's dialog stack, as
// if /plans had been run.
func openPlanBrowser(t *testing.T, m *appModel, result plans.ListResult) {
	t.Helper()
	openDialog(t, m, dialog.NewPlanBrowserDialog(result))
}

// openDialog pushes d onto the model's dialog stack.
func openDialog(t *testing.T, m *appModel, d dialog.Dialog) {
	t.Helper()
	updated, _ := m.dialogMgr.Update(dialog.OpenDialogMsg{Model: d})
	m.dialogMgr = updated.(dialog.Manager)
	require.True(t, m.dialogMgr.Open())
}

// sizeDialogs gives the dialog manager (and every dialog opened afterwards)
// real dimensions so buried dialogs render meaningful views.
func sizeDialogs(t *testing.T, m *appModel) {
	t.Helper()
	updated, _ := m.dialogMgr.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.dialogMgr = updated.(dialog.Manager)
}

// runPlanFlow drives msg through the model like the Bubble Tea runtime
// would: produced commands run on the test goroutine and the typed results
// of asynchronous plan reads and mutations are dispatched back into Update.
// All other produced messages are returned in order.
func runPlanFlow(t *testing.T, m *appModel, msg tea.Msg) []tea.Msg {
	t.Helper()
	_, cmd := m.Update(msg)
	return drainPlanFlow(t, m, cmd)
}

// drainPlanFlow executes cmd, feeding every produced plan result message
// back into Update until the flow settles.
func drainPlanFlow(t *testing.T, m *appModel, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	queue := collectMsgs(cmd)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		switch next.(type) {
		case planStatusResultMsg, planDeleteResultMsg, planWriteResultMsg,
			planBrowserLoadedMsg, planRefreshedMsg, planDetailLoadedMsg,
			planEditReadyMsg, planExportResultMsg:
			_, cmd := m.Update(next)
			queue = append(queue, collectMsgs(cmd)...)
		default:
			out = append(out, next)
		}
	}
	return out
}

func firstOfType[T any](msgs []tea.Msg) (T, bool) {
	for _, msg := range msgs {
		if typed, ok := msg.(T); ok {
			return typed, true
		}
	}
	var zero T
	return zero, false
}

func countOfType[T any](msgs []tea.Msg) int {
	n := 0
	for _, msg := range msgs {
		if _, ok := msg.(T); ok {
			n++
		}
	}
	return n
}

func notificationTexts(msgs []tea.Msg) []string {
	var texts []string
	for _, msg := range msgs {
		if note, ok := msg.(notification.ShowMsg); ok {
			texts = append(texts, note.Text)
		}
	}
	return texts
}

func TestHandleShowPlanBrowser_OpensDialog(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	msgs := runPlanFlow(t, m, messages.ShowPlanBrowserMsg{})
	openMsg, ok := firstOfType[dialog.OpenDialogMsg](msgs)
	require.True(t, ok, "/plans must open the browser dialog")

	openMsg.Model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	assert.Contains(t, openMsg.Model.View(), "release")
}

func TestHandleSetPlanStatus_RefreshesAfterWrite(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	created := mustCreatePlan(t, svc, "release", "content")
	require.Equal(t, 1, *created.Version)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{created}})

	msgs := runPlanFlow(t, m, messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "done",
		ExpectedVersion: 1,
	})

	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "done")

	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a successful write must refresh the browser data")
	require.Len(t, dataMsg.Result.Plans, 1)
	assert.Equal(t, "done", dataMsg.Result.Plans[0].Status)
	assert.Equal(t, 2, *dataMsg.Result.Plans[0].Version)

	stored, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, "done", stored.Status)
}

func TestHandleSetPlanStatus_StaleConflictPreservesNewerData(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	// Someone else moved the plan to v2 with a newer status.
	v1 := 1
	_, err := svc.SetStatus(t.Context(), plans.SetStatusRequest{
		Ref: plans.SharedRef("release"), Status: "newer", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	msgs := runPlanFlow(t, m, messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "stale-write",
		ExpectedVersion: 1,
	})

	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	assert.Contains(t, texts[0], "v2", "the conflict notification must name the current version")

	// The newer content stays intact and is re-read into the dialogs.
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a conflict must refresh so the newer version is visible")
	assert.Equal(t, "newer", dataMsg.Result.Plans[0].Status)

	stored, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, "newer", stored.Status, "the stale write must not clobber the newer status")
	assert.Equal(t, 2, *stored.Version)
}

func TestHandleDeletePlan_Semantics(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	// Stale guard: delete is refused and the plan survives.
	v1 := 1
	_, err := svc.SetStatus(t.Context(), plans.SetStatusRequest{
		Ref: plans.SharedRef("release"), Status: "moved-on", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	msgs := runPlanFlow(t, m, messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	_, err = svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err, "a stale delete must leave the plan intact")

	// Correct guard: delete succeeds and the refresh drops the row.
	msgs = runPlanFlow(t, m, messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 2})
	texts = notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "Deleted")

	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok)
	assert.Empty(t, dataMsg.Result.Plans)

	_, err = svc.Get(t.Context(), plans.SharedRef("release"))
	require.Error(t, err)
}

func TestHandleExportPlan_RefusesOverwrite(t *testing.T) {
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "the content")

	workDir := t.TempDir()
	t.Chdir(workDir)

	msgs := runPlanFlow(t, m, messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	note, ok := firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeSuccess, note.Type)

	path := filepath.Join(workDir, "release.md")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "the content", string(data))

	// Second export to the same default path must refuse to overwrite.
	require.NoError(t, os.WriteFile(path, []byte("precious local edits"), 0o600))
	msgs = runPlanFlow(t, m, messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	note, ok = firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "already exists")

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "precious local edits", string(data), "an existing file must never be overwritten")
}

func TestHandlePlanEditorClosed_CreatesPlan(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("# fresh plan"), 0o600))

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "Created")

	stored, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.NoError(t, err)
	assert.Equal(t, "# fresh plan", stored.Content)

	_, err = os.Stat(draft)
	assert.True(t, os.IsNotExist(err), "the draft is removed after a successful write")
}

func TestHandlePlanEditorClosed_EmptyDraftAborts(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("  \n \n"), 0o600))

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not created")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "an empty draft must not create a plan")

	_, err = os.Stat(draft)
	assert.True(t, os.IsNotExist(err), "an empty draft is removed, not kept")
}

// TestHandlePlanEditorClosed_OversizedDraftRefusedBounded proves the draft
// read is bounded: a draft past the plan content cap is refused with an
// actionable notification — detected from the descriptor size, never by
// slurping the file whole — and the draft is preserved for the user to trim.
func TestHandlePlanEditorClosed_OversizedDraftRefusedBounded(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, make([]byte, plan.MaxPlanContentSize+1), 0o600))

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "maximum plan size")
	assert.Contains(t, strings.Join(texts, " "), draft, "the notification must point at the kept draft")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "an oversized draft must not create a plan")
	_, err = os.Stat(draft)
	require.NoError(t, err, "the draft must be kept when it is refused")
}

// TestHandlePlanEditorClosed_NonRegularDraftRejected proves a draft path
// that is not a regular file (here: a directory) is refused on the opened
// descriptor instead of read, with the path preserved.
func TestHandlePlanEditorClosed_NonRegularDraftRejected(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)

	draftDir := t.TempDir()
	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draftDir})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not a regular file")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "nothing may be written from an unreadable draft")
	_, err = os.Stat(draftDir)
	require.NoError(t, err, "the draft path must be preserved on a read failure")
}

func TestHandlePlanEditorClosed_ConflictKeepsDraftAndNewerContent(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "v1 content")

	// The plan moved to v2 while the user was editing v1.
	v1 := 1
	_, err := svc.Update(t.Context(), plans.UpdateRequest{
		Ref: plans.SharedRef("release"), Content: "newer content", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("stale edited content"), 0o600))

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("release"), expectedVersion: 1, path: draft})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	assert.Contains(t, texts[0], "v2")
	assert.Contains(t, texts[0], draft, "the notification must point at the kept draft")

	_, err = os.Stat(draft)
	require.NoError(t, err, "the draft must be kept on conflict")

	stored, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, "newer content", stored.Content, "the newer content must stay intact")
}

func TestHandleEditPlan_VersionDriftRefreshesInsteadOfEditing(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "v1 content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})
	v1 := 1
	_, err := svc.Update(t.Context(), plans.UpdateRequest{
		Ref: plans.SharedRef("release"), Content: "v2 content", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	_, cmd := m.Update(messages.EditPlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	msgs := drainPlanFlow(t, m, cmd)
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "v2")

	_, refreshed := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	assert.True(t, refreshed, "version drift must refresh the data on screen")
}

func TestPlanChangedEvent_RefreshesOpenPlanDialogs(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	msgs := runPlanFlow(t, m, runtime.PlanChanged("shared", "release", plan.ChangeActionWrite, 1, "root"))
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "an open plan browser must live-refresh on shared plan mutations")
	require.Len(t, dataMsg.Result.Plans, 1)
	assert.Equal(t, "release", dataMsg.Result.Plans[0].Name)
}

func TestPlanChangedEvent_BackgroundSessionStillRefreshes(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	sv := supervisor.New(nil)
	activeID := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	backgroundID := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	m.supervisor = sv
	require.Equal(t, activeID, sv.ActiveID())
	m.chatPages[backgroundID] = &mockChatPage{}

	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	msgs := runPlanFlow(t, m, messages.RoutedMsg{
		SessionID: backgroundID,
		Inner:     runtime.PlanChanged("shared", "release", plan.ChangeActionStatus, 2, "bg-agent"),
	})
	_, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	assert.True(t, ok, "shared plans are scope-global: background mutations refresh the open browser")
}

func TestLeanModeDropsPlanBrowser(t *testing.T) {
	t.Parallel()
	// Lean mode short-circuits before any handler runs, so no plans service
	// or application is needed.
	m, _ := newTestModel(t)
	m.leanMode = true

	_, cmd := m.Update(messages.ShowPlanBrowserMsg{})
	assert.Nil(t, cmd, "lean mode has no overlays; /plans is dropped like /settings")
}

// --- Asynchronous mutations under a wedged lock --------------------------------

// blockingPlansService simulates a wedged cross-process plans lock: every
// mutation blocks until its context expires and reports the deadline as a
// typed storage error, exactly like the real storage's lock acquisition.
// Reads delegate to the embedded real service.
type blockingPlansService struct {
	plans.Service

	mutationsStarted atomic.Int32
}

func (s *blockingPlansService) block(ctx context.Context, op string) error {
	s.mutationsStarted.Add(1)
	<-ctx.Done()
	return &plans.StorageError{Scope: plans.ScopeShared, Op: op, Err: ctx.Err()}
}

func (s *blockingPlansService) Create(ctx context.Context, _ plans.CreateRequest) (plans.Plan, error) {
	return plans.Plan{}, s.block(ctx, "create")
}

func (s *blockingPlansService) Update(ctx context.Context, _ plans.UpdateRequest) (plans.Plan, error) {
	return plans.Plan{}, s.block(ctx, "update")
}

func (s *blockingPlansService) SetStatus(ctx context.Context, _ plans.SetStatusRequest) (plans.Plan, error) {
	return plans.Plan{}, s.block(ctx, "set_status")
}

func (s *blockingPlansService) Delete(ctx context.Context, _ plans.DeleteRequest) error {
	return s.block(ctx, "delete")
}

// TestHandleSetPlanStatus_WedgedLockTimesOutAsynchronously proves the
// mutation never runs inside Update: the event loop stays responsive while
// the write is pending, and a wedged lock surfaces as a bounded, actionable
// timeout notification instead of a freeze.
func TestHandleSetPlanStatus_WedgedLockTimesOutAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})
	blocking := &blockingPlansService{Service: svc}
	WithPlansService(blocking)(m)
	m.planMutationTimeout = 50 * time.Millisecond

	_, cmd := m.Update(messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "done",
		ExpectedVersion: 1,
	})
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.mutationsStarted.Load(), "Update must defer the mutation to a command, never run it inline")

	// The TUI stays responsive while the mutation is pending: a read-driven
	// refresh is processed before the mutation command has even run.
	_, ok := firstOfType[dialog.PlanBrowserDataMsg](runPlanFlow(t, m, messages.RefreshPlansMsg{}))
	assert.True(t, ok, "reads keep working while a mutation is pending")

	// Running the command blocks on the wedged lock until the bounded
	// timeout fires, then reports back as a typed result message.
	start := time.Now()
	result := cmd()
	elapsed := time.Since(start)
	statusResult, ok := result.(planStatusResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	assert.Equal(t, int32(1), blocking.mutationsStarted.Load())
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "the command waits for the bounded timeout")
	assert.Less(t, elapsed, time.Second, "the bounded timeout must fire, not the 5s default or never")
	require.Error(t, statusResult.err)
	require.ErrorIs(t, statusResult.err, context.DeadlineExceeded)

	// Dispatching the result yields the actionable notification.
	_, notifyCmd := m.Update(result)
	note, ok := firstOfType[notification.ShowMsg](collectMsgs(notifyCmd))
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "timed out")
	assert.Contains(t, note.Text, "locked")
}

func TestHandleDeletePlan_WedgedLockTimesOutAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := &blockingPlansService{Service: svc}
	WithPlansService(blocking)(m)
	m.planMutationTimeout = 50 * time.Millisecond

	_, cmd := m.Update(messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.mutationsStarted.Load())

	result := cmd()
	deleteResult, ok := result.(planDeleteResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	require.ErrorIs(t, deleteResult.err, context.DeadlineExceeded)

	_, notifyCmd := m.Update(result)
	note, ok := firstOfType[notification.ShowMsg](collectMsgs(notifyCmd))
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "timed out")

	// The plan survives the timed-out delete.
	_, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
}

// TestHandlePlanEditorClosed_TimeoutKeepsDraft proves a timed-out
// editor-driven write behaves like any other failed write: the draft file
// survives and the notification points at it.
func TestHandlePlanEditorClosed_TimeoutKeepsDraft(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	blocking := &blockingPlansService{Service: svc}
	WithPlansService(blocking)(m)
	m.planMutationTimeout = 50 * time.Millisecond

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("drafted content"), 0o600))

	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.mutationsStarted.Load(), "Update must defer the write to a command")

	result := cmd()
	writeResult, ok := result.(planWriteResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	require.Error(t, writeResult.err)
	require.ErrorIs(t, writeResult.err, context.DeadlineExceeded)

	_, notifyCmd := m.Update(result)
	texts := notificationTexts(collectMsgs(notifyCmd))
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "timed out")
	assert.Contains(t, strings.Join(texts, " "), draft, "the notification must point at the kept draft")

	_, err := os.Stat(draft)
	require.NoError(t, err, "the draft must be kept when the write times out")
}

// --- Live refresh reaches buried plan dialogs -----------------------------------

// TestPlanChangedEvent_RefreshesBuriedPlanDialogs stacks a real non-plan
// dialog on top of the plan browser and detail, fires a PlanChanged event,
// and proves both buried dialogs receive the updated data once the produced
// commands are applied.
func TestPlanChangedEvent_RefreshesBuriedPlanDialogs(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "v1 content")

	sizeDialogs(t, m)
	browser := dialog.NewPlanBrowserDialog(plans.ListResult{Plans: []plans.Plan{}})
	openDialog(t, m, browser)
	p, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	detail := dialog.NewPlanDetailDialog(p)
	openDialog(t, m, detail)
	openDialog(t, m, dialog.NewHelpDialog(nil))

	// An agent moves the plan to v2 while the plan dialogs are buried.
	v1 := 1
	_, err = svc.Update(t.Context(), plans.UpdateRequest{
		Ref: plans.SharedRef("release"), Content: "v2 content", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	msgs := runPlanFlow(t, m, runtime.PlanChanged("shared", "release", plan.ChangeActionWrite, 2, "root"))

	listMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a buried browser must still trigger a list refresh")
	require.Len(t, listMsg.Result.Plans, 1)

	detailMsg, ok := firstOfType[dialog.PlanDetailDataMsg](msgs)
	require.True(t, ok, "a buried detail must be refreshed by its exact ref")
	assert.Equal(t, "release", detailMsg.Plan.Name)
	assert.Equal(t, "v2 content", detailMsg.Plan.Content)

	// Apply the broadcasts and prove the buried instances updated.
	_, _ = m.Update(listMsg)
	_, _ = m.Update(detailMsg)
	assert.Contains(t, browser.View(), "release", "the buried browser must render the refreshed rows")
	assert.Contains(t, detail.View(), "v2 content", "the buried detail must render the refreshed plan")
}

// failingGetPlansService wraps a real service and fails Get for one exact
// ref with a configured error, so tests drive detail-refresh failures
// deterministically instead of through fragile filesystem state.
type failingGetPlansService struct {
	plans.Service

	failRef plans.Ref
	err     error
}

func (s *failingGetPlansService) Get(ctx context.Context, ref plans.Ref) (plans.Plan, error) {
	if s.err != nil && ref == s.failRef {
		return plans.Plan{}, s.err
	}
	return s.Service.Get(ctx, ref)
}

// TestPlanRefresh_BuriedDetailSuppressesErrorsUntilSurfaced proves a detail
// dialog whose plan can no longer be read does not spam notifications while
// it is buried under another dialog, whatever the error: refreshes skip it
// silently (a buried detail cannot be closed without popping the wrong
// dialog), and exactly one notification — plus a close only for a plan that
// disappeared — happens once it surfaces and the next refresh runs.
func TestPlanRefresh_BuriedDetailSuppressesErrorsUntilSurfaced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		getErr    error
		wantClose bool
		wantText  string
	}{
		{
			name:      "not found closes and notifies",
			getErr:    &plans.NotFoundError{Scope: plans.ScopeShared, Name: "release"},
			wantClose: true,
			wantText:  "No shared plan",
		},
		{
			name:     "corrupt notifies without closing",
			getErr:   &plans.CorruptError{Scope: plans.ScopeShared, Name: "release", Err: errors.New("invalid frontmatter")},
			wantText: "corrupt",
		},
		{
			name:     "storage failure notifies without closing",
			getErr:   &plans.StorageError{Scope: plans.ScopeShared, Op: "get", Err: errors.New("device gone")},
			wantText: "storage failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, svc := newPlansTestModel(t)
			p := mustCreatePlan(t, svc, "release", "content")
			WithPlansService(&failingGetPlansService{
				Service: svc,
				failRef: plans.SharedRef("release"),
				err:     tt.getErr,
			})(m)

			sizeDialogs(t, m)
			openDialog(t, m, dialog.NewPlanDetailDialog(p))
			openDialog(t, m, dialog.NewHelpDialog(nil))

			// Repeated refreshes neither notify nor try to close the buried
			// detail.
			for range 2 {
				msgs := runPlanFlow(t, m, messages.RefreshPlansMsg{})
				assert.Empty(t, notificationTexts(msgs), "a buried failing detail must not notify on refresh")
				_, closed := firstOfType[dialog.ClosePlanDetailMsg](msgs)
				assert.False(t, closed, "a buried detail must never be closed")
			}

			// The covering dialog closes, the failing detail surfaces: the
			// next refresh notifies exactly once, closing only a vanished
			// plan.
			updated, _ := m.dialogMgr.Update(dialog.CloseDialogMsg{})
			m.dialogMgr = updated.(dialog.Manager)

			msgs := runPlanFlow(t, m, messages.RefreshPlansMsg{})
			closeMsg, closed := firstOfType[dialog.ClosePlanDetailMsg](msgs)
			assert.Equal(t, tt.wantClose, closed, "only a vanished plan closes the surfaced detail")
			if closed {
				assert.Equal(t, plans.SharedRef("release"), closeMsg.Ref, "the close must target the vanished detail")
			}

			texts := notificationTexts(msgs)
			require.Len(t, texts, 1, "the surfaced failing detail notifies exactly once")
			assert.Contains(t, texts[0], tt.wantText)
		})
	}
}

// TestPlanRefresh_DuplicateVanishedDetailClosesOnlyDetail reproduces the
// adversarial double-close: two refreshes are requested while a detail's
// plan has vanished, and both results are dispatched before either close is
// applied, so each one still sees the detail on top and emits a targeted
// close. Applying both must pop only the detail — never the browser
// underneath. Notifications may repeat; the dialog stack must stay correct.
func TestPlanRefresh_DuplicateVanishedDetailClosesOnlyDetail(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	p := mustCreatePlan(t, svc, "release", "content")
	WithPlansService(&failingGetPlansService{
		Service: svc,
		failRef: plans.SharedRef("release"),
		err:     &plans.NotFoundError{Scope: plans.ScopeShared, Name: "release"},
	})(m)

	sizeDialogs(t, m)
	browser := dialog.NewPlanBrowserDialog(plans.ListResult{Plans: []plans.Plan{p}})
	openDialog(t, m, browser)
	openDialog(t, m, dialog.NewPlanDetailDialog(p))

	// Two refreshes are requested back-to-back; the second coalesces behind
	// the in-flight first one.
	_, cmd1 := m.Update(messages.RefreshPlansMsg{})
	require.NotNil(t, cmd1)
	_, cmd2 := m.Update(messages.RefreshPlansMsg{})
	assert.Nil(t, cmd2, "a refresh requested while one is in flight is coalesced")

	// Handle the first result. It emits the first targeted close (the close
	// is NOT applied yet) and replays the coalesced refresh, whose read runs
	// here and yields the second result.
	result1, ok := firstOfType[planRefreshedMsg](collectMsgs(cmd1))
	require.True(t, ok)
	_, cmd := m.Update(result1)
	out1 := collectMsgs(cmd)
	assert.Equal(t, 1, countOfType[dialog.ClosePlanDetailMsg](out1))
	result2, ok := firstOfType[planRefreshedMsg](out1)
	require.True(t, ok, "the coalesced refresh must be replayed")

	// Handle the second result before the first close was applied: the
	// detail is still on top, so a second close for the same ref is emitted.
	_, cmd = m.Update(result2)
	out2 := collectMsgs(cmd)
	assert.Equal(t, 1, countOfType[dialog.ClosePlanDetailMsg](out2))

	// Apply everything — both targeted closes included — like the runtime
	// would. Only the detail may close; the browser survives as top.
	for _, msg := range append(out1, out2...) {
		if _, ok := msg.(planRefreshedMsg); ok {
			continue // already handled above
		}
		_, _ = m.Update(msg)
	}
	require.True(t, m.dialogMgr.Open(), "the browser must survive both closes")
	assert.Same(t, browser, m.dialogMgr.TopDialog(), "exactly one pop: the detail closed, the browser is top")
}

// TestPlanRefresh_CoalescesInFlightRequests proves refresh requests arriving
// while a reload is in flight collapse into exactly one follow-up reload.
func TestPlanRefresh_CoalescesInFlightRequests(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	_, cmd1 := m.Update(messages.RefreshPlansMsg{})
	require.NotNil(t, cmd1)
	_, cmd2 := m.Update(messages.RefreshPlansMsg{})
	assert.Nil(t, cmd2, "the second request coalesces behind the in-flight reload")
	_, cmd3 := m.Update(messages.RefreshPlansMsg{})
	assert.Nil(t, cmd3, "the third request folds into the same follow-up")

	msgs := drainPlanFlow(t, m, cmd1)
	assert.Equal(t, 2, countOfType[dialog.PlanBrowserDataMsg](msgs),
		"the in-flight reload plus exactly one coalesced follow-up")

	// The pipeline is idle again: a new request launches immediately.
	_, cmd4 := m.Update(messages.RefreshPlansMsg{})
	assert.NotNil(t, cmd4)
}

// TestHandlePlanEditorClosed_EditorErrorKeepsDraft proves an editor failure
// (e.g. a non-zero exit after the user saved) never deletes the draft: the
// content survives on disk and the notifications name both the error and
// the kept path.
func TestHandlePlanEditorClosed_EditorErrorKeepsDraft(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("# saved before the editor failed"), 0o600))

	msgs := runPlanFlow(t, m, planEditorClosedMsg{
		ref:    plans.SharedRef("fresh"),
		create: true,
		path:   draft,
		err:    errors.New("exit status 1"),
	})
	texts := notificationTexts(msgs)
	require.Len(t, texts, 2)
	assert.Contains(t, texts[0], "Editor error")
	assert.Contains(t, texts[0], "exit status 1")
	assert.Contains(t, texts[1], draft, "the notification must point at the kept draft")

	data, err := os.ReadFile(draft)
	require.NoError(t, err, "the draft must survive an editor error")
	assert.Equal(t, "# saved before the editor failed", string(data))

	_, err = svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "nothing may be written when the editor failed")
}

// --- Reads never run inline in Update -------------------------------------

// blockingReadPlansService blocks every read until release is closed or the
// read's context expires, counting the reads that started. It simulates
// slow storage: if Update ran a read inline it would block forever on the
// unreleased channel and fail the test by timeout, the same proof style as
// the FIFO draft-read test. An expired context reports the deadline as a
// typed storage error, exactly like real storage that respects
// cancellation, so bounded-read tests never leak a blocked goroutine.
type blockingReadPlansService struct {
	plans.Service

	readsStarted atomic.Int32
	release      chan struct{}
}

func newBlockingReadPlansService(svc plans.Service) *blockingReadPlansService {
	return &blockingReadPlansService{Service: svc, release: make(chan struct{})}
}

func (s *blockingReadPlansService) await(ctx context.Context, op string) error {
	s.readsStarted.Add(1)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return &plans.StorageError{Scope: plans.ScopeShared, Op: op, Err: ctx.Err()}
	}
}

func (s *blockingReadPlansService) List(ctx context.Context) (plans.ListResult, error) {
	if err := s.await(ctx, "list"); err != nil {
		return plans.ListResult{}, err
	}
	return s.Service.List(ctx)
}

func (s *blockingReadPlansService) Get(ctx context.Context, ref plans.Ref) (plans.Plan, error) {
	if err := s.await(ctx, "get"); err != nil {
		return plans.Plan{}, err
	}
	return s.Service.Get(ctx, ref)
}

func (s *blockingReadPlansService) Export(ctx context.Context, req plans.ExportRequest) (plans.ExportResult, error) {
	if err := s.await(ctx, "export"); err != nil {
		return plans.ExportResult{}, err
	}
	return s.Service.Export(ctx, req)
}

// requireDeferredRead asserts Update produced a command without starting any
// service read inline, then releases the blocked reads, runs the command,
// and returns every message the settled flow produced.
func requireDeferredRead(t *testing.T, m *appModel, blocking *blockingReadPlansService, msg tea.Msg) []tea.Msg {
	t.Helper()
	_, cmd := m.Update(msg)
	require.NotNil(t, cmd, "Update must defer the read to a command")
	assert.Zero(t, blocking.readsStarted.Load(), "Update must never read the plan service inline")

	close(blocking.release)
	return drainPlanFlow(t, m, cmd)
}

func TestHandleShowPlanBrowser_ReadsAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	msgs := requireDeferredRead(t, m, blocking, messages.ShowPlanBrowserMsg{})
	_, ok := firstOfType[dialog.OpenDialogMsg](msgs)
	assert.True(t, ok, "the browser opens once the deferred read completes")
}

func TestHandleRefreshPlans_ReadsAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	p := mustCreatePlan(t, svc, "release", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	sizeDialogs(t, m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{p}})
	openDialog(t, m, dialog.NewPlanDetailDialog(p))

	msgs := requireDeferredRead(t, m, blocking, messages.RefreshPlansMsg{})
	_, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	assert.True(t, ok, "the browser rows arrive once the deferred reads complete")
	detailMsg, ok := firstOfType[dialog.PlanDetailDataMsg](msgs)
	require.True(t, ok, "the open detail is re-fetched off the event loop too")
	assert.Equal(t, "content", detailMsg.Plan.Content)
	assert.Equal(t, int32(2), blocking.readsStarted.Load(), "one List plus one Get for the open detail")
}

func TestHandleOpenPlanDetail_ReadsAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	// The detail is requested from an open browser; its result is dropped
	// once no plan dialog is open anymore.
	sizeDialogs(t, m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	msgs := requireDeferredRead(t, m, blocking, messages.OpenPlanDetailMsg{Ref: plans.SharedRef("release")})
	openMsg, ok := firstOfType[dialog.OpenDialogMsg](msgs)
	require.True(t, ok, "the detail opens once the deferred read completes")
	viewer, ok := openMsg.Model.(dialog.PlanDetailViewer)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), viewer.PlanRef())
}

func TestHandleExportPlan_ExportsAsynchronously(t *testing.T) {
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "the content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	workDir := t.TempDir()
	t.Chdir(workDir)
	path := filepath.Join(workDir, "release.md")

	_, cmd := m.Update(messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	require.NotNil(t, cmd, "Update must defer the export to a command")
	assert.Zero(t, blocking.readsStarted.Load(), "Update must never read the plan service inline")
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "nothing may be written before the command runs")

	close(blocking.release)
	msgs := drainPlanFlow(t, m, cmd)
	note, ok := firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeSuccess, note.Type)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "the content", string(data))
}

func TestPlanChangedEvent_RefreshReadsAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	sizeDialogs(t, m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	msgs := requireDeferredRead(t, m, blocking, runtime.PlanChanged("shared", "release", plan.ChangeActionWrite, 1, "root"))
	_, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	assert.True(t, ok, "the event-driven refresh lands once the deferred read completes")
}

// TestHandleEditPlan_PreparesAsynchronously proves the whole edit
// preparation — the Get and the draft-file write seeded from it — runs in a
// command, never inline in Update: with reads blocked, Update returns
// immediately and no read has started (and since the draft is seeded from
// the read plan, no draft was written either).
func TestHandleEditPlan_PreparesAsynchronously(t *testing.T) {
	t.Parallel()

	t.Run("matching version drafts and launches the editor", func(t *testing.T) {
		t.Parallel()
		m, svc := newPlansTestModel(t)
		p := mustCreatePlan(t, svc, "release", "v1 content")
		blocking := newBlockingReadPlansService(svc)
		WithPlansService(blocking)(m)

		sizeDialogs(t, m)
		openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{p}})

		_, cmd := m.Update(messages.EditPlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
		require.NotNil(t, cmd, "Update must defer the edit preparation to a command")
		assert.Zero(t, blocking.readsStarted.Load(), "Update must never read the plan service inline")

		close(blocking.release)
		result := cmd()
		ready, ok := result.(planEditReadyMsg)
		require.True(t, ok, "the command must yield a typed result, got %T", result)
		require.NoError(t, ready.err)
		require.NoError(t, ready.draftErr)
		require.NotEmpty(t, ready.draftPath)
		t.Cleanup(func() { _ = os.Remove(ready.draftPath) })

		data, err := os.ReadFile(ready.draftPath)
		require.NoError(t, err)
		assert.Equal(t, "v1 content", string(data), "the draft must be seeded with the current content")

		// Dispatching the result launches the editor — an exec command, not a
		// notification — and the draft survives for the editor to open.
		_, editorCmd := m.Update(result)
		require.NotNil(t, editorCmd, "the prepared edit must launch the editor")
		assert.Empty(t, notificationTexts(collectMsgs(editorCmd)), "the launch must not be a notification")
		_, err = os.Stat(ready.draftPath)
		require.NoError(t, err, "the draft must be kept for the editor")
	})

	t.Run("version drift refreshes instead of editing", func(t *testing.T) {
		t.Parallel()
		m, svc := newPlansTestModel(t)
		mustCreatePlan(t, svc, "release", "v1 content")
		openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})
		v1 := 1
		_, err := svc.Update(t.Context(), plans.UpdateRequest{
			Ref: plans.SharedRef("release"), Content: "v2 content", ExpectedVersion: &v1,
		})
		require.NoError(t, err)
		blocking := newBlockingReadPlansService(svc)
		WithPlansService(blocking)(m)

		_, cmd := m.Update(messages.EditPlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
		require.NotNil(t, cmd)
		assert.Zero(t, blocking.readsStarted.Load(), "Update must never read the plan service inline")

		close(blocking.release)
		result := cmd()
		ready, ok := result.(planEditReadyMsg)
		require.True(t, ok, "the command must yield a typed result, got %T", result)
		require.NoError(t, ready.err)
		assert.Equal(t, 2, ready.currentVersion)
		assert.Empty(t, ready.draftPath, "no draft may be created for a drifted base")

		msgs := drainPlanFlow(t, m, func() tea.Msg { return result })
		texts := notificationTexts(msgs)
		require.NotEmpty(t, texts)
		assert.Contains(t, texts[0], "v2")
		_, refreshed := firstOfType[dialog.PlanBrowserDataMsg](msgs)
		assert.True(t, refreshed, "version drift must refresh the data on screen")
	})
}

// --- Concurrent duplicate exports -----------------------------------------

// TestHandleExportPlan_DuplicateInFlightRefused proves two exports to the
// same destination cannot race the no-overwrite pre-check against the
// write: the second request — arriving before the first command has even
// run — is refused with a notification and never starts an export, exactly
// one write happens, and once it completed the pre-check refuses the
// existing file.
func TestHandleExportPlan_DuplicateInFlightRefused(t *testing.T) {
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "the content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	workDir := t.TempDir()
	t.Chdir(workDir)
	path := filepath.Join(workDir, "release.md")

	// Two exports of the same plan before the first command executes.
	_, cmd1 := m.Update(messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	require.NotNil(t, cmd1)
	_, cmd2 := m.Update(messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	require.NotNil(t, cmd2)

	// The duplicate yields only the refusal notification — no export command.
	dupMsgs := collectMsgs(cmd2)
	assert.Zero(t, countOfType[planExportResultMsg](dupMsgs), "the duplicate must not start an export")
	note, ok := firstOfType[notification.ShowMsg](dupMsgs)
	require.True(t, ok, "the duplicate must be refused with a notification")
	assert.Equal(t, notification.TypeInfo, note.Type)
	assert.Contains(t, note.Text, path)
	assert.Contains(t, note.Text, "already running")
	assert.Zero(t, blocking.readsStarted.Load(), "no export may have reached the service yet")

	// The first export completes: exactly one write happened.
	close(blocking.release)
	msgs := drainPlanFlow(t, m, cmd1)
	note, ok = firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeSuccess, note.Type)
	assert.Equal(t, int32(1), blocking.readsStarted.Load(), "exactly one export reaches the service")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "the content", string(data))
	assert.Empty(t, m.planExportsInFlight, "a completed export must clear its in-flight key")

	// The destination is registrable again — and the no-overwrite pre-check
	// now sees the exported file and refuses.
	msgs = runPlanFlow(t, m, messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	note, ok = firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "already exists")
	assert.Empty(t, m.planExportsInFlight, "a refused export must clear its in-flight key too")
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "the content", string(data), "the existing file must never be overwritten")
}

// --- Duplicate async browser/detail opens ----------------------------------

// TestHandleShowPlanBrowser_DuplicateRequestsDropped proves two /plans
// requests racing one in-flight listing read start exactly one List and
// stack exactly one browser.
func TestHandleShowPlanBrowser_DuplicateRequestsDropped(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	_, cmd1 := m.Update(messages.ShowPlanBrowserMsg{})
	require.NotNil(t, cmd1)
	_, cmd2 := m.Update(messages.ShowPlanBrowserMsg{})
	assert.Nil(t, cmd2, "a second /plans while the listing read is in flight must not start another read")

	close(blocking.release)
	msgs := drainPlanFlow(t, m, cmd1)
	assert.Equal(t, int32(1), blocking.readsStarted.Load(), "exactly one List reaches the service")
	assert.Equal(t, 1, countOfType[dialog.OpenDialogMsg](msgs), "exactly one browser opens")
	assert.False(t, m.planBrowserLoadInFlight, "the guard must clear when the result lands")
}

// TestHandleShowPlanBrowser_RefusedWhenBrowserAlreadyOpen proves /plans with
// a browser already on the dialog stack neither reads nor stacks a second
// browser.
func TestHandleShowPlanBrowser_RefusedWhenBrowserAlreadyOpen(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	_, cmd := m.Update(messages.ShowPlanBrowserMsg{})
	assert.Nil(t, cmd, "a /plans with the browser already open must not start a read")
	assert.Zero(t, blocking.readsStarted.Load())
}

// TestHandleOpenPlanDetail_DuplicateRequestsDropped proves two open requests
// for the same plan racing one in-flight read start exactly one Get and
// stack exactly one detail dialog.
func TestHandleOpenPlanDetail_DuplicateRequestsDropped(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	sizeDialogs(t, m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	_, cmd1 := m.Update(messages.OpenPlanDetailMsg{Ref: plans.SharedRef("release")})
	require.NotNil(t, cmd1)
	_, cmd2 := m.Update(messages.OpenPlanDetailMsg{Ref: plans.SharedRef("release")})
	assert.Nil(t, cmd2, "a repeated open for the same plan while its read is in flight must not start another Get")

	close(blocking.release)
	msgs := drainPlanFlow(t, m, cmd1)
	assert.Equal(t, int32(1), blocking.readsStarted.Load(), "exactly one Get reaches the service")
	assert.Equal(t, 1, countOfType[dialog.OpenDialogMsg](msgs), "exactly one detail opens")
	assert.Empty(t, m.planDetailLoadsInFlight, "the guard must clear when the result lands")
}

// TestHandleOpenPlanDetail_RefusedWhenDetailAlreadyOpen proves an open for a
// plan whose detail is already on the stack neither reads nor stacks a
// duplicate, while a different plan still loads.
func TestHandleOpenPlanDetail_RefusedWhenDetailAlreadyOpen(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	p := mustCreatePlan(t, svc, "release", "content")
	mustCreatePlan(t, svc, "other", "content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)

	sizeDialogs(t, m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{p}})
	openDialog(t, m, dialog.NewPlanDetailDialog(p))

	_, cmd := m.Update(messages.OpenPlanDetailMsg{Ref: plans.SharedRef("release")})
	assert.Nil(t, cmd, "an open for an already-shown detail must not start a read")
	assert.Zero(t, blocking.readsStarted.Load())

	_, cmd = m.Update(messages.OpenPlanDetailMsg{Ref: plans.SharedRef("other")})
	assert.NotNil(t, cmd, "a different plan's detail may still load")
}

// TestHandlePlanDetailLoaded_DuplicateOpenRefused proves a completed read
// for a plan whose detail appeared in the meantime does not stack a second
// copy.
func TestHandlePlanDetailLoaded_DuplicateOpenRefused(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	p := mustCreatePlan(t, svc, "release", "content")

	sizeDialogs(t, m)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{p}})
	openDialog(t, m, dialog.NewPlanDetailDialog(p))

	_, cmd := m.Update(planDetailLoadedMsg{ref: plans.SharedRef("release"), plan: p})
	assert.Nil(t, cmd, "a result for an already-open detail must not stack a duplicate")
}

// --- Bounded reads under wedged storage ------------------------------------

// TestPlanReads_WedgedStorageTimesOut proves every plan read command is
// bounded: against storage that never answers, Update stays responsive (no
// read ever runs inline), the command returns a typed deadline result
// within the configured read timeout instead of hanging forever, and
// dispatching the result yields the actionable timeout notification and
// clears any in-flight bookkeeping.
//
// Not parallel: the export case pins the working directory via t.Chdir.
func TestPlanReads_WedgedStorageTimesOut(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.Msg
		// openBrowser puts a plan dialog on the stack for results that are
		// dropped without one.
		openBrowser bool
		chdir       bool
		resultErr   func(t *testing.T, result tea.Msg) error
		after       func(t *testing.T, m *appModel)
	}{
		{
			name: "show browser",
			msg:  messages.ShowPlanBrowserMsg{},
			resultErr: func(t *testing.T, result tea.Msg) error {
				t.Helper()
				msg, ok := result.(planBrowserLoadedMsg)
				require.True(t, ok, "got %T", result)
				return msg.err
			},
			after: func(t *testing.T, m *appModel) {
				t.Helper()
				assert.False(t, m.planBrowserLoadInFlight, "a timed-out open must clear the browser-load guard")
				_, cmd := m.Update(messages.ShowPlanBrowserMsg{})
				assert.NotNil(t, cmd, "/plans must relaunch after a timeout")
			},
		},
		{
			name:        "open detail",
			msg:         messages.OpenPlanDetailMsg{Ref: plans.SharedRef("release")},
			openBrowser: true,
			resultErr: func(t *testing.T, result tea.Msg) error {
				t.Helper()
				msg, ok := result.(planDetailLoadedMsg)
				require.True(t, ok, "got %T", result)
				return msg.err
			},
			after: func(t *testing.T, m *appModel) {
				t.Helper()
				assert.Empty(t, m.planDetailLoadsInFlight, "a timed-out open must clear the detail-load guard")
				_, cmd := m.Update(messages.OpenPlanDetailMsg{Ref: plans.SharedRef("release")})
				assert.NotNil(t, cmd, "the detail open must relaunch after a timeout")
			},
		},
		{
			name:        "refresh",
			msg:         messages.RefreshPlansMsg{},
			openBrowser: true,
			resultErr: func(t *testing.T, result tea.Msg) error {
				t.Helper()
				msg, ok := result.(planRefreshedMsg)
				require.True(t, ok, "got %T", result)
				return msg.listErr
			},
			after: func(t *testing.T, m *appModel) {
				t.Helper()
				assert.False(t, m.planRefreshInFlight, "a timed-out reload must clear the in-flight flag")
				_, cmd := m.Update(messages.RefreshPlansMsg{})
				assert.NotNil(t, cmd, "the refresh pipeline must accept new requests after a timeout")
			},
		},
		{
			name:  "export",
			msg:   messages.ExportPlanMsg{Ref: plans.SharedRef("release")},
			chdir: true,
			resultErr: func(t *testing.T, result tea.Msg) error {
				t.Helper()
				msg, ok := result.(planExportResultMsg)
				require.True(t, ok, "got %T", result)
				return msg.err
			},
			after: func(t *testing.T, m *appModel) {
				t.Helper()
				assert.Empty(t, m.planExportsInFlight, "a timed-out export must clear its in-flight key")
			},
		},
		{
			name: "edit",
			msg:  messages.EditPlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1},
			resultErr: func(t *testing.T, result tea.Msg) error {
				t.Helper()
				msg, ok := result.(planEditReadyMsg)
				require.True(t, ok, "got %T", result)
				return msg.err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, svc := newPlansTestModel(t)
			mustCreatePlan(t, svc, "release", "content")
			blocking := newBlockingReadPlansService(svc) // never released: reads answer only via ctx
			WithPlansService(blocking)(m)
			m.planReadTimeout = 50 * time.Millisecond

			if tt.openBrowser {
				sizeDialogs(t, m)
				openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})
			}
			if tt.chdir {
				t.Chdir(t.TempDir())
			}

			_, cmd := m.Update(tt.msg)
			require.NotNil(t, cmd, "Update must defer the read to a command")
			assert.Zero(t, blocking.readsStarted.Load(), "Update must never read the plan service inline")

			start := time.Now()
			result := cmd()
			elapsed := time.Since(start)
			assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "the command waits for the bounded timeout")
			assert.Less(t, elapsed, time.Second, "the bounded timeout must fire, not the 10s default or never")

			err := tt.resultErr(t, result)
			require.Error(t, err)
			require.ErrorIs(t, err, context.DeadlineExceeded)

			_, notifyCmd := m.Update(result)
			texts := notificationTexts(collectMsgs(notifyCmd))
			require.NotEmpty(t, texts, "the deadline must surface as a notification")
			assert.Contains(t, texts[0], "timed out")
			assert.Contains(t, texts[0], "unavailable")

			if tt.after != nil {
				tt.after(t, m)
			}
		})
	}
}

// TestPlanRefresh_TimeoutClearsInFlightAndRunsQueued proves a timed-out
// reload cannot wedge the refresh pipeline: the deadline result clears the
// in-flight flag and the refresh that was coalesced behind it launches as
// the follow-up reload.
func TestPlanRefresh_TimeoutClearsInFlightAndRunsQueued(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})
	blocking := newBlockingReadPlansService(svc) // never released
	WithPlansService(blocking)(m)
	m.planReadTimeout = 50 * time.Millisecond

	_, cmd1 := m.Update(messages.RefreshPlansMsg{})
	require.NotNil(t, cmd1)
	_, cmd2 := m.Update(messages.RefreshPlansMsg{})
	assert.Nil(t, cmd2, "the second refresh coalesces behind the in-flight one")

	result := cmd1()
	refreshed, ok := result.(planRefreshedMsg)
	require.True(t, ok, "the reload must report back despite wedged storage, got %T", result)
	require.ErrorIs(t, refreshed.listErr, context.DeadlineExceeded)

	_, cmd := m.Update(result)
	assert.True(t, m.planRefreshInFlight, "the coalesced refresh must be relaunched as the follow-up")

	out := collectMsgs(cmd)
	texts := notificationTexts(out)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "timed out")

	followUp, ok := firstOfType[planRefreshedMsg](out)
	require.True(t, ok, "the queued refresh must run once the timed-out reload lands")
	require.ErrorIs(t, followUp.listErr, context.DeadlineExceeded)

	_, _ = m.Update(followUp)
	assert.False(t, m.planRefreshInFlight, "the pipeline must be idle again")
}

// --- Stale async results after the user moved on ---------------------------

// TestStalePlanResults_Dropped proves slow read results cannot disrupt a
// user who moved on: a detail result opens no dialog after /plans was
// closed, and a prepared edit launches no editor (and leaves no draft
// behind).
func TestStalePlanResults_Dropped(t *testing.T) {
	t.Parallel()

	t.Run("detail result after /plans closed opens no dialog", func(t *testing.T) {
		t.Parallel()
		m, svc := newPlansTestModel(t)
		p := mustCreatePlan(t, svc, "release", "content")

		// No plan dialog is open anymore when the read lands.
		_, cmd := m.Update(planDetailLoadedMsg{plan: p})
		assert.Nil(t, cmd, "a stale detail result must not open a dialog")
	})

	t.Run("edit result after /plans closed launches no editor", func(t *testing.T) {
		t.Parallel()
		m, _ := newPlansTestModel(t)
		draft := filepath.Join(t.TempDir(), "draft.md")
		require.NoError(t, os.WriteFile(draft, []byte("stored content"), 0o600))

		_, cmd := m.Update(planEditReadyMsg{
			ref:             plans.SharedRef("release"),
			expectedVersion: 1,
			currentVersion:  1,
			draftPath:       draft,
		})
		assert.Nil(t, cmd, "a stale edit must not take over the terminal with an editor")
		_, err := os.Stat(draft)
		assert.True(t, os.IsNotExist(err), "the unused draft holds no user edits and is removed")
	})
}

// TestPlanRefresh_ResultWithoutDialogsDropsSilently proves a reload result
// (here: a failed one) arriving after every plan dialog closed yields no
// orphan notification and leaves the pipeline clean, queued intent included.
func TestPlanRefresh_ResultWithoutDialogsDropsSilently(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	// A reload was in flight (with a queued follow-up) when the user closed
	// the last plan dialog.
	m.planRefreshInFlight = true
	m.planRefreshQueued = true
	m.planRefreshQueuedWarnings = true

	_, cmd := m.Update(planRefreshedMsg{listErr: errors.New("boom")})
	assert.Nil(t, cmd, "no notification and no follow-up may be produced")
	assert.False(t, m.planRefreshInFlight, "the pipeline must be idle")
	assert.False(t, m.planRefreshQueued, "the queued follow-up must be dropped")
	assert.False(t, m.planRefreshQueuedWarnings)

	// The pipeline accepts new requests immediately.
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})
	_, cmd = m.Update(messages.RefreshPlansMsg{})
	assert.NotNil(t, cmd)
}
