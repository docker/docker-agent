package dialog

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// planDetailKeyMap defines key bindings for the plan detail dialog. Scrolling
// is handled by the scrollview's read-only key map.
type planDetailKeyMap struct {
	Escape  key.Binding
	Refresh key.Binding
	Export  key.Binding
	Status  key.Binding
	Delete  key.Binding
	Edit    key.Binding
}

func defaultPlanDetailKeyMap() planDetailKeyMap {
	return planDetailKeyMap{
		Escape:  key.NewBinding(key.WithKeys("esc", "q")),
		Refresh: key.NewBinding(key.WithKeys("r")),
		Export:  key.NewBinding(key.WithKeys("x")),
		Status:  key.NewBinding(key.WithKeys("s")),
		Delete:  key.NewBinding(key.WithKeys("d")),
		Edit:    key.NewBinding(key.WithKeys("e")),
	}
}

// planDetailChrome is border (2) + padding (2) of the dialog frame.
const planDetailChrome = 4

// planDetailDialog shows one plan in full: explicit scope, identity and
// metadata, and the markdown content in a scrollable viewport. It emits the
// same action intents as the browser; the app model owns all persistence.
type planDetailDialog struct {
	BaseDialog

	plan       plans.Plan
	scrollview *scrollview.Model
	keyMap     planDetailKeyMap
	// now supplies the reference time for the relative "updated" age, so it
	// advances on every render. Injectable for deterministic tests.
	now func() time.Time

	// Markdown render cache, invalidated on data refresh or width change.
	contentLines []string
	renderedFor  int
}

var (
	_ Dialog           = (*planDetailDialog)(nil)
	_ PlanDialog       = (*planDetailDialog)(nil)
	_ PlanDetailViewer = (*planDetailDialog)(nil)
)

// NewPlanDetailDialog creates the detail dialog for a plan fetched through
// the pkg/plans service (content included).
func NewPlanDetailDialog(p plans.Plan) Dialog {
	return &planDetailDialog{
		plan: p,
		scrollview: scrollview.New(
			scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()),
			scrollview.WithReserveScrollbarSpace(true),
		),
		keyMap: defaultPlanDetailKeyMap(),
		now:    time.Now,
	}
}

func (d *planDetailDialog) planDialog() {}

// PlanRef implements PlanDetailViewer so the app model can re-fetch the
// shown plan on refresh.
func (d *planDetailDialog) PlanRef() plans.Ref { return planRef(d.plan) }

func (d *planDetailDialog) Init() tea.Cmd {
	return nil
}

func (d *planDetailDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case PlanDetailDataMsg:
		// Broadcast refresh: only apply data for the plan this dialog shows.
		if planRef(msg.Plan) == d.PlanRef() {
			d.plan = msg.Plan
			d.contentLines = nil
			d.renderedFor = 0
		}
		return d, nil

	case tea.KeyPressMsg:
		return d.handleKeyPress(msg)
	}

	return d, nil
}

func (d *planDetailDialog) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	if cmd := HandleQuit(msg); cmd != nil {
		return d, cmd
	}

	switch {
	case key.Matches(msg, d.keyMap.Escape):
		return d, core.CmdHandler(CloseDialogMsg{})

	case key.Matches(msg, d.keyMap.Refresh):
		return d, core.CmdHandler(messages.RefreshPlansMsg{})

	case key.Matches(msg, d.keyMap.Export):
		return d, core.CmdHandler(messages.ExportPlanMsg{Ref: d.PlanRef()})

	case key.Matches(msg, d.keyMap.Status):
		if cmd := planMutationGuard(d.plan, "status"); cmd != nil {
			return d, cmd
		}
		return d, core.CmdHandler(OpenDialogMsg{Model: newPlanStatusDialog(d.plan.Name, d.plan.Status, *d.plan.Version)})

	case key.Matches(msg, d.keyMap.Delete):
		if cmd := planMutationGuard(d.plan, "delete"); cmd != nil {
			return d, cmd
		}
		return d, core.CmdHandler(OpenDialogMsg{Model: newPlanDeleteConfirmDialog(d.plan.Name, *d.plan.Version)})

	case key.Matches(msg, d.keyMap.Edit):
		if cmd := planMutationGuard(d.plan, "edit"); cmd != nil {
			return d, cmd
		}
		return d, core.CmdHandler(messages.EditPlanMsg{Ref: d.PlanRef(), ExpectedVersion: planVersionOrZero(d.plan)})
	}

	return d, nil
}

func (d *planDetailDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(80, 60, 110)
	maxHeight = min(d.Height()*80/100, 40)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

// headerLines renders the fixed metadata block above the scrollable content.
func (d *planDetailDialog) headerLines(contentWidth int) []string {
	p := d.plan

	title := "Plan: " + p.Name

	lines := []string{
		RenderTitle(toolcommon.TruncateText(title, contentWidth), contentWidth, styles.DialogTitleStyle),
		RenderSeparator(contentWidth),
	}

	field := func(label, value string) string {
		l := styles.MutedStyle.Render(planCell(label, 9))
		return l + styles.DialogContentStyle.Render(toolcommon.TruncateText(value, max(1, contentWidth-10)))
	}

	lines = append(lines,
		field("Scope", "shared — collaborative, versioned"),
		field("Name", p.Name),
	)
	if p.Title != "" {
		lines = append(lines, field("Title", p.Title))
	}
	lines = append(lines,
		field("Status", planLabel(p.Status)),
		field("Version", planVersionLabel(p.Version)),
		field("Author", planLabel(p.Author)),
		field("Updated", d.updatedLabel()),
		RenderSeparator(contentWidth),
	)
	return lines
}

func (d *planDetailDialog) updatedLabel() string {
	if d.plan.UpdatedAt.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s (%s)", d.plan.UpdatedAt.Local().Format("2006-01-02 15:04"), planTimeAgo(d.now(), d.plan.UpdatedAt))
}

// renderContent returns the markdown-rendered plan body, cached per width.
func (d *planDetailDialog) renderContent(contentWidth int) []string {
	if d.contentLines != nil && d.renderedFor == contentWidth {
		return d.contentLines
	}

	content := strings.TrimRight(d.plan.Content, "\n")
	var lines []string
	if strings.TrimSpace(content) == "" {
		lines = []string{styles.MutedStyle.Italic(true).Render("(empty plan)")}
	} else if rendered, err := markdown.NewRendererWithoutCopyIcon(contentWidth).Render(content); err == nil {
		lines = strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	} else {
		// Fallback: raw text, word-wrapped so long lines can't overflow.
		lines = toolcommon.WrapLinesWords(content, contentWidth)
	}

	// Belt and braces: cap every line so pathological content never breaks
	// the dialog frame.
	capStyle := lipgloss.NewStyle().MaxWidth(contentWidth)
	for i, l := range lines {
		if lipgloss.Width(l) > contentWidth {
			lines[i] = capStyle.Render(l)
		}
	}

	d.contentLines = lines
	d.renderedFor = contentWidth
	return lines
}

func (d *planDetailDialog) helpKeys() []string {
	keys := []string{"↑/↓", "scroll", "r", "refresh", "x", "export"}
	keys = append(keys, "s", "status", "e", "edit", "d", "delete")
	return append(keys, "esc", "close")
}

// viewport computes the number of scrollable content lines that fit.
func (d *planDetailDialog) viewport(headerCount int) int {
	_, maxHeight, _ := d.dialogSize()
	// header + space(1) + help(1) + frame
	return max(1, maxHeight-headerCount-2-planDetailChrome)
}

func (d *planDetailDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	regionWidth := contentWidth + d.scrollview.ReservedCols()

	header := d.headerLines(contentWidth)
	contentLines := d.renderContent(contentWidth)
	viewport := d.viewport(len(header))

	d.scrollview.SetSize(regionWidth, viewport)
	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+planDetailChrome/2+len(header))
	d.scrollview.SetContent(contentLines, len(contentLines))

	scrollOut := d.scrollview.View()
	scrollLines := strings.Split(scrollOut, "\n")
	for len(scrollLines) < viewport {
		scrollLines = append(scrollLines, "")
	}
	scrollLines = scrollLines[:viewport]

	parts := make([]string, 0, len(header)+viewport+2)
	parts = append(parts, header...)
	parts = append(parts, scrollLines...)
	parts = append(parts, "", RenderHelpKeys(regionWidth, d.helpKeys()...))

	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}

// SetSize sets the dialog dimensions and configures the scrollview region.
func (d *planDetailDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	_, _, contentWidth := d.dialogSize()
	regionWidth := contentWidth + d.scrollview.ReservedCols()
	d.scrollview.SetSize(regionWidth, d.viewport(len(d.headerLines(contentWidth))))
	return cmd
}

func (d *planDetailDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}
