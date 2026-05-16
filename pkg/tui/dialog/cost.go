package dialog

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ---------------------------------------------------------------------------
// costDialog – TUI dialog displaying session cost breakdown
// ---------------------------------------------------------------------------

type costDialog struct {
	BaseDialog

	session    *session.Session
	keyMap     costDialogKeyMap
	scrollview *scrollview.Model
}

type costDialogKeyMap struct {
	Close, Copy key.Binding
}

func NewCostDialog(sess *session.Session) Dialog {
	return &costDialog{
		session: sess,
		scrollview: scrollview.New(
			scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()),
			scrollview.WithReserveScrollbarSpace(true),
		),
		keyMap: costDialogKeyMap{
			Close: key.NewBinding(key.WithKeys("esc", "enter", "q"), key.WithHelp("Esc", "close")),
			Copy:  key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
		},
	}
}

func (d *costDialog) Init() tea.Cmd { return nil }

func (d *costDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return d, core.CmdHandler(CloseDialogMsg{})
		case key.Matches(msg, d.keyMap.Copy):
			_ = clipboard.WriteAll(d.renderPlainText())
			return d, notification.SuccessCmd("Cost details copied to clipboard.")
		}
	}
	return d, nil
}

func (d *costDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(70, 50, 80)
	maxHeight = min(d.Height()*70/100, 40)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *costDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}

func (d *costDialog) View() string {
	dialogWidth, maxHeight, contentWidth := d.dialogSize()
	content := d.renderContent(contentWidth, maxHeight)
	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}

// ---------------------------------------------------------------------------
// totalUsage – per-model / per-message cost record
// ---------------------------------------------------------------------------

type totalUsage struct {
	chat.Usage

	label  string
	model  string // model name (only set for per-message entries)
	cost   float64
	marker bool // true for visual separators (sub-session boundaries)
}

func (u *totalUsage) add(cost float64, usage *chat.Usage) {
	u.cost += cost
	u.InputTokens += usage.InputTokens
	u.OutputTokens += usage.OutputTokens
	u.CachedInputTokens += usage.CachedInputTokens
	u.CacheWriteTokens += usage.CacheWriteTokens
	u.ReasoningTokens += usage.ReasoningTokens
}

func (u *totalUsage) totalInput() int64 {
	return u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
}

func (u *totalUsage) totalTokens() int64 {
	return u.totalInput() + u.OutputTokens
}

func (u *totalUsage) isSubSessionMarker() bool { return u.marker }

// plainTextLine returns a fixed-width plain-text representation used by the
// clipboard-copy output. An optional suffix (e.g. model name) is appended.
func (u *totalUsage) plainTextLine(suffix string) string {
	line := fmt.Sprintf("%-8s  input: %-8s  output: %-8s  %s",
		formatCostPadded(u.cost),
		formatTokenCount(u.totalInput()),
		formatTokenCount(u.OutputTokens),
		u.label)
	if suffix != "" {
		line += "  " + suffix
	}
	return line
}

// ---------------------------------------------------------------------------
// stat – a label/value pair used in the Total section
// ---------------------------------------------------------------------------

type stat struct {
	label string
	value string
}

// ---------------------------------------------------------------------------
// costData – aggregated cost data for a session
// ---------------------------------------------------------------------------

type costData struct {
	total             totalUsage
	models            []totalUsage
	messages          []totalUsage
	hasPerMessageData bool
	duration          time.Duration
	messageCount      int
}

// actualMessageCount returns the number of real usage entries, excluding
// sub-session markers and other visual separators.
func (d *costData) actualMessageCount() int {
	n := 0
	for _, m := range d.messages {
		if !m.isSubSessionMarker() {
			n++
		}
	}
	return n
}

// totalStats computes the summary statistics shown in the Total section.
// Both the styled and plain-text renderers consume the same slice.
func (d *costData) totalStats() []stat {
	var stats []stat

	if d.total.ReasoningTokens > 0 {
		stats = append(stats, stat{"reasoning:", formatTokenCount(d.total.ReasoningTokens)})
	}
	if tok := d.total.totalTokens(); tok > 0 && d.total.cost > 0 {
		stats = append(stats, stat{"avg cost/1K tokens:", formatCost(d.total.cost / float64(tok) * 1000)})
	}
	if candidateIn := d.total.CachedInputTokens + d.total.InputTokens; candidateIn > 0 && d.total.CachedInputTokens > 0 {
		stats = append(stats, stat{"cache hit rate:", fmt.Sprintf("%.0f%%", float64(d.total.CachedInputTokens)/float64(candidateIn)*100)})
	}
	if actual := d.actualMessageCount(); actual > 1 && d.total.cost > 0 {
		stats = append(stats, stat{"avg cost/message:", formatCost(d.total.cost / float64(actual))})
	}
	return stats
}

// ---------------------------------------------------------------------------
// Data gathering
// ---------------------------------------------------------------------------

func (d *costDialog) gatherCostData() costData {
	var data costData
	data.duration = d.session.Duration()
	data.messageCount = d.session.MessageCount()
	modelMap := make(map[string]*totalUsage)
	msgCounter := 0

	addRecord := func(agentName, model string, cost float64, usage *chat.Usage) {
		data.hasPerMessageData = true
		data.total.add(cost, usage)

		model = cmp.Or(model, "unknown")
		if modelMap[model] == nil {
			modelMap[model] = &totalUsage{label: model}
		}
		modelMap[model].add(cost, usage)

		msgCounter++
		msgLabel := fmt.Sprintf("#%d", msgCounter)
		if agentName != "" {
			msgLabel = fmt.Sprintf("#%d [%s]", msgCounter, agentName)
		}
		data.messages = append(data.messages, totalUsage{
			label: msgLabel,
			model: model,
			cost:  cost,
			Usage: *usage,
		})
	}

	addCompactionCost := func(cost float64) {
		data.hasPerMessageData = true
		data.total.cost += cost
		data.messages = append(data.messages, totalUsage{label: "compaction", cost: cost})
	}

	addMarker := func(label string) {
		data.messages = append(data.messages, totalUsage{label: label, marker: true})
	}

	var walkSession func(sess *session.Session)
	walkSession = func(sess *session.Session) {
		for _, item := range sess.Messages {
			switch {
			case item.IsMessage():
				msg := item.Message
				if msg.Message.Role != chat.MessageRoleSystem && msg.Message.Usage != nil {
					addRecord(msg.AgentName, msg.Message.Model, msg.Message.Cost, msg.Message.Usage)
				}
			case item.IsSubSession():
				addMarker("── sub-session start ──")
				walkSession(item.SubSession)
				if subCost := item.SubSession.TotalCost(); subCost > 0 {
					addMarker(fmt.Sprintf("── sub-session end (%s) ──", formatCost(subCost)))
				} else {
					addMarker("── sub-session end ──")
				}
			}
			if item.Summary != "" && item.Cost > 0 {
				addCompactionCost(item.Cost)
			}
		}
	}

	walkSession(d.session)

	// Fall back to remote mode if no per-message data was found.
	if !data.hasPerMessageData {
		for _, record := range d.session.MessageUsageHistory {
			addRecord(record.AgentName, record.Model, record.Cost, &record.Usage)
		}
	}

	for _, m := range modelMap {
		data.models = append(data.models, *m)
	}
	slices.SortFunc(data.models, func(a, b totalUsage) int {
		return cmp.Compare(b.cost, a.cost)
	})

	// Fall back to session-level totals (e.g. past sessions without per-message data).
	if !data.hasPerMessageData {
		data.total = totalUsage{
			cost: d.session.TotalCost(),
			Usage: chat.Usage{
				InputTokens:  d.session.InputTokens,
				OutputTokens: d.session.OutputTokens,
			},
		}
	}

	return data
}

// ---------------------------------------------------------------------------
// Styled rendering (TUI view)
// ---------------------------------------------------------------------------

func (d *costDialog) renderContent(contentWidth, maxHeight int) string {
	data := d.gatherCostData()

	// Header
	header := RenderTitle("Session Cost Details", contentWidth, styles.DialogTitleStyle)
	if meta := d.headerMeta(data); meta != "" {
		header += "\n" + styles.DialogOptionsStyle.Width(contentWidth).Render(meta)
	}

	lines := []string{
		header,
		RenderSeparator(contentWidth),
		"",
		sectionStyle().Render("Total"),
		"",
		accentStyle().Render(formatCost(data.total.cost)),
		styledStat("tokens:", formatTokenCount(data.total.totalTokens())),
		d.renderInputLine(data.total, true),
		styledStat("output:", formatTokenCount(data.total.OutputTokens)),
	}
	for _, s := range data.totalStats() {
		lines = append(lines, styledStat(s.label, s.value))
	}
	lines = append(lines, "")

	// By Model
	if len(data.models) > 0 {
		lines = append(lines, sectionStyle().Render("By Model"), "")
		for _, m := range data.models {
			lines = append(lines, d.renderUsageLine(m, data.total.cost))
		}
		lines = append(lines, "")
	}

	// By Message
	if len(data.messages) > 0 {
		lines = append(lines, sectionStyle().Render("By Message"), "")
		for _, m := range data.messages {
			if m.isSubSessionMarker() {
				lines = append(lines, styles.MutedStyle.Render(m.label))
			} else {
				lines = append(lines, d.renderUsageLine(m, data.total.cost))
			}
		}
		lines = append(lines, "")
	} else if !data.hasPerMessageData && data.total.cost > 0 {
		lines = append(lines, styles.MutedStyle.Render("Per-message breakdown not available for this session."), "")
	}

	return d.applyScrolling(lines, contentWidth, maxHeight)
}

// headerMeta returns the duration/message-count line, or "" if empty.
func (d *costDialog) headerMeta(data costData) string {
	var parts []string
	if data.duration > 0 {
		parts = append(parts, "duration: "+formatDuration(data.duration))
	}
	if data.messageCount > 0 {
		parts = append(parts, fmt.Sprintf("messages: %d", data.messageCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "  •  ")
}

// styledStat renders a single "label: value" line for the Total section.
func styledStat(label, value string) string {
	return fmt.Sprintf("%s %s", labelStyle().Render(label), valueStyle().Render(value))
}

func (d *costDialog) renderInputLine(u totalUsage, showBreakdown bool) string {
	line := styledStat("input:", formatTokenCount(u.totalInput()))
	if showBreakdown && (u.CachedInputTokens > 0 || u.CacheWriteTokens > 0) {
		line += valueStyle().Render(fmt.Sprintf(" (%s new + %s cached + %s cache write)",
			formatTokenCount(u.InputTokens),
			formatTokenCount(u.CachedInputTokens),
			formatTokenCount(u.CacheWriteTokens)))
	}
	return line
}

func (d *costDialog) renderUsageLine(u totalUsage, totalCost float64) string {
	var extras []string
	if totalCost > 0 && u.cost > 0 {
		if pct := u.cost / totalCost * 100; pct >= 0.1 {
			extras = append(extras, fmt.Sprintf("%.0f%%", pct))
		}
	}
	if u.CachedInputTokens > 0 {
		extras = append(extras, "cached: "+formatTokenCount(u.CachedInputTokens))
	}

	suffix := ""
	if len(extras) > 0 {
		suffix = "  " + valueStyle().Render(strings.Join(extras, "  "))
	}
	return fmt.Sprintf("%s  %s %s  %s %s  %s%s",
		accentStyle().Render(padRight(formatCostPadded(u.cost))),
		labelStyle().Render("input:"),
		valueStyle().Render(padRight(formatTokenCount(u.totalInput()))),
		labelStyle().Render("output:"),
		valueStyle().Render(padRight(formatTokenCount(u.OutputTokens))),
		accentStyle().Render(u.label),
		suffix)
}

func (d *costDialog) applyScrolling(allLines []string, contentWidth, maxHeight int) string {
	const headerLines = 3 // title + separator + space
	const footerLines = 2 // space + help

	visibleLines := max(1, maxHeight-headerLines-footerLines-4)
	contentLines := allLines[headerLines:]

	regionWidth := contentWidth + d.scrollview.ReservedCols()
	d.scrollview.SetSize(regionWidth, visibleLines)

	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+2+headerLines)
	d.scrollview.SetContent(contentLines, len(contentLines))

	// Build final output. Use slices.Clone to avoid mutating allLines.
	parts := slices.Clone(allLines[:headerLines])
	parts = append(parts, d.scrollview.View(), "", RenderHelpKeys(regionWidth, "↑↓", "scroll", "c", "copy", "Esc", "close"))
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// ---------------------------------------------------------------------------
// Plain-text rendering (clipboard copy)
// ---------------------------------------------------------------------------

func (d *costDialog) renderPlainText() string {
	data := d.gatherCostData()
	var lines []string

	// Total section
	inputLine := "input: " + formatTokenCount(data.total.totalInput())
	if data.total.CachedInputTokens > 0 || data.total.CacheWriteTokens > 0 {
		inputLine += fmt.Sprintf(" (%s new + %s cached + %s cache write)",
			formatTokenCount(data.total.InputTokens),
			formatTokenCount(data.total.CachedInputTokens),
			formatTokenCount(data.total.CacheWriteTokens))
	}

	lines = append(lines, "Session Cost Details", "", "Total", formatCost(data.total.cost),
		"tokens: "+formatTokenCount(data.total.totalTokens()),
		inputLine, "output: "+formatTokenCount(data.total.OutputTokens))

	for _, s := range data.totalStats() {
		lines = append(lines, s.label+" "+s.value)
	}
	lines = append(lines, "")

	// By Model
	if len(data.models) > 0 {
		lines = append(lines, "By Model")
		for _, m := range data.models {
			suffix := ""
			if m.CachedInputTokens > 0 {
				suffix = "cached: " + formatTokenCount(m.CachedInputTokens)
			}
			lines = append(lines, m.plainTextLine(suffix))
		}
		lines = append(lines, "")
	}

	// By Message
	if len(data.messages) > 0 {
		lines = append(lines, "By Message")
		for _, m := range data.messages {
			if m.isSubSessionMarker() {
				lines = append(lines, m.label)
			} else {
				suffix := ""
				if m.model != "" {
					suffix = "(" + m.model + ")"
				}
				lines = append(lines, m.plainTextLine(suffix))
			}
		}
	}

	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Style helpers (functions so they pick up theme changes dynamically)
// ---------------------------------------------------------------------------

func sectionStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(styles.TextSecondary)
}

func labelStyle() lipgloss.Style { return lipgloss.NewStyle().Bold(true) }

func valueStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(styles.TextSecondary)
}

func accentStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(styles.Highlight)
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func formatCost(cost float64) string {
	if cost < 0 {
		return "-" + formatCost(-cost)
	}
	if cost < 0.0001 {
		return "$0.00"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func formatCostPadded(cost float64) string {
	if cost < 0 {
		return "-" + formatCostPadded(-cost)
	}
	if cost < 0.0001 {
		return "$0.0000"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f  ", cost)
}

func formatTokenCount(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return strconv.FormatInt(count, 10)
	}
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func padRight(s string) string {
	const width = 8
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}
