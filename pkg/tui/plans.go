package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/internal/editorname"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// defaultPlanMutationTimeout bounds every plan persistence call issued from
// the TUI (create, update, set-status, delete). Those writes take a
// cross-process file lock, so a lock wedged by another process must surface
// as an actionable timeout notification instead of freezing the event loop
// indefinitely.
const defaultPlanMutationTimeout = 5 * time.Second

// defaultPlanReadTimeout bounds every plan read issued from the TUI (list,
// get, export, and the read preparing an edit). Reads never take the plans
// lock, but storage can still wedge (network filesystem, dying disk); the
// deliberately generous bound turns a stuck read into an actionable
// notification instead of dialogs that never load, and guarantees the
// refresh pipeline's in-flight flag is always cleared.
const defaultPlanReadTimeout = 10 * time.Second

// WithPlansService injects the plans host service, replacing the lazily
// built default over plan.SharedStorage(). Intended for tests, which run
// against temp-backed storage instead of the user's data directory.
func WithPlansService(svc plans.Service) Option {
	return func(m *appModel) {
		if svc != nil {
			m.plansSvc = svc
		}
	}
}

// planMutationTimeoutOrDefault returns the persistence timeout for this
// model, falling back to the package default. Tests set the field to keep
// timeout scenarios fast.
func (m *appModel) planMutationTimeoutOrDefault() time.Duration {
	return cmp.Or(m.planMutationTimeout, defaultPlanMutationTimeout)
}

// planReadTimeoutOrDefault returns the read timeout for this model, falling
// back to the package default. Tests set the field to keep timeout scenarios
// fast.
func (m *appModel) planReadTimeoutOrDefault() time.Duration {
	return cmp.Or(m.planReadTimeout, defaultPlanReadTimeout)
}

// plansService returns the host-facing plan service, building it on first
// use. The lazy build matters: plan.SharedStorage() memoizes the plans
// directory process-wide, so it must not be touched before the CLI pre-run
// has applied path configuration (e.g. --data-dir).
func (m *appModel) plansService() plans.Service {
	if m.plansSvc == nil {
		m.plansSvc = plans.NewService(plan.SharedStorage())
	}
	return m.plansSvc
}

// currentPlanSessionID identifies the active session whose plan is included
// in listings. Only this session is ever consulted; plan files left behind
// by other sessions are never enumerated.
func (m *appModel) currentPlanSessionID() string {
	if m.application == nil {
		return ""
	}
	if sess := m.application.Session(); sess != nil {
		return sess.ID
	}
	return ""
}

// planListOptions snapshots the listing options from model state; commands
// run off the event loop and must not touch the model.
func (m *appModel) planListOptions() plans.ListOptions {
	return plans.ListOptions{SessionID: m.currentPlanSessionID()}
}

// planDialogOpen reports whether any dialog of the /plans flow is on the
// stack — topmost or buried under another dialog — i.e. plan data is on
// screen and worth live-refreshing.
func (m *appModel) planDialogOpen() bool {
	return m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		_, ok := d.(dialog.PlanDialog)
		return ok
	})
}

// planBrowserOpen reports whether the /plans browser dialog is on the stack,
// topmost or buried.
func (m *appModel) planBrowserOpen() bool {
	return m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		_, ok := d.(dialog.PlanBrowserViewer)
		return ok
	})
}

// planDetailOpen reports whether a detail dialog for exactly ref is on the
// stack, topmost or buried.
func (m *appModel) planDetailOpen(ref plans.Ref) bool {
	return m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		viewer, ok := d.(dialog.PlanDetailViewer)
		return ok && viewer.PlanRef() == ref
	})
}

func (m *appModel) handleShowPlanBrowser() (tea.Model, tea.Cmd) {
	// One browser only: with a browser already on the stack (even buried) or
	// its opening read already in flight for this session, a repeated /plans
	// must not start a second List or stack a duplicate browser. A request
	// for a different session may launch — the superseded read's result is
	// dropped as stale in handlePlanBrowserLoaded.
	if m.planBrowserOpen() {
		return m, nil
	}
	svc, ctx, opts := m.plansService(), m.ctx(), m.planListOptions()
	if m.planBrowserLoadInFlight && m.planBrowserLoadSessionID == opts.SessionID {
		return m, nil
	}
	m.planBrowserLoadInFlight = true
	m.planBrowserLoadSessionID = opts.SessionID
	timeout := m.planReadTimeoutOrDefault()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		result, err := svc.List(ctx, opts)
		return planBrowserLoadedMsg{sessionID: opts.SessionID, result: result, err: err}
	}
}

// handlePlanBrowserLoaded opens the /plans browser with the completed
// listing. A result whose session no longer matches the active one is
// dropped: the user switched tabs while the read was in flight, and popping
// a browser that lists the previous tab's session plan would mislead.
func (m *appModel) handlePlanBrowserLoaded(msg planBrowserLoadedMsg) (tea.Model, tea.Cmd) {
	// Clear the guard first, whatever the outcome, so a failed or stale open
	// never wedges /plans. A result whose session differs from the guard's
	// belongs to a superseded launch; the guard keeps tracking the newer
	// in-flight read.
	if msg.sessionID == m.planBrowserLoadSessionID {
		m.planBrowserLoadInFlight = false
	}
	if msg.sessionID != m.currentPlanSessionID() {
		return m, nil
	}
	if msg.err != nil {
		cmd := m.planReadFailureCmd(msg.err)
		return m, cmd
	}
	// A browser that appeared while this read was in flight wins; stacking a
	// second one would duplicate it.
	if m.planBrowserOpen() {
		return m, nil
	}
	cmds := []tea.Cmd{core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewPlanBrowserDialog(msg.result)})}
	cmds = append(cmds, planWarningsCmds(msg.result.Warnings)...)
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleRefreshPlans() (tea.Model, tea.Cmd) {
	cmd := m.planRefreshCmd(true)
	return m, cmd
}

// planRefreshCmd returns a command that reloads plan data for the open plan
// dialogs off the event loop: fresh rows for the browser and fresh content
// for every open detail dialog, buried ones included. The reads run in the
// command — never in Update, which a slow disk would stall — and report back
// as one planRefreshedMsg so the data is applied in a deterministic order.
// At most one reload runs at a time: requests arriving while one is in
// flight are coalesced into a single follow-up reload launched when the
// in-flight result lands, so event bursts cannot pile up redundant reads.
// Returns nil when the request was coalesced.
func (m *appModel) planRefreshCmd(notifyWarnings bool) tea.Cmd {
	if m.planRefreshInFlight {
		m.planRefreshQueued = true
		m.planRefreshQueuedWarnings = m.planRefreshQueuedWarnings || notifyWarnings
		return nil
	}
	m.planRefreshInFlight = true
	svc, ctx, opts := m.plansService(), m.ctx(), m.planListOptions()
	timeout := m.planReadTimeoutOrDefault()
	refs := m.openPlanDetailRefs()
	return func() tea.Msg {
		// One shared deadline for the whole reload: however wedged storage
		// is, the result always lands and clears the in-flight flag, so the
		// refresh pipeline can never get stuck.
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		msg := planRefreshedMsg{sessionID: opts.SessionID, notifyWarnings: notifyWarnings}
		msg.list, msg.listErr = svc.List(ctx, opts)
		for _, ref := range refs {
			p, err := svc.Get(ctx, ref)
			msg.details = append(msg.details, planDetailFetch{ref: ref, plan: p, err: err})
		}
		return msg
	}
}

// appendPlanRefreshCmd appends a coalesced refresh (without warning
// notifications) when one was launched; a coalesced request adds nothing.
func (m *appModel) appendPlanRefreshCmd(cmds []tea.Cmd) []tea.Cmd {
	if cmd := m.planRefreshCmd(false); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// handlePlanRefreshed applies a completed reload to the open plan dialogs.
// The in-flight flag is always cleared first, whatever the outcome, so the
// refresh pipeline can never wedge. With no plan dialog left the result —
// data, errors, and warnings alike — is dropped along with any queued
// follow-up: it has nowhere to land and a notification would reference
// nothing on screen. A result read for another session's listing (the user
// switched tabs while it was in flight) is dropped too, and exactly one
// fresh reload for the current session replaces it, folding in the queued
// intent. Otherwise the list data (or its failure) surfaces
// unconditionally. Detail read failures surface only for a detail that is
// the topmost dialog now, when the result is applied: a plan that
// disappeared closes it — through the targeted ClosePlanDetailMsg, so
// duplicated results for the same vanished plan can never pop a second
// dialog — with one notification, and any other failure notifies without
// closing. A buried detail cannot be closed without popping the wrong
// dialog, and notifying on every refresh would repeat the same warning
// indefinitely, so a buried failing ref is skipped silently: it is notified
// (and closed if gone) once it surfaces and the next refresh runs.
func (m *appModel) handlePlanRefreshed(msg planRefreshedMsg) (tea.Model, tea.Cmd) {
	m.planRefreshInFlight = false

	if !m.planDialogOpen() {
		m.planRefreshQueued = false
		m.planRefreshQueuedWarnings = false
		return m, nil
	}
	if msg.sessionID != m.currentPlanSessionID() {
		notify := msg.notifyWarnings || m.planRefreshQueuedWarnings
		m.planRefreshQueued = false
		m.planRefreshQueuedWarnings = false
		cmd := m.planRefreshCmd(notify)
		return m, cmd
	}

	var cmds []tea.Cmd
	if msg.listErr != nil {
		cmds = append(cmds, m.planReadFailureCmd(msg.listErr))
	} else {
		cmds = append(cmds, core.CmdHandler(dialog.PlanBrowserDataMsg{Result: msg.list}))
		if msg.notifyWarnings {
			cmds = append(cmds, planWarningsCmds(msg.list.Warnings)...)
		}
	}
	for _, fetch := range msg.details {
		if fetch.err != nil {
			if viewer, ok := m.dialogMgr.TopDialog().(dialog.PlanDetailViewer); !ok || viewer.PlanRef() != fetch.ref {
				continue
			}
			var notFound *plans.NotFoundError
			if errors.As(fetch.err, &notFound) {
				cmds = append(cmds, core.CmdHandler(dialog.ClosePlanDetailMsg{Ref: fetch.ref}))
			}
			cmds = append(cmds, m.planReadFailureCmd(fetch.err))
			continue
		}
		cmds = append(cmds, core.CmdHandler(dialog.PlanDetailDataMsg{Plan: fetch.plan}))
	}

	// Replay the requests that arrived while this reload was in flight as
	// one follow-up reload.
	if m.planRefreshQueued {
		m.planRefreshQueued = false
		notify := m.planRefreshQueuedWarnings
		m.planRefreshQueuedWarnings = false
		if cmd := m.planRefreshCmd(notify); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Sequence(cmds...)
}

// openPlanDetailRefs returns the refs shown by every open plan detail
// dialog, including ones buried under other dialogs. The predicate never
// matches: it is used as a visitor over the whole stack.
func (m *appModel) openPlanDetailRefs() []plans.Ref {
	var refs []plans.Ref
	m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		if viewer, ok := d.(dialog.PlanDetailViewer); ok {
			refs = append(refs, viewer.PlanRef())
		}
		return false
	})
	return refs
}

// planDetailFetch is one detail dialog's re-fetched plan (or the failure to
// fetch it) inside a planRefreshedMsg.
type planDetailFetch struct {
	ref  plans.Ref
	plan plans.Plan
	err  error
}

// planBrowserLoadedMsg reports the listing read that backs opening the
// /plans browser. sessionID is the session the listing was requested for,
// so a result that raced a tab switch can be told apart and dropped.
type planBrowserLoadedMsg struct {
	sessionID string
	result    plans.ListResult
	err       error
}

// planRefreshedMsg reports a completed asynchronous reload of the open plan
// dialogs: the browser listing plus the full plan of every detail dialog
// that was open when the reload started. sessionID is the session the
// listing was read for, so a reload that raced a tab switch can be told
// apart, dropped, and replaced by a fresh one.
type planRefreshedMsg struct {
	sessionID      string
	list           plans.ListResult
	listErr        error
	details        []planDetailFetch
	notifyWarnings bool
}

// planDetailLoadedMsg reports the read that backs opening a detail dialog.
// ref identifies the request so its in-flight guard is cleared on every
// outcome.
type planDetailLoadedMsg struct {
	ref  plans.Ref
	plan plans.Plan
	err  error
}

// planExportResultMsg reports a completed asynchronous export to path.
type planExportResultMsg struct {
	path   string
	result plans.ExportResult
	// exists marks a refused export: the default path already existed.
	exists bool
	// statErr reports a failed overwrite pre-check; the export was never
	// attempted.
	statErr error
	err     error
}

func (m *appModel) handleOpenPlanDetail(ref plans.Ref) (tea.Model, tea.Cmd) {
	// One detail per plan: with a detail for this ref already on the stack or
	// its opening read already in flight, a repeated open must not start a
	// second Get or stack a duplicate dialog.
	if m.planDetailOpen(ref) {
		return m, nil
	}
	if _, inFlight := m.planDetailLoadsInFlight[ref]; inFlight {
		return m, nil
	}
	if m.planDetailLoadsInFlight == nil {
		m.planDetailLoadsInFlight = make(map[plans.Ref]struct{})
	}
	m.planDetailLoadsInFlight[ref] = struct{}{}
	svc, ctx := m.plansService(), m.ctx()
	timeout := m.planReadTimeoutOrDefault()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		p, err := svc.Get(ctx, ref)
		return planDetailLoadedMsg{ref: ref, plan: p, err: err}
	}
}

// handlePlanDetailLoaded opens the detail dialog for a completed read. The
// result is dropped when no plan dialog is open anymore: the detail was
// requested from the browser, so after the user closed /plans while the
// read was in flight no dialog may pop up out of nowhere.
func (m *appModel) handlePlanDetailLoaded(msg planDetailLoadedMsg) (tea.Model, tea.Cmd) {
	// Clear the guard first, whatever the outcome, so a failed open can be
	// retried.
	delete(m.planDetailLoadsInFlight, msg.ref)
	if !m.planDialogOpen() {
		return m, nil
	}
	if msg.err != nil {
		cmd := m.planReadFailureCmd(msg.err)
		return m, cmd
	}
	// A detail for this plan that appeared while the read was in flight wins;
	// stacking a second copy would duplicate it.
	if m.planDetailOpen(msg.ref) {
		return m, nil
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewPlanDetailDialog(msg.plan)})
}

// handleExportPlan resolves the export destination from model state, then
// runs the read and the file write in a command so a large plan or a slow
// disk never stalls the event loop. At most one export per destination is
// in flight: concurrent duplicates would race the no-overwrite pre-check
// against the write and could both pass it. planExportsInFlight is only
// touched here and in handlePlanExportResult — never from the command.
func (m *appModel) handleExportPlan(ref plans.Ref) (tea.Model, tea.Cmd) {
	path := filepath.Join(m.activeWorkingDir(), planExportFilename(ref))
	if _, inFlight := m.planExportsInFlight[path]; inFlight {
		return m, notification.InfoCmd("An export to " + path + " is already running — wait for its result.")
	}
	if m.planExportsInFlight == nil {
		m.planExportsInFlight = make(map[string]struct{})
	}
	m.planExportsInFlight[path] = struct{}{}
	svc, ctx := m.plansService(), m.ctx()
	timeout := m.planReadTimeoutOrDefault()
	return m, func() tea.Msg {
		msg := planExportResultMsg{path: path}
		// Refuse to overwrite: the default path is deterministic, so a repeat
		// export must fail loudly instead of clobbering the previous file.
		if _, err := os.Stat(path); err == nil {
			msg.exists = true
			return msg
		} else if !errors.Is(err, fs.ErrNotExist) {
			msg.statErr = err
			return msg
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		msg.result, msg.err = svc.Export(ctx, plans.ExportRequest{Ref: ref, Path: path})
		return msg
	}
}

func (m *appModel) handlePlanExportResult(msg planExportResultMsg) (tea.Model, tea.Cmd) {
	// Whatever the outcome, the destination is no longer being exported to.
	delete(m.planExportsInFlight, msg.path)
	switch {
	case msg.exists:
		return m, notification.ErrorCmd(
			fmt.Sprintf("%s already exists — move it away, or export to a custom path with '%s plans export'.", msg.path, invocation.DockerAgent()))
	case msg.statErr != nil:
		return m, notification.ErrorCmd(fmt.Sprintf("Cannot export to %s: %v", msg.path, msg.statErr))
	case msg.err != nil:
		cmd := m.planReadFailureCmd(msg.err)
		return m, cmd
	}
	return m, notification.SuccessCmd(fmt.Sprintf("Exported %s plan to %s (%d bytes)", msg.result.Scope, msg.result.Path, msg.result.BytesWritten))
}

// planExportFilename is the deterministic default export target: the plan
// name for shared plans, a short session marker for session plans.
func planExportFilename(ref plans.Ref) string {
	if ref.Scope == plans.ScopeSession {
		return "session-plan-" + planShortSessionID(ref.SessionID) + ".md"
	}
	return ref.Name + ".md"
}

func planShortSessionID(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// activeWorkingDir is the working directory of the active tab's runtime,
// falling back to the process working directory.
func (m *appModel) activeWorkingDir() string {
	if m.supervisor != nil {
		if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil && runner.WorkingDir != "" {
			return runner.WorkingDir
		}
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// planStatusResultMsg reports a completed asynchronous set-status write.
type planStatusResultMsg struct {
	plan plans.Plan
	err  error
}

// planDeleteResultMsg reports a completed asynchronous delete.
type planDeleteResultMsg struct {
	ref             plans.Ref
	expectedVersion int
	err             error
}

// planWriteResultMsg reports a completed asynchronous editor-driven create
// or update. draftPath is the draft file the content came from; it is only
// removed after the service confirmed the write (or the draft turned out
// empty) and is preserved on every error so no edit is ever lost.
type planWriteResultMsg struct {
	ref       plans.Ref
	create    bool
	draftPath string
	plan      plans.Plan
	// emptyDraft marks a draft with no content: nothing was written and the
	// draft file was removed.
	emptyDraft bool
	// readErr reports a draft that could not be read (or was refused as
	// non-regular or oversized); the write was never attempted.
	readErr error
	// err reports a failed persistence call for a successfully read draft.
	err error
}

// handleSetPlanStatus starts the guarded status write in a command so a
// contended plans lock can never freeze the event loop; the outcome arrives
// as a planStatusResultMsg.
func (m *appModel) handleSetPlanStatus(msg messages.SetPlanStatusMsg) (tea.Model, tea.Cmd) {
	svc := m.plansService()
	ctx, timeout := m.ctx(), m.planMutationTimeoutOrDefault()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		expected := msg.ExpectedVersion
		p, err := svc.SetStatus(ctx, plans.SetStatusRequest{
			Ref:             msg.Ref,
			Status:          msg.Status,
			ExpectedVersion: &expected,
		})
		return planStatusResultMsg{plan: p, err: err}
	}
}

func (m *appModel) handlePlanStatusResult(msg planStatusResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		cmd := m.planWriteFailureCmd(msg.err)
		return m, cmd
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("Status of %q set to %q (now v%d)", msg.plan.Name, msg.plan.Status, planVersionOf(msg.plan)))}
	cmds = m.appendPlanRefreshCmd(cmds)
	return m, tea.Sequence(cmds...)
}

// handleDeletePlan starts the guarded delete in a command; the outcome
// arrives as a planDeleteResultMsg.
func (m *appModel) handleDeletePlan(msg messages.DeletePlanMsg) (tea.Model, tea.Cmd) {
	svc := m.plansService()
	ctx, timeout := m.ctx(), m.planMutationTimeoutOrDefault()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		expected := msg.ExpectedVersion
		err := svc.Delete(ctx, plans.DeleteRequest{Ref: msg.Ref, ExpectedVersion: &expected})
		return planDeleteResultMsg{ref: msg.Ref, expectedVersion: msg.ExpectedVersion, err: err}
	}
}

func (m *appModel) handlePlanDeleteResult(msg planDeleteResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		cmd := m.planWriteFailureCmd(msg.err)
		return m, cmd
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("Deleted shared plan %q (was v%d)", msg.ref.Name, msg.expectedVersion))}
	// A detail dialog showing the deleted plan has nothing left to show.
	// The targeted close re-checks the top when it is applied, so it can
	// never pop anything but that exact detail. The browser row is only
	// removed by the refresh below — never before the service confirmed the
	// delete.
	if viewer, ok := m.dialogMgr.TopDialog().(dialog.PlanDetailViewer); ok && viewer.PlanRef() == msg.ref {
		cmds = append(cmds, core.CmdHandler(dialog.ClosePlanDetailMsg{Ref: msg.ref}))
	}
	cmds = m.appendPlanRefreshCmd(cmds)
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleCreatePlan(name string) (tea.Model, tea.Cmd) {
	name = strings.TrimSpace(name)
	// Validate before opening the editor so a bad name fails fast instead
	// of discarding a drafted document.
	if err := plan.ValidateName(name); err != nil {
		return m, notification.ErrorCmd(err.Error())
	}
	tmpPath, err := planDraftFile("cagent-plan-new-*.md", "")
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to create draft file: %v", err))
	}
	cmd := m.execPlanEditor(planEditorClosedMsg{ref: plans.SharedRef(name), create: true, path: tmpPath})
	return m, cmd
}

// handleEditPlan prepares an editor-driven edit off the event loop: the
// current plan is read and the draft file seeded with its content in a
// command — wedged storage or a slow disk must never stall Update — and the
// outcome reports back as a planEditReadyMsg.
func (m *appModel) handleEditPlan(msg messages.EditPlanMsg) (tea.Model, tea.Cmd) {
	svc, ctx := m.plansService(), m.ctx()
	timeout := m.planReadTimeoutOrDefault()
	return m, func() tea.Msg {
		ready := planEditReadyMsg{ref: msg.Ref, expectedVersion: msg.ExpectedVersion}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		p, err := svc.Get(ctx, msg.Ref)
		if err != nil {
			ready.err = err
			return ready
		}
		ready.currentVersion = planVersionOf(p)
		if ready.currentVersion != msg.ExpectedVersion {
			// No draft for a drifted base; handlePlanEditReady refreshes
			// instead of editing. Session plans never take this branch: they
			// have no versions, so both sides are always 0.
			return ready
		}
		ready.draftPath, ready.draftErr = planDraftFile(planDraftPattern(msg.Ref), p.Content)
		return ready
	}
}

// planEditReadyMsg reports the preparation behind an editor-driven edit:
// the plan's current version and, when it still matches the version the
// user saw, the draft file seeded with the plan's content.
type planEditReadyMsg struct {
	ref             plans.Ref
	expectedVersion int
	// currentVersion is the version read from storage. When it differs from
	// expectedVersion no draft was created and the data on screen is
	// refreshed instead of edited.
	currentVersion int
	draftPath      string
	// draftErr reports a draft file that could not be created; the editor is
	// never opened.
	draftErr error
	// err reports a failed plan read; nothing else was attempted.
	err error
}

// handlePlanEditReady launches the external editor over the prepared draft,
// or surfaces why there is nothing to edit.
func (m *appModel) handlePlanEditReady(msg planEditReadyMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		cmd := m.planReadFailureCmd(msg.err)
		return m, cmd
	}
	// The plan moved on since the version on screen: refresh instead of
	// editing a base the user has not seen.
	if msg.currentVersion != msg.expectedVersion {
		cmds := []tea.Cmd{notification.WarningCmd(fmt.Sprintf(
			"Plan %q is at v%d now (you read v%d). Data refreshed — review and press e again.",
			msg.ref.Name, msg.currentVersion, msg.expectedVersion))}
		cmds = m.appendPlanRefreshCmd(cmds)
		return m, tea.Sequence(cmds...)
	}
	if msg.draftErr != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to create draft file: %v", msg.draftErr))
	}
	// The user closed the plan dialogs while the edit was being prepared:
	// taking over the terminal with an editor now would be disruptive. The
	// draft holds only the stored content, so removing it loses nothing.
	if !m.planDialogOpen() {
		_ = os.Remove(msg.draftPath)
		return m, nil
	}
	cmd := m.execPlanEditor(planEditorClosedMsg{ref: msg.ref, expectedVersion: msg.expectedVersion, path: msg.draftPath})
	return m, cmd
}

// planEditorClosedMsg reports that the external editor for a plan draft has
// exited; the app model then reads the draft and performs the guarded write.
type planEditorClosedMsg struct {
	ref             plans.Ref
	expectedVersion int
	create          bool
	path            string
	err             error
}

// planDraftPattern names the temp draft of an editor-driven edit after the
// plan's identity: the shared plan name, or a short session marker mirroring
// planExportFilename. Both are service-validated identifiers by the time a
// draft is created, so the pattern is filename-safe.
func planDraftPattern(ref plans.Ref) string {
	if ref.Scope == plans.ScopeSession {
		return "cagent-plan-session-" + planShortSessionID(ref.SessionID) + "-*.md"
	}
	return "cagent-plan-" + ref.Name + "-*.md"
}

// planDraftFile writes content to a fresh temp markdown file and returns its
// path.
func planDraftFile(pattern, content string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// execPlanEditor opens result.path in $VISUAL/$EDITOR via tea.ExecProcess
// and reports back with result once the editor exits.
func (m *appModel) execPlanEditor(result planEditorClosedMsg) tea.Cmd {
	return tea.ExecProcess(editorname.Command(result.path), func(err error) tea.Msg {
		result.err = err
		return result
	})
}

func (m *appModel) handlePlanEditorClosed(msg planEditorClosedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// The editor may have failed after the user saved content (e.g. it
		// exited non-zero); the draft is kept so no edit is ever lost.
		return m, tea.Sequence(
			notification.ErrorCmd(fmt.Sprintf("Editor error: %v", msg.err)),
			notification.InfoCmd("Your draft is kept at "+msg.path))
	}

	// Both the draft read and the persistence call run in a command: reading
	// in Update would stall the event loop on a draft path swapped for a FIFO
	// or device and allocate unbounded memory for a runaway file, and a
	// contended plans lock could freeze it just the same. The draft file
	// outlives the command until the service confirms the write.
	svc := m.plansService()
	ctx, timeout := m.ctx(), m.planMutationTimeoutOrDefault()
	return m, func() tea.Msg {
		result := planWriteResultMsg{ref: msg.ref, create: msg.create, draftPath: msg.path}
		content, err := readPlanDraft(msg.path)
		if err != nil {
			result.readErr = err
			return result
		}
		if strings.TrimSpace(content) == "" {
			_ = os.Remove(msg.path)
			result.emptyDraft = true
			return result
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		switch {
		case msg.create:
			result.plan, result.err = svc.Create(ctx, plans.CreateRequest{Ref: msg.ref, Content: content})
		case msg.ref.Scope == plans.ScopeSession:
			// Session plans have no versions: the replace is deliberately
			// unguarded, last-write-wins.
			result.plan, result.err = svc.UpdateSession(ctx, msg.ref.SessionID, content)
		default:
			expected := msg.expectedVersion
			result.plan, result.err = svc.Update(ctx, plans.UpdateRequest{Ref: msg.ref, Content: content, ExpectedVersion: &expected})
		}
		return result
	}
}

// readPlanDraft reads the edited draft back, bounded and hang-safe: the open
// cannot block on a FIFO with no writer (plan.OpenContentFile), only a
// regular file is accepted — checked on the opened descriptor, so a
// concurrent path swap cannot slip a device past the check — and the read is
// capped at the plan content limit so a runaway draft can never exhaust
// memory. The service would refuse over-cap content anyway; refusing here
// skips reading and shipping megabytes that cannot be persisted.
func readPlanDraft(path string) (string, error) {
	f, err := plan.OpenContentFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("draft %s is not a regular file", path)
	}
	if info.Size() > plan.MaxPlanContentSize {
		return "", fmt.Errorf("draft exceeds the maximum plan size (%d bytes; max %d)", info.Size(), plan.MaxPlanContentSize)
	}

	// Read one byte past the cap so a draft that grew since the size check
	// is still detected without trusting the stat size.
	data, err := io.ReadAll(io.LimitReader(f, plan.MaxPlanContentSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > plan.MaxPlanContentSize {
		return "", fmt.Errorf("draft exceeds the maximum plan size (max %d bytes)", plan.MaxPlanContentSize)
	}
	return string(data), nil
}

func (m *appModel) handlePlanWriteResult(msg planWriteResultMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.readErr != nil:
		return m, tea.Sequence(
			notification.ErrorCmd(fmt.Sprintf("Failed to read edited plan: %v", msg.readErr)),
			notification.InfoCmd("Your draft is kept at "+msg.draftPath))
	case msg.emptyDraft:
		switch {
		case msg.create:
			return m, notification.InfoCmd(fmt.Sprintf("Plan %q not created: the draft was empty.", msg.ref.Name))
		case msg.ref.Scope == plans.ScopeSession:
			return m, notification.InfoCmd("Session plan left unchanged: an empty draft is never committed.")
		default:
			return m, notification.InfoCmd(fmt.Sprintf("Plan %q left unchanged: an empty draft is never committed.", msg.ref.Name))
		}
	case msg.err != nil:
		cmd := m.planEditorFailureCmd(msg.err, msg.draftPath)
		return m, cmd
	}
	_ = os.Remove(msg.draftPath)

	var text string
	switch {
	case msg.create:
		text = fmt.Sprintf("Created shared plan %q (now v%d)", msg.plan.Name, planVersionOf(msg.plan))
	case msg.ref.Scope == plans.ScopeSession:
		// Session plans have no version to report.
		text = "Updated the current session plan."
	default:
		text = fmt.Sprintf("Updated shared plan %q (now v%d)", msg.plan.Name, planVersionOf(msg.plan))
	}
	cmds := []tea.Cmd{notification.SuccessCmd(text)}
	cmds = m.appendPlanRefreshCmd(cmds)
	return m, tea.Sequence(cmds...)
}

// planEditorFailureCmd reports a failed editor-driven write. The draft file
// is deliberately kept so a conflict, timeout, or storage failure never
// loses the user's edits; the newer plan content stays intact and is re-read
// into the open dialogs.
func (m *appModel) planEditorFailureCmd(err error, draftPath string) tea.Cmd {
	if timeout := m.planTimeoutCmd(err); timeout != nil {
		return tea.Sequence(timeout, notification.InfoCmd("Your draft is kept at "+draftPath))
	}
	var conflict *plans.ConflictError
	if errors.As(err, &conflict) {
		text := fmt.Sprintf(
			"Version conflict on %q: it is at v%d, you edited v%d. Your draft is kept at %s — refresh and retry from it.",
			conflict.Name, conflict.Current, conflict.Expected, draftPath)
		if conflict.Expected == 0 {
			text = fmt.Sprintf(
				"Plan %q already exists (v%d). Your draft is kept at %s — pick another name or edit the existing plan.",
				conflict.Name, conflict.Current, draftPath)
		}
		cmds := []tea.Cmd{notification.ErrorCmd(text)}
		cmds = m.appendPlanRefreshCmd(cmds)
		return tea.Sequence(cmds...)
	}
	return tea.Sequence(planErrorCmd(err), notification.InfoCmd("Your draft is kept at "+draftPath))
}

// planWriteFailureCmd reports a failed status/delete write. Conflicts
// refresh the open dialogs so the newer version is visible right away.
func (m *appModel) planWriteFailureCmd(err error) tea.Cmd {
	if timeout := m.planTimeoutCmd(err); timeout != nil {
		return timeout
	}
	var conflict *plans.ConflictError
	if errors.As(err, &conflict) {
		cmds := []tea.Cmd{notification.ErrorCmd(fmt.Sprintf(
			"Version conflict on %q: it changed to v%d since you read v%d. Data refreshed — review and retry.",
			conflict.Name, conflict.Current, conflict.Expected))}
		cmds = m.appendPlanRefreshCmd(cmds)
		return tea.Sequence(cmds...)
	}
	return planErrorCmd(err)
}

// planTimeoutCmd returns the actionable notification for a persistence
// operation that hit the bounded mutation timeout — most likely the
// cross-process plans lock is held by a wedged process — or nil when err is
// not a timeout.
func (m *appModel) planTimeoutCmd(err error) tea.Cmd {
	if !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return notification.ErrorCmd(fmt.Sprintf(
		"Plan write timed out after %s — the plan store may be locked by another process. Retry shortly.",
		m.planMutationTimeoutOrDefault()))
}

// planReadFailureCmd reports a failed plan read (list, get, export, or the
// read preparing an edit), mapping a hit read deadline onto an actionable
// timeout notification; every other failure goes through the typed
// planErrorCmd notifications.
func (m *appModel) planReadFailureCmd(err error) tea.Cmd {
	if !errors.Is(err, context.DeadlineExceeded) {
		return planErrorCmd(err)
	}
	return notification.ErrorCmd(fmt.Sprintf(
		"Plan read timed out after %s — plan storage may be unavailable. Retry shortly.",
		m.planReadTimeoutOrDefault()))
}

// planVersionOf reads a shared plan's version defensively; the service
// always sets it for shared plans.
func planVersionOf(p plans.Plan) int {
	if p.Version == nil {
		return 0
	}
	return *p.Version
}

// planErrorCmd maps the typed pkg/plans errors onto distinguishable
// user-facing notifications, never classifying by error text.
func planErrorCmd(err error) tea.Cmd {
	var (
		conflict    *plans.ConflictError
		notFound    *plans.NotFoundError
		validation  *plans.ValidationError
		corrupt     *plans.CorruptError
		unsupported *plans.UnsupportedError
	)
	switch {
	case errors.As(err, &conflict):
		return notification.ErrorCmd(fmt.Sprintf(
			"Version conflict on plan %q: expected v%d but it is at v%d. Refresh (r) and retry.",
			conflict.Name, conflict.Expected, conflict.Current))
	case errors.As(err, &notFound):
		return notification.WarningCmd(fmt.Sprintf("No %s plan %q — it may have been deleted; refresh (r).", notFound.Scope, notFound.Name))
	case errors.As(err, &validation):
		return notification.ErrorCmd("Invalid input: " + validation.Message)
	case errors.As(err, &corrupt):
		return notification.ErrorCmd(fmt.Sprintf("Plan %q is corrupt and cannot be read (%v). Delete it to recover.", corrupt.Name, corrupt.Err))
	case errors.As(err, &unsupported):
		return notification.InfoCmd("Unsupported: " + unsupported.Error())
	default:
		return notification.ErrorCmd("Plan storage failure: " + err.Error())
	}
}

func planWarningsCmds(warnings []string) []tea.Cmd {
	if len(warnings) == 0 {
		return nil
	}
	return []tea.Cmd{notification.WarningCmd(fmt.Sprintf(
		"%d plan(s) could not be read: %s", len(warnings), strings.Join(warnings, "; ")))}
}

// handleSessionPlanUpdatedEvent forwards the event to the chat page like any
// runtime event and live-refreshes open plan dialogs when the active
// session's plan changed. The refresh reads run in a command, never here.
func (m *appModel) handleSessionPlanUpdatedEvent(msg *runtime.SessionPlanUpdatedEvent) (tea.Model, tea.Cmd) {
	if name := msg.GetAgentName(); name != "" {
		m.sessionState.SetCurrentAgentName(name)
	}
	chatCmd := m.updateChatCmd(msg)
	var refresh tea.Cmd
	if m.planDialogOpen() && msg.SessionID == m.currentPlanSessionID() {
		refresh = m.planRefreshCmd(false)
	}
	return m, tea.Batch(chatCmd, refresh)
}

// handlePlanChangedEvent live-refreshes open plan dialogs after an agent
// mutated a shared plan. Shared plans are scope-global, so the refresh does
// not depend on which session emitted the event.
func (m *appModel) handlePlanChangedEvent(msg *runtime.PlanChangedEvent) (tea.Model, tea.Cmd) {
	if name := msg.GetAgentName(); name != "" {
		m.sessionState.SetCurrentAgentName(name)
	}
	chatCmd := m.updateChatCmd(msg)
	var refresh tea.Cmd
	if m.planDialogOpen() {
		refresh = m.planRefreshCmd(false)
	}
	return m, tea.Batch(chatCmd, refresh)
}
