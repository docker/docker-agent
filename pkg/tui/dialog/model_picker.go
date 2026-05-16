package dialog

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// modelPickerDialog is a dialog for selecting a model for the current agent.
type modelPickerDialog struct {
	pickerCore

	models   []runtime.ModelChoice
	filtered []runtime.ModelChoice
	errMsg   string // validation error message
}

// Model picker dialog dimension constants
const (
	// Column widths for the per-row stats. Values are right-aligned in their
	// own column so the list reads like a table.
	pickerInputColWidth   = 10
	pickerOutputColWidth  = 10
	pickerContextColWidth = 8

	// pickerDetailsLines is the number of lines reserved for the model
	// details panel rendered below the model list.
	pickerDetailsLines = 4

	// pickerListVerticalOverhead is the number of rows used by dialog chrome:
	// title(1) + space(1) + input(1) + separator(1) + column header(1) +
	// details separator(1) + details (pickerDetailsLines) + space at bottom(1) +
	// help keys(1) + borders/padding(2) = 10 + pickerDetailsLines
	pickerListVerticalOverhead = 10 + pickerDetailsLines

	// pickerListStartOffset is the Y offset from dialog top to where the model list starts:
	// border(1) + padding(1) + title(1) + space(1) + input(1) + separator(1) +
	// column header(1) = 7
	pickerListStartOffset = 7

	// pickerDetailsLabelWidth is the column width for the labels in the
	// details panel ("Reference", "Pricing", "Limits", "Modalities").
	pickerDetailsLabelWidth = 12

	// catalogSeparatorLabel labels the separator above the catalog group.
	catalogSeparatorLabel = "Other models"
	// customSeparatorLabel labels the separator above the custom-models group.
	customSeparatorLabel = "Custom models"
)

// modelPickerLayout is the layout used by the model picker.
var modelPickerLayout = pickerLayout{
	WidthPercent:    pickerWidthPercent,
	MinWidth:        pickerMinWidth,
	MaxWidth:        pickerMaxWidth,
	HeightPercent:   pickerHeightPercent,
	MaxHeight:       pickerMaxHeight,
	ListOverhead:    pickerListVerticalOverhead,
	ListStartOffset: pickerListStartOffset,
}

// NewModelPickerDialog creates a new model picker dialog.
func NewModelPickerDialog(models []runtime.ModelChoice) Dialog {
	d := &modelPickerDialog{
		pickerCore: newPickerCore(modelPickerLayout, "Type to search or enter custom model (provider/model)…"),
	}
	d.textInput.CharLimit = 100

	// Sort models: config first, then catalog, then custom.
	sortedModels := slices.Clone(models)
	slices.SortFunc(sortedModels, func(a, b runtime.ModelChoice) int {
		return comparePickerSortKeys(modelSortKeys(a), modelSortKeys(b))
	})
	d.models = sortedModels
	d.filterModels()
	return d
}

// modelSortKeys derives the sort key tuple from a runtime.ModelChoice.
func modelSortKeys(m runtime.ModelChoice) pickerSortKeys {
	section := 0
	switch {
	case m.IsCustom:
		section = 2
	case m.IsCatalog:
		section = 1
	}
	return pickerSortKeys{
		Section:   section,
		IsCurrent: m.IsCurrent,
		IsDefault: m.IsDefault,
		Name:      m.Name,
	}
}

func (d *modelPickerDialog) Init() tea.Cmd { return textinput.Blink }

func (d *modelPickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Scrollview handles mouse scrollbar, wheel, and pgup/pgdn/home/end.
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		cmd := d.handleInputChange(msg)
		return d, cmd

	case tea.MouseClickMsg:
		if dbl, _ := d.handleListClick(msg, d.lineToModelIndex); dbl {
			cmd := d.handleSelection()
			return d, cmd
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, closeDialogCmd()
		case key.Matches(msg, d.keyMap.Up):
			d.navigate(-1, len(d.filtered), d.findSelectedLine)
			return d, nil
		case key.Matches(msg, d.keyMap.Down):
			d.navigate(+1, len(d.filtered), d.findSelectedLine)
			return d, nil
		case key.Matches(msg, d.keyMap.Enter):
			cmd := d.handleSelection()
			return d, cmd
		default:
			cmd := d.handleInputChange(msg)
			return d, cmd
		}
	}

	return d, nil
}

// handleInputChange forwards msg to the text input, re-runs the filter, and
// clears any validation error from a previous submission.
func (d *modelPickerDialog) handleInputChange(msg tea.Msg) tea.Cmd {
	return d.updateInput(msg, func() {
		d.filterModels()
		d.errMsg = ""
	})
}

// buildList constructs the list of models with section separators between
// the (config -> catalog -> custom) groups. Pass contentWidth=0 to compute
// the layout without rendering items (used by mouse hit-testing and
// findSelectedLine).
func (d *modelPickerDialog) buildList(contentWidth int) *groupedList {
	gl := newGroupedList()

	hasConfig := false
	hasCatalog := false
	for _, m := range d.filtered {
		switch {
		case m.IsCustom:
			// Custom models don't affect the catalog/config separator logic.
		case m.IsCatalog:
			hasCatalog = true
		default:
			hasConfig = true
		}
	}

	catalogSepShown := false
	customSepShown := false
	for i, model := range d.filtered {
		if model.IsCatalog && !model.IsCustom && !catalogSepShown {
			if hasConfig {
				gl.AddNonItem(RenderGroupSeparator(catalogSeparatorLabel, contentWidth))
			}
			catalogSepShown = true
		}
		if model.IsCustom && !customSepShown {
			if hasConfig || hasCatalog {
				gl.AddNonItem(RenderGroupSeparator(customSeparatorLabel, contentWidth))
			}
			customSepShown = true
		}
		gl.AddItem(d.renderModel(model, i == d.selected, contentWidth))
	}

	return gl
}

func (d *modelPickerDialog) lineToModelIndex(line int) int {
	return d.buildList(0).ItemForLine(line)
}

func (d *modelPickerDialog) findSelectedLine() int {
	return d.buildList(0).LineForItem(d.selected)
}

func (d *modelPickerDialog) handleSelection() tea.Cmd {
	query := strings.TrimSpace(d.textInput.Value())

	// If user typed something that looks like a custom model (contains /), validate and use it
	if strings.Contains(query, "/") {
		if err := validateCustomModelSpec(query); err != nil {
			d.errMsg = err.Error()
			return nil
		}
		return tea.Sequence(
			closeDialogCmd(),
			core.CmdHandler(messages.ChangeModelMsg{ModelRef: query}),
		)
	}

	// Otherwise, use the selected item from the filtered list
	if d.selected >= 0 && d.selected < len(d.filtered) {
		selected := d.filtered[d.selected]
		// If selecting the default model, send empty ref to clear the override
		modelRef := selected.Ref
		if selected.IsDefault {
			modelRef = ""
		}
		return tea.Sequence(
			closeDialogCmd(),
			core.CmdHandler(messages.ChangeModelMsg{ModelRef: modelRef}),
		)
	}

	return nil
}

// validateCustomModelSpec validates a custom model specification entered by the user.
// It checks that each provider/model pair is properly formatted and uses a supported provider.
func validateCustomModelSpec(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}

	// Handle alloy specs (comma-separated)
	parts := strings.SplitSeq(spec, ",")
	for part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		providerName, modelName, ok := strings.Cut(part, "/")
		if !ok {
			return errors.New("invalid format: expected 'provider/model'")
		}

		providerName = strings.TrimSpace(providerName)
		modelName = strings.TrimSpace(modelName)

		if providerName == "" {
			return fmt.Errorf("provider name cannot be empty (got '/%s')", modelName)
		}
		if modelName == "" {
			return fmt.Errorf("model name cannot be empty (got '%s/')", providerName)
		}

		if !provider.IsKnownProvider(providerName) {
			return fmt.Errorf("unknown provider '%s'. Supported: %s",
				providerName, strings.Join(provider.AllProviders(), ", "))
		}
	}

	return nil
}

func (d *modelPickerDialog) filterModels() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))

	// If query contains "/", show "Custom" option as well as matches
	isCustomQuery := strings.Contains(query, "/")

	d.filtered = d.filtered[:0]
	for _, model := range d.models {
		if query == "" {
			d.filtered = append(d.filtered, model)
			continue
		}

		// Match against name, provider, and model
		searchText := strings.ToLower(model.Name + " " + model.Provider + " " + model.Model)
		if strings.Contains(searchText, query) {
			d.filtered = append(d.filtered, model)
		}
	}

	// If query looks like a custom model spec and we have no exact match, show it as an option
	if isCustomQuery && len(d.filtered) == 0 {
		d.filtered = append(d.filtered, runtime.ModelChoice{
			Name: "Custom: " + query,
			Ref:  query,
		})
	}

	if d.selected >= len(d.filtered) {
		d.selected = max(0, len(d.filtered)-1)
	}
	d.scrollview.SetScrollOffset(0)
}

func (d *modelPickerDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)

	gl := d.buildList(contentWidth)
	d.updateScrollviewPosition()
	d.scrollview.SetContent(gl.Lines(), len(gl.Lines()))

	var scrollableContent string
	if len(d.filtered) == 0 {
		scrollableContent = d.renderEmptyState("No models found", contentWidth)
	} else {
		scrollableContent = d.scrollview.View()
	}

	contentBuilder := NewContent(d.regionWidth(contentWidth)).
		AddTitle("Select Model").
		AddSpace().
		AddContent(d.textInput.View())

	if d.errMsg != "" {
		contentBuilder.AddContent(styles.ErrorStyle.Render("⚠ " + d.errMsg))
	}

	content := contentBuilder.
		AddSeparator().
		AddContent(d.renderColumnHeader(contentWidth)).
		AddContent(scrollableContent).
		AddSeparator().
		AddContent(d.renderDetails(contentWidth)).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "select", "esc", "cancel").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

// pickerRowPalette is the set of styles used to render one row of the
// model list. Selection inverts the foreground/background colours of
// every visible element so the row reads as a single highlighted band.
type pickerRowPalette struct {
	name     lipgloss.Style
	desc     lipgloss.Style
	alloy    lipgloss.Style
	defBadge lipgloss.Style
	current  lipgloss.Style
	stats    lipgloss.Style
	missing  lipgloss.Style
}

func pickerRowStyles(selected bool) pickerRowPalette {
	p := pickerRowPalette{
		name:     styles.PaletteUnselectedActionStyle,
		desc:     styles.PaletteUnselectedDescStyle,
		alloy:    styles.BadgeAlloyStyle,
		defBadge: styles.BadgeDefaultStyle,
		current:  styles.BadgeCurrentStyle,
		stats:    styles.SecondaryStyle,
		missing:  styles.MutedStyle,
	}
	if !selected {
		return p
	}
	p.name = styles.PaletteSelectedActionStyle
	p.desc = styles.PaletteSelectedDescStyle
	p.alloy = p.alloy.Background(styles.MobyBlue)
	p.defBadge = p.defBadge.Background(styles.MobyBlue)
	p.current = p.current.Background(styles.MobyBlue)
	// Reuse the description style so the cells share the selection band.
	p.stats = p.desc
	p.missing = p.desc.Italic(true)
	return p
}

func (d *modelPickerDialog) renderModel(model runtime.ModelChoice, selected bool, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	p := pickerRowStyles(selected)
	nameWidth := pickerNameColWidth(maxWidth)
	return renderRowName(model, nameWidth, p) + renderRowStats(model, p)
}

// pickerNameColWidth returns the width allotted to the name column for
// a given total content width.
func pickerNameColWidth(maxWidth int) int {
	return max(1, maxWidth-pickerInputColWidth-pickerOutputColWidth-pickerContextColWidth)
}

// renderRowName renders the model name and any badges, padded to width.
func renderRowName(model runtime.ModelChoice, width int, p pickerRowPalette) string {
	badges, badgeWidth := renderRowBadges(model, p)

	nameMax := max(1, width-badgeWidth)
	displayName := model.Name
	if lipgloss.Width(displayName) > nameMax {
		displayName = toolcommon.TruncateText(displayName, nameMax)
	}

	name := p.name.Render(displayName) + badges
	padding := max(0, width-lipgloss.Width(name))
	return name + p.desc.Render(strings.Repeat(" ", padding))
}

// renderRowBadges returns the rendered badge segment plus its width.
func renderRowBadges(model runtime.ModelChoice, p pickerRowPalette) (string, int) {
	var (
		text  string
		width int
	)
	add := func(label string, style lipgloss.Style) {
		text += style.Render(label)
		width += lipgloss.Width(label)
	}
	if isAlloyModel(model) {
		add(" (alloy)", p.alloy)
	}
	switch {
	case model.IsCurrent:
		add(" (current)", p.current)
	case model.IsDefault:
		add(" (default)", p.defBadge)
	}
	return text, width
}

// renderRowStats renders the three right-aligned stats columns.
func renderRowStats(model runtime.ModelChoice, p pickerRowPalette) string {
	return renderStatsCell(formatCostPerMillion(model.InputCost), pickerInputColWidth, p, model.InputCost > 0) +
		renderStatsCell(formatCostPerMillion(model.OutputCost), pickerOutputColWidth, p, model.OutputCost > 0) +
		renderStatsCell(formatContextCell(model.ContextLimit), pickerContextColWidth, p, model.ContextLimit > 0)
}

// renderStatsCell right-aligns value in a fixed-width column. Missing
// values fade by using the palette's missing style.
func renderStatsCell(value string, width int, p pickerRowPalette, present bool) string {
	padding := max(0, width-lipgloss.Width(value))
	pad := p.stats.Render(strings.Repeat(" ", padding))
	valueStyle := p.stats
	if !present {
		valueStyle = p.missing
	}
	return pad + valueStyle.Render(value)
}

// isAlloyModel returns true when the model is an alloy spec (no
// provider, comma-separated provider/model list in Model).
func isAlloyModel(model runtime.ModelChoice) bool {
	return model.Provider == "" && strings.Contains(model.Model, ",")
}

// renderColumnHeader renders the static header above the model list,
// labelling the per-row stats columns.
func (d *modelPickerDialog) renderColumnHeader(maxWidth int) string {
	header := strings.Repeat(" ", pickerNameColWidth(maxWidth)) +
		rightAlign("Input/1M", pickerInputColWidth) +
		rightAlign("Output/1M", pickerOutputColWidth) +
		rightAlign("Context", pickerContextColWidth)
	return styles.MutedStyle.Render(header)
}

// rightAlign returns s padded with leading spaces so its rendered width
// equals width. Strings already wider than width are returned unchanged.
func rightAlign(s string, width int) string {
	padding := width - lipgloss.Width(s)
	if padding <= 0 {
		return s
	}
	return strings.Repeat(" ", padding) + s
}

// leftPad returns s padded with trailing spaces to width. Strings already
// wider than width are returned unchanged.
func leftPad(s string, width int) string {
	padding := width - lipgloss.Width(s)
	if padding <= 0 {
		return s
	}
	return s + strings.Repeat(" ", padding)
}

// formatContextCell formats a context window size for the table column.
// Returns an em-dash placeholder when the size is unknown.
func formatContextCell(tokens int) string {
	if tokens <= 0 {
		return "—"
	}
	return formatTokenCount(int64(tokens))
}

// formatCostPerMillion renders a USD-per-million-tokens price using a
// compact representation. Values <= 0 render as an em-dash; sub-cent
// values keep four decimals so they don't collapse to "$0.00";
// sub-dollar values keep two decimals; larger values trim trailing
// zeros (e.g., $3 instead of $3.00).
func formatCostPerMillion(cost float64) string {
	switch {
	case cost <= 0:
		return "—"
	case cost < 0.01:
		return fmt.Sprintf("$%.4f", cost)
	case cost < 1:
		return fmt.Sprintf("$%.2f", cost)
	}
	s := strconv.FormatFloat(cost, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return "$" + s
}

// modelReference returns the technical "provider/model" reference for a
// model choice, suitable for the details panel.
func modelReference(model runtime.ModelChoice) string {
	switch {
	case model.IsCustom:
		return model.Ref
	case isAlloyModel(model):
		return model.Model
	case model.Provider != "" && model.Model != "":
		return model.Provider + "/" + model.Model
	default:
		return model.Ref
	}
}

// detailsStyles bundles the styles used by the details panel.
type detailsStyles struct {
	label lipgloss.Style
	value lipgloss.Style
	muted lipgloss.Style
}

func newDetailsStyles() detailsStyles {
	return detailsStyles{
		label: styles.SecondaryStyle.Bold(true),
		value: styles.BaseStyle,
		muted: styles.MutedStyle.Italic(true),
	}
}

// renderDetails returns the details panel for the currently-selected
// model. It always renders pickerDetailsLines lines so the dialog has a
// stable height.
func (d *modelPickerDialog) renderDetails(width int) string {
	s := newDetailsStyles()

	var lines []string
	if d.selected >= 0 && d.selected < len(d.filtered) {
		lines = formatDetailsLines(d.filtered[d.selected], s)
	} else {
		lines = []string{s.muted.Render("No model selected")}
	}

	// Pad to a stable height so the dialog doesn't change size.
	for len(lines) < pickerDetailsLines {
		lines = append(lines, "")
	}
	// Truncate any line that would wrap.
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = toolcommon.TruncateText(l, width)
		}
	}
	return strings.Join(lines[:pickerDetailsLines], "\n")
}

// formatDetailsLines builds the four labelled rows shown for a model.
func formatDetailsLines(model runtime.ModelChoice, s detailsStyles) []string {
	row := func(label, value string) string {
		return s.label.Render(leftPad(label, pickerDetailsLabelWidth)) + value
	}

	ref := s.value.Render(modelReference(model))
	if model.Family != "" && !strings.EqualFold(model.Family, model.Provider) {
		ref += s.muted.Render(" · " + model.Family + " family")
	}

	return []string{
		row("Reference", ref),
		row("Pricing", formatPricingRow(model, s)),
		row("Limits", formatLimitsRow(model, s)),
		row("Modalities", formatModalitiesRow(model, s)),
	}
}

// formatPricingRow renders the pricing line of the details panel.
func formatPricingRow(model runtime.ModelChoice, s detailsStyles) string {
	var parts []string
	if model.InputCost > 0 || model.OutputCost > 0 {
		parts = append(parts,
			s.value.Render(formatCostPerMillion(model.InputCost)+" in"),
			s.value.Render(formatCostPerMillion(model.OutputCost)+" out"),
		)
	}
	if model.CacheReadCost > 0 {
		parts = append(parts, s.value.Render(formatCostPerMillion(model.CacheReadCost)+" cache read"))
	}
	if model.CacheWriteCost > 0 {
		parts = append(parts, s.value.Render(formatCostPerMillion(model.CacheWriteCost)+" cache write"))
	}
	if len(parts) == 0 {
		return s.muted.Render("unavailable")
	}
	parts = append(parts, s.muted.Render("per 1M tokens"))
	return strings.Join(parts, s.muted.Render(" · "))
}

// formatLimitsRow renders the limits line of the details panel.
func formatLimitsRow(model runtime.ModelChoice, s detailsStyles) string {
	var parts []string
	if model.ContextLimit > 0 {
		parts = append(parts, s.value.Render(formatTokenCount(int64(model.ContextLimit))+" context window"))
	}
	if model.OutputLimit > 0 {
		parts = append(parts, s.value.Render(formatTokenCount(model.OutputLimit)+" max output"))
	}
	if len(parts) == 0 {
		return s.muted.Render("unavailable")
	}
	return strings.Join(parts, s.muted.Render(" · "))
}

// formatModalitiesRow renders the modalities line of the details panel.
func formatModalitiesRow(model runtime.ModelChoice, s detailsStyles) string {
	if len(model.InputModalities) == 0 && len(model.OutputModalities) == 0 {
		return s.muted.Render("unavailable")
	}
	in := joinOrDash(model.InputModalities)
	out := joinOrDash(model.OutputModalities)
	return s.value.Render(in) + s.muted.Render(" → ") + s.value.Render(out)
}

// joinOrDash returns the comma-joined list, or an em-dash when empty.
func joinOrDash(parts []string) string {
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}
