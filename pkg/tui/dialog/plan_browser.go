package dialog

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// PlanBrowserDataMsg replaces the plan browser's rows with a fresh listing.
// It is Broadcastable so a browser buried under the detail dialog stays in
// sync after a write.
type PlanBrowserDataMsg struct {
	Result plans.ListResult
}

// BroadcastToDialogs implements Broadcastable.
func (PlanBrowserDataMsg) BroadcastToDialogs() {}

// PlanDetailDataMsg replaces the plan shown by an open detail dialog. It is
// Broadcastable for the same reason as PlanBrowserDataMsg; a detail dialog
// only applies it when the plan identity matches what it is showing.
type PlanDetailDataMsg struct {
	Plan plans.Plan
}

// BroadcastToDialogs implements Broadcastable.
func (PlanDetailDataMsg) BroadcastToDialogs() {}

// ClosePlanDetailMsg closes the topmost dialog only when it is a plan detail
// dialog showing exactly Ref. The manager checks the current top when the
// message is applied — not when it was emitted — so duplicated closes for
// the same vanished plan pop at most one dialog, and a close arriving after
// the detail was already closed (or covered by another dialog) is a no-op
// instead of popping the wrong dialog.
type ClosePlanDetailMsg struct {
	Ref plans.Ref
}

// PlanDialog is implemented by every dialog of the /plans flow, so the app
// model can tell whether plan data is on screen and needs live refreshing.
type PlanDialog interface {
	planDialog()
}

// PlanDetailViewer identifies an open plan detail dialog and the plan it
// shows, so the app model can re-fetch exactly that plan on refresh.
type PlanDetailViewer interface {
	PlanRef() plans.Ref
}

// PlanBrowserViewer identifies the /plans browser dialog on the stack, so
// the app model can refuse stacking a duplicate browser. The marker method
// is unexported like PlanDialog's: only this package's browser implements
// it, while other packages can still assert against the interface.
type PlanBrowserViewer interface {
	planBrowserDialog()
}

// planBrowserKeyMap defines key bindings for the plan browser.
type planBrowserKeyMap struct {
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Escape  key.Binding
	Filter  key.Binding
	Refresh key.Binding
	Export  key.Binding
	Status  key.Binding
	Delete  key.Binding
	New     key.Binding
	Edit    key.Binding
}

func defaultPlanBrowserKeyMap() planBrowserKeyMap {
	return planBrowserKeyMap{
		Up:      key.NewBinding(key.WithKeys("up", "ctrl+k")),
		Down:    key.NewBinding(key.WithKeys("down", "ctrl+j")),
		Enter:   key.NewBinding(key.WithKeys("enter")),
		Escape:  key.NewBinding(key.WithKeys("esc")),
		Filter:  key.NewBinding(key.WithKeys("/")),
		Refresh: key.NewBinding(key.WithKeys("r")),
		Export:  key.NewBinding(key.WithKeys("x")),
		Status:  key.NewBinding(key.WithKeys("s")),
		Delete:  key.NewBinding(key.WithKeys("d")),
		New:     key.NewBinding(key.WithKeys("n")),
		Edit:    key.NewBinding(key.WithKeys("e")),
	}
}

// Plan browser dialog dimension constants, mirroring the session browser.
const (
	planBrowserListOverhead = 12 // title(1) + space(1) + input(1) + separator(1) + separator(1) + footer(1) + space(1) + help(1) + borders(2) + extra(2)
	planBrowserListStartY   = 6  // border(1) + padding(1) + title(1) + space(1) + input(1) + separator(1)
)

// Column widths of a plan row; the title takes the remaining width.
const (
	planColScope   = 7
	planColName    = 22
	planColStatus  = 12
	planColVersion = 4
	planColUpdated = 8
	planColGap     = 2
)

type planBrowserDialog struct {
	BaseDialog

	filterInput textinput.Model
	// filtering is true while keystrokes go to the filter input; outside
	// filter mode plain letters are action keys (r/x/s/d/n/e).
	filtering bool

	all      []plans.Plan
	warnings []string
	filtered []plans.Plan
	selected int

	scrollview *scrollview.Model
	keyMap     planBrowserKeyMap
	// now supplies the reference time for relative "updated" ages, so they
	// advance on every render. Injectable for deterministic tests.
	now func() time.Time

	// Double-click detection
	lastClickTime  time.Time
	lastClickIndex int
}

var (
	_ Dialog            = (*planBrowserDialog)(nil)
	_ PlanDialog        = (*planBrowserDialog)(nil)
	_ PlanBrowserViewer = (*planBrowserDialog)(nil)
)

// NewPlanBrowserDialog creates the /plans browser over a listing produced by
// the pkg/plans service. The dialog never touches plan storage itself: every
// action is emitted as a message the app model services.
func NewPlanBrowserDialog(result plans.ListResult) Dialog {
	ti := textinput.New()
	ti.Placeholder = "Filter plans…"
	ti.CharLimit = 100
	ti.SetWidth(50)

	d := &planBrowserDialog{
		filterInput:    ti,
		scrollview:     scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
		keyMap:         defaultPlanBrowserKeyMap(),
		now:            time.Now,
		lastClickIndex: -1,
	}
	d.setData(result)
	return d
}

func (d *planBrowserDialog) planDialog() {}

func (d *planBrowserDialog) planBrowserDialog() {}

func (d *planBrowserDialog) Init() tea.Cmd {
	return nil
}

// setData replaces the listing and re-applies the filter. When the selected
// plan still exists the selection follows it and the viewport keeps its
// scroll position (clamped if the list shrank), so a live refresh never
// yanks the view back to the top mid-scroll.
func (d *planBrowserDialog) setData(result plans.ListResult) {
	var selectedRef *plans.Ref
	if p, ok := d.selectedPlan(); ok {
		ref := planRef(p)
		selectedRef = &ref
	}
	offset := d.scrollview.ScrollOffset()

	d.all = result.Plans
	d.warnings = result.Warnings
	d.applyFilter()

	if selectedRef != nil {
		for i, p := range d.filtered {
			if planRef(p) == *selectedRef {
				d.selected = i
				// SetScrollOffset clamps against the new row count, so a
				// shrunken list can never leave the viewport past the end.
				d.scrollview.SetScrollOffset(offset)
				break
			}
		}
	}
	d.ensureSelectedVisible()
}

func (d *planBrowserDialog) selectedPlan() (plans.Plan, bool) {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return plans.Plan{}, false
	}
	return d.filtered[d.selected], true
}

// planRef derives the service address of a listed plan.
func planRef(p plans.Plan) plans.Ref {
	if p.Scope == plans.ScopeSession {
		return plans.SessionRef(p.SessionID)
	}
	return plans.SharedRef(p.Name)
}

// planCurrentSessionLabel is the browser-row identity of the listed session
// plan. The service only ever lists the active session's plan, so labelling
// it beats showing a bare session ID that means nothing at a glance; the
// full ID stays visible in the footer and the detail dialog.
const planCurrentSessionLabel = "current session"

// planDisplayName is the identity a browser row shows: the shared plan's
// name, or the current-session label for the session plan.
func planDisplayName(p plans.Plan) string {
	if p.Scope == plans.ScopeSession {
		return planCurrentSessionLabel
	}
	return p.Name
}

func (d *planBrowserDialog) applyFilter() {
	query := strings.ToLower(strings.TrimSpace(d.filterInput.Value()))
	d.filtered = d.filtered[:0]
	for _, p := range d.all {
		if query == "" ||
			strings.Contains(strings.ToLower(p.Name), query) ||
			strings.Contains(strings.ToLower(planDisplayName(p)), query) ||
			strings.Contains(strings.ToLower(p.Title), query) ||
			strings.Contains(strings.ToLower(p.Status), query) ||
			strings.Contains(string(p.Scope), query) {
			d.filtered = append(d.filtered, p)
		}
	}
	if d.selected >= len(d.filtered) {
		d.selected = max(0, len(d.filtered)-1)
	}
	d.scrollview.SetContent(nil, len(d.filtered))
	d.scrollview.SetScrollOffset(0)
}

func (d *planBrowserDialog) ensureSelectedVisible() {
	if d.selected >= 0 && d.selected < len(d.filtered) {
		d.scrollview.EnsureLineVisible(d.selected)
	}
}

func (d *planBrowserDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case PlanBrowserDataMsg:
		d.setData(msg.Result)
		return d, nil

	case tea.PasteMsg:
		if d.filtering {
			var cmd tea.Cmd
			d.filterInput, cmd = d.filterInput.Update(msg)
			d.applyFilter()
			return d, cmd
		}
		return d, nil

	case tea.MouseClickMsg:
		return d.handleMouseClick(msg)

	case tea.KeyPressMsg:
		return d.handleKeyPress(msg)
	}

	return d, nil
}

func (d *planBrowserDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return d, nil
	}
	idx := d.mouseYToPlanIndex(msg.Y)
	if idx < 0 {
		return d, nil
	}
	now := time.Now()
	if idx == d.lastClickIndex && now.Sub(d.lastClickTime) < styles.DoubleClickThreshold {
		d.selected = idx
		d.lastClickTime = time.Time{}
		cmd := d.openDetailCmd()
		return d, cmd
	}
	d.selected = idx
	d.lastClickTime = now
	d.lastClickIndex = idx
	return d, nil
}

func (d *planBrowserDialog) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	if cmd := HandleQuit(msg); cmd != nil {
		return d, cmd
	}

	// Navigation works in both modes: the filter input has no use for
	// up/down/enter.
	switch {
	case key.Matches(msg, d.keyMap.Up):
		if d.selected > 0 {
			d.selected--
			d.ensureSelectedVisible()
		}
		return d, nil

	case key.Matches(msg, d.keyMap.Down):
		if d.selected < len(d.filtered)-1 {
			d.selected++
			d.ensureSelectedVisible()
		}
		return d, nil

	case key.Matches(msg, d.keyMap.Enter):
		d.stopFiltering()
		cmd := d.openDetailCmd()
		return d, cmd

	case key.Matches(msg, d.keyMap.Escape):
		if d.filtering {
			d.stopFiltering()
			return d, nil
		}
		return d, core.CmdHandler(CloseDialogMsg{})
	}

	if d.filtering {
		var cmd tea.Cmd
		d.filterInput, cmd = d.filterInput.Update(msg)
		d.applyFilter()
		return d, cmd
	}

	switch {
	case key.Matches(msg, d.keyMap.Filter):
		d.filtering = true
		return d, d.filterInput.Focus()

	case key.Matches(msg, d.keyMap.Refresh):
		return d, core.CmdHandler(messages.RefreshPlansMsg{})

	case key.Matches(msg, d.keyMap.Export):
		if p, ok := d.selectedPlan(); ok {
			return d, core.CmdHandler(messages.ExportPlanMsg{Ref: planRef(p)})
		}
		return d, nil

	case key.Matches(msg, d.keyMap.Status):
		cmd := d.statusCmd()
		return d, cmd

	case key.Matches(msg, d.keyMap.Delete):
		cmd := d.deleteCmd()
		return d, cmd

	case key.Matches(msg, d.keyMap.New):
		return d, core.CmdHandler(OpenDialogMsg{Model: newPlanNameDialog()})

	case key.Matches(msg, d.keyMap.Edit):
		cmd := d.editCmd()
		return d, cmd
	}

	return d, nil
}

func (d *planBrowserDialog) stopFiltering() {
	d.filtering = false
	d.filterInput.Blur()
}

func (d *planBrowserDialog) openDetailCmd() tea.Cmd {
	p, ok := d.selectedPlan()
	if !ok {
		return nil
	}
	return core.CmdHandler(messages.OpenPlanDetailMsg{Ref: planRef(p)})
}

// guardedPlan returns the selected plan when the given action applies to it:
// session plans support only edit, and shared plans must carry a displayed
// version. A refused action yields an explanatory notification instead of a
// failed service call.
func (d *planBrowserDialog) guardedPlan(action string) (plans.Plan, tea.Cmd, bool) {
	p, ok := d.selectedPlan()
	if !ok {
		return plans.Plan{}, nil, false
	}
	if cmd := planMutationGuard(p, action); cmd != nil {
		return plans.Plan{}, cmd, false
	}
	return p, nil, true
}

func (d *planBrowserDialog) statusCmd() tea.Cmd {
	p, cmd, ok := d.guardedPlan("status")
	if !ok {
		return cmd
	}
	return core.CmdHandler(OpenDialogMsg{Model: newPlanStatusDialog(p.Name, p.Status, *p.Version)})
}

func (d *planBrowserDialog) deleteCmd() tea.Cmd {
	p, cmd, ok := d.guardedPlan("delete")
	if !ok {
		return cmd
	}
	return core.CmdHandler(OpenDialogMsg{Model: newPlanDeleteConfirmDialog(p.Name, *p.Version)})
}

func (d *planBrowserDialog) editCmd() tea.Cmd {
	p, cmd, ok := d.guardedPlan("edit")
	if !ok {
		return cmd
	}
	return core.CmdHandler(messages.EditPlanMsg{Ref: planRef(p), ExpectedVersion: planVersionOrZero(p)})
}

// planMutationGuard returns an explanatory notification when the plan does
// not support the action from the host: session plans support only edit —
// they belong to their session and carry no shared-plan metadata — and a
// shared plan without a version (which the service always provides) is
// refused rather than mutated unguarded.
func planMutationGuard(p plans.Plan, action string) tea.Cmd {
	if p.Scope == plans.ScopeSession {
		if action == "edit" {
			return nil
		}
		return notification.InfoCmd(fmt.Sprintf(
			"Session plans don't support %s: they belong to their session and carry no shared-plan metadata. Press e to edit the plan body, or use a shared plan.", action,
		))
	}
	if p.Version == nil {
		return notification.ErrorCmd(fmt.Sprintf("Cannot %s %q: no version is known; refresh (r) and retry.", action, p.Name))
	}
	return nil
}

func (d *planBrowserDialog) mouseYToPlanIndex(y int) int {
	dialogRow, _ := d.Position()
	visLines := d.scrollview.VisibleHeight()
	listStartY := dialogRow + planBrowserListStartY

	if y < listStartY || y >= listStartY+visLines {
		return -1
	}
	idx := d.scrollview.ScrollOffset() + (y - listStartY)
	if idx < 0 || idx >= len(d.filtered) {
		return -1
	}
	return idx
}

func (d *planBrowserDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(85, 60, 120)
	maxHeight = min(d.Height()*70/100, 30)
	contentWidth = dialogWidth - 6 - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *planBrowserDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.filterInput.SetWidth(contentWidth)

	regionWidth := contentWidth + d.scrollview.ReservedCols()
	visibleLines := d.scrollview.VisibleHeight()

	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+planBrowserListStartY)

	total := len(d.filtered)
	d.scrollview.SetContent(nil, total)
	d.scrollview.SetScrollOffset(d.scrollview.ScrollOffset())

	var scrollableContent string
	if total == 0 {
		message := "No plans yet — press n to create a shared plan"
		if strings.TrimSpace(d.filterInput.Value()) != "" {
			message = "No plans match the filter"
		}
		emptyLines := []string{"", styles.DialogContentStyle.
			Italic(true).Align(lipgloss.Center).Width(contentWidth).
			Render(message)}
		for len(emptyLines) < visibleLines {
			emptyLines = append(emptyLines, "")
		}
		scrollableContent = d.scrollview.ViewWithLines(emptyLines)
	} else {
		offset := d.scrollview.ScrollOffset()
		end := min(offset+visibleLines, total)
		windowLines := make([]string, 0, end-offset)
		for i := offset; i < end; i++ {
			windowLines = append(windowLines, d.renderPlan(d.filtered[i], i == d.selected, contentWidth))
		}
		scrollableContent = d.scrollview.ViewWithLines(windowLines)
	}

	var countLabel string
	if len(d.filtered) == len(d.all) {
		countLabel = strconv.Itoa(len(d.all))
	} else {
		countLabel = fmt.Sprintf("%d/%d", len(d.filtered), len(d.all))
	}
	title := fmt.Sprintf("Plans (%s)", countLabel)

	filterView := d.filterInput.View()
	if !d.filtering && strings.TrimSpace(d.filterInput.Value()) == "" {
		filterView = styles.MutedStyle.Render("Press / to filter")
	}

	footer := d.footerLine(contentWidth)

	content := NewContent(regionWidth).
		AddTitle(title).
		AddSpace().
		AddContent(filterView).
		AddSeparator().
		AddContent(scrollableContent).
		AddSeparator().
		AddContent(footer).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "detail", "/", "filter", "r", "refresh", "esc", "close").
		AddHelpKeys("n", "new", "e", "edit", "s", "status", "x", "export", "d", "delete").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

// footerLine shows load warnings when present, otherwise the identity of the
// selected plan (useful for truncated names such as session IDs).
func (d *planBrowserDialog) footerLine(contentWidth int) string {
	if len(d.warnings) > 0 {
		text := fmt.Sprintf("⚠ %d plan(s) could not be read: %s", len(d.warnings), d.warnings[0])
		return styles.WarningStyle.Render(toolcommon.TruncateText(text, contentWidth))
	}
	p, ok := d.selectedPlan()
	if !ok {
		return ""
	}
	label := string(p.Scope) + " plan: "
	return styles.MutedStyle.Render(label) + styles.SecondaryStyle.Render(toolcommon.TruncateText(p.Name, max(1, contentWidth-lipgloss.Width(label))))
}

// SetSize sets the dialog dimensions and configures the scrollview region.
func (d *planBrowserDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	_, maxHeight, contentWidth := d.dialogSize()
	regionWidth := contentWidth + d.scrollview.ReservedCols()
	visibleLines := max(1, maxHeight-planBrowserListOverhead)
	d.scrollview.SetSize(regionWidth, visibleLines)
	return cmd
}

func (d *planBrowserDialog) renderPlan(p plans.Plan, selected bool, maxWidth int) string {
	mainStyle, metaStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	scopeStyle := styles.MutedStyle
	if p.Scope == plans.ScopeSession {
		scopeStyle = styles.WarningStyle
	}
	if selected {
		mainStyle, metaStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
		scopeStyle = metaStyle
	}

	gap := strings.Repeat(" ", planColGap)
	fixed := planColScope + planColName + planColStatus + planColVersion + planColUpdated + 5*planColGap
	titleWidth := max(0, maxWidth-fixed)

	row := scopeStyle.Render(planCell(string(p.Scope), planColScope)) + gap +
		mainStyle.Render(planCell(planDisplayName(p), planColName)) + gap +
		metaStyle.Render(planCell(planLabel(p.Status), planColStatus)) + gap +
		metaStyle.Render(planCell(planVersionLabel(p.Version), planColVersion)) + gap +
		metaStyle.Render(planCell(planTimeAgo(d.now(), p.UpdatedAt), planColUpdated)) + gap +
		metaStyle.Render(toolcommon.TruncateText(planLabel(p.Title), titleWidth))

	// Hard cap so a pathological row can never overflow the dialog.
	return lipgloss.NewStyle().MaxWidth(maxWidth).Render(row)
}

// planCell truncates s to width and pads it to exactly width columns.
func planCell(s string, width int) string {
	s = toolcommon.TruncateText(s, width)
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

// planLabel substitutes a dash for empty display metadata.
func planLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// planVersionLabel renders a shared plan's version and "-" for session
// plans, which have none.
func planVersionLabel(version *int) string {
	if version == nil {
		return "-"
	}
	return "v" + strconv.Itoa(*version)
}

// planVersionOrZero reads a plan's displayed version, with 0 as the
// no-version sentinel for session plans (shared versions start at 1).
func planVersionOrZero(p plans.Plan) int {
	if p.Version == nil {
		return 0
	}
	return *p.Version
}

func planTimeAgo(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	elapsed := now.Sub(t)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(elapsed.Seconds())))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

func (d *planBrowserDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}

// --- Status input dialog ---

// planStatusDialog is the small text-input dialog behind the `s` action. It
// emits SetPlanStatusMsg guarded by the version that was displayed when the
// action started.
type planStatusDialog struct {
	BaseDialog

	input   textinput.Model
	name    string
	version int
}

var (
	_ Dialog     = (*planStatusDialog)(nil)
	_ PlanDialog = (*planStatusDialog)(nil)
)

func newPlanStatusDialog(name, currentStatus string, version int) Dialog {
	ti := textinput.New()
	ti.Placeholder = "e.g. in-progress, blocked, done"
	ti.CharLimit = 100
	ti.SetWidth(50)
	ti.SetValue(currentStatus)
	ti.Focus()

	return &planStatusDialog{input: ti, name: name, version: version}
}

func (d *planStatusDialog) planDialog() {}

func (d *planStatusDialog) Init() tea.Cmd {
	return textinput.Blink
}

func (d *planStatusDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch msg.String() {
		case "esc":
			return d, core.CmdHandler(CloseDialogMsg{})
		case "enter":
			status := strings.TrimSpace(d.input.Value())
			if status == "" {
				return d, notification.ErrorCmd("Status must not be empty.")
			}
			return d, tea.Sequence(
				core.CmdHandler(CloseDialogMsg{}),
				core.CmdHandler(messages.SetPlanStatusMsg{
					Ref:             plans.SharedRef(d.name),
					Status:          status,
					ExpectedVersion: d.version,
				}),
			)
		default:
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			return d, cmd
		}
	}
	return d, nil
}

func (d *planStatusDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 40, 70)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	d.input.SetWidth(contentWidth)

	content := NewContent(contentWidth).
		AddTitle(fmt.Sprintf("Set status: %s (v%d)", d.name, d.version)).
		AddSeparator().
		AddSpace().
		AddContent(d.input.View()).
		AddSpace().
		AddHelpKeys("enter", "apply", "esc", "cancel").
		Build()

	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}

func (d *planStatusDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// --- Delete confirmation dialog ---

// planDeleteConfirmDialog names the plan and the version the delete is
// guarded by; the actual delete only happens in the app model after Yes.
type planDeleteConfirmDialog struct {
	BaseDialog

	name    string
	version int
	keyMap  ConfirmKeyMap
	escape  key.Binding
}

var (
	_ Dialog     = (*planDeleteConfirmDialog)(nil)
	_ PlanDialog = (*planDeleteConfirmDialog)(nil)
)

func newPlanDeleteConfirmDialog(name string, version int) Dialog {
	return &planDeleteConfirmDialog{
		name:    name,
		version: version,
		keyMap:  DefaultConfirmKeyMap(),
		escape:  key.NewBinding(key.WithKeys("esc")),
	}
}

func (d *planDeleteConfirmDialog) planDialog() {}

func (d *planDeleteConfirmDialog) Init() tea.Cmd {
	return nil
}

func (d *planDeleteConfirmDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		if key.Matches(msg, d.escape) {
			return d, core.CmdHandler(CloseDialogMsg{})
		}
		if model, cmd, handled := HandleConfirmKeys(msg, d.keyMap,
			func() (layout.Model, tea.Cmd) {
				return d, tea.Sequence(
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(messages.DeletePlanMsg{
						Ref:             plans.SharedRef(d.name),
						ExpectedVersion: d.version,
					}),
				)
			},
			func() (layout.Model, tea.Cmd) {
				return d, core.CmdHandler(CloseDialogMsg{})
			},
		); handled {
			return model, cmd
		}
	}
	return d, nil
}

func (d *planDeleteConfirmDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 40, 70)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Delete plan").
		AddSeparator().
		AddSpace().
		AddQuestion(fmt.Sprintf("Delete shared plan %q at version %d? This cannot be undone.", d.name, d.version)).
		AddSpace().
		AddHelpKeys("Y", "delete", "N/esc", "cancel").
		Build()

	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}

func (d *planDeleteConfirmDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// --- New plan name dialog ---

// planNameDialog asks for the name of a new shared plan; the content is then
// drafted in the external editor by the app model.
type planNameDialog struct {
	BaseDialog

	input textinput.Model
}

var (
	_ Dialog     = (*planNameDialog)(nil)
	_ PlanDialog = (*planNameDialog)(nil)
)

func newPlanNameDialog() Dialog {
	ti := textinput.New()
	ti.Placeholder = "plan-name (lowercase letters, digits, - and _)"
	ti.CharLimit = 100
	ti.SetWidth(50)
	ti.Focus()

	return &planNameDialog{input: ti}
}

func (d *planNameDialog) planDialog() {}

func (d *planNameDialog) Init() tea.Cmd {
	return textinput.Blink
}

func (d *planNameDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch msg.String() {
		case "esc":
			return d, core.CmdHandler(CloseDialogMsg{})
		case "enter":
			name := strings.TrimSpace(d.input.Value())
			if name == "" {
				return d, notification.ErrorCmd("Plan name must not be empty.")
			}
			return d, tea.Sequence(
				core.CmdHandler(CloseDialogMsg{}),
				core.CmdHandler(messages.CreatePlanMsg{Name: name}),
			)
		default:
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			return d, cmd
		}
	}
	return d, nil
}

func (d *planNameDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 40, 70)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	d.input.SetWidth(contentWidth)

	content := NewContent(contentWidth).
		AddTitle("New shared plan").
		AddSeparator().
		AddSpace().
		AddContent(d.input.View()).
		AddSpace().
		AddHelpKeys("enter", "open editor", "esc", "cancel").
		Build()

	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}

func (d *planNameDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}
