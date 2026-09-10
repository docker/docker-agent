package tui

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/board"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Layout constants. cardAt relies on these mirroring renderBoard exactly.
const (
	// boardTop is the first row of the columns area: title row + blank row.
	boardTop = 2
	// footerRows is the reserved space below the columns area.
	footerRows = 1
	// columnGap separates adjacent columns.
	columnGap = 1
	// minColumnWidth keeps columns readable on narrow terminals.
	minColumnWidth = 22
	// cardHeight is the outer height of a card: border (2) + title (2) +
	// project (1) + status (1).
	cardHeight = 6
	// columnHeaderRows is the number of content rows above the first card
	// inside a column box: title row + thick colored rule.
	columnHeaderRows = 2
)

// sanitize strips ANSI escape sequences and control characters from
// untrusted strings (agent-controlled titles, repository content) so they
// cannot inject terminal controls — clipboard writes, title changes, screen
// manipulation — when rendered. Newlines are preserved; tabs become spaces.
func sanitize(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// projectColorAt returns the accent color of the i-th configured project.
// The palette is read lazily: theme colors are only bound after ApplyTheme.
func projectColorAt(i int) color.Color {
	palette := []color.Color{
		styles.BadgeCyan, styles.BadgePurple, styles.BadgeGreen,
		styles.Info, styles.Warning, styles.Success,
	}
	return palette[((i%len(palette))+len(palette))%len(palette)]
}

// projectColor returns the accent color shared by all of a project's cards.
// Cards whose project was removed from the config hash to a stable color.
func (m *model) projectColor(name string) color.Color {
	if c, ok := m.projectColors[name]; ok {
		return c
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return projectColorAt(int(h.Sum32() % 6))
}

func (m *model) View() tea.View {
	var body string
	switch {
	case m.width <= 0 || m.height <= 0:
		body = ""
	default:
		body = lipgloss.JoinVertical(
			lipgloss.Left,
			m.renderHeader(),
			"",
			m.renderBoard(),
			m.renderFooter(),
		)
	}

	if m.dialog != nil {
		overlay := m.dialog.View(m.width, m.height)
		body = placeOverlay(body, overlay, m.width, m.height)
	} else if m.totalCards() == 0 && m.width > 0 {
		body = placeOverlay(body, m.renderWelcome(), m.width, m.height)
	}

	view := tea.NewView(body)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.BackgroundColor = styles.Background
	view.WindowTitle = m.windowTitle()
	return view
}

// windowTitle reflects the board's activity in the terminal title, like the
// main TUI does for its sessions.
func (m *model) windowTitle() string {
	busy := 0
	for _, cards := range m.cards {
		busy += busyCount(cards)
	}
	if busy == 0 {
		return "docker agent board"
	}
	return fmt.Sprintf("docker agent board — %d running", busy)
}

// placeOverlay composites a dialog over the live board, so cards and
// statuses stay visible behind the modal.
func placeOverlay(base, overlay string, width, height int) string {
	dialog := lipgloss.NewLayer(overlay)
	dialog = dialog.X(max((width-dialog.Width())/2, 0)).Y(max((height-dialog.Height())/2, 0)).Z(1)
	canvas := lipgloss.NewCanvas(width, height)
	canvas.Compose(lipgloss.NewCompositor(
		lipgloss.NewLayer(base).Z(0),
		dialog,
	))
	return canvas.Render()
}

func (m *model) renderHeader() string {
	title := styles.HighlightWhiteStyle.Render(" 🐳 Docker Agent Board")

	// On narrow terminals, show how many columns are hidden on each side.
	prefix := ""
	offset, count := m.columnWindow()
	if offset > 0 {
		prefix = "◀ " + strconv.Itoa(offset) + " · "
	}
	projects := plural(len(m.projects), "project")
	rest := " "
	if hidden := len(m.columns) - offset - count; hidden > 0 {
		rest += "· " + strconv.Itoa(hidden) + " ▶ "
	}

	// The project count is a button: underlined, and clicking it opens the
	// projects dialog (see projectsButtonAt).
	styled := styles.MutedStyle.Render(prefix) +
		styles.MutedStyle.Underline(true).Render(projects) +
		styles.MutedStyle.Render(rest)

	pad := max(m.width-lipgloss.Width(title)-lipgloss.Width(styled), 1)
	m.projStartX = lipgloss.Width(title) + pad + lipgloss.Width(prefix)
	m.projEndX = m.projStartX + lipgloss.Width(projects)
	return title + strings.Repeat(" ", pad) + styled
}

// projectsButtonAt reports whether the coordinate hits the project count in
// the top header, whose hitbox renderHeader records.
func (m *model) projectsButtonAt(x, y int) bool {
	return y == 0 && x >= m.projStartX && x < m.projEndX
}

func (m *model) totalCards() int {
	total := 0
	for _, cards := range m.cards {
		total += len(cards)
	}
	return total
}

// renderWelcome is the first-run overlay shown while the board is empty.
func (m *model) renderWelcome() string {
	key := styles.BaseStyle.Foreground(styles.BadgeCyan).Bold(true)
	lines := []string{
		styles.HighlightWhiteStyle.Render("Welcome to Docker Agent Board"),
		"",
		styles.SecondaryStyle.Render("Each card runs an agent in a tmux session, on an"),
		styles.SecondaryStyle.Render("isolated git worktree of one of your projects."),
		styles.SecondaryStyle.Render("Move a card forward to send it the column's prompt."),
		"",
		key.Render("n") + styles.MutedStyle.Render("  create your first card"),
		key.Render("p") + styles.MutedStyle.Render("  configure projects"),
		key.Render("?") + styles.MutedStyle.Render("  all key bindings"),
	}
	return styles.DialogStyle.Render(strings.Join(lines, "\n"))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// columnWindow returns the first visible column and how many fit. On
// terminals too narrow for the whole pipeline, the window slides to keep
// the selected column visible instead of clipping columns off screen.
func (m *model) columnWindow() (offset, count int) {
	n := len(m.columns)
	if n == 0 {
		return 0, 0
	}
	count = min(n, max((m.width+columnGap)/(minColumnWidth+columnGap), 1))
	m.colScroll = clamp(m.colScroll, 0, max(n-count, 0))
	m.colScroll = clamp(m.colScroll, m.selCol-count+1, m.selCol)
	return m.colScroll, count
}

// boardSize returns the columns area height and the outer column width.
func (m *model) boardSize() (boardHeight, colWidth int) {
	boardHeight = max(m.height-boardTop-footerRows, cardHeight+columnHeaderRows+2)
	_, count := m.columnWindow()
	count = max(count, 1)
	colWidth = max((m.width-(count-1)*columnGap)/count, minColumnWidth)
	return boardHeight, colWidth
}

// visibleSlots returns how many cards fit in a column.
func (m *model) visibleSlots() int {
	boardHeight, _ := m.boardSize()
	return max((boardHeight-2-columnHeaderRows)/cardHeight, 1)
}

func (m *model) renderBoard() string {
	if len(m.columns) == 0 {
		return ""
	}
	offset, count := m.columnWindow()
	boardHeight, colWidth := m.boardSize()

	cols := make([]string, 0, count*2)
	for i := offset; i < offset+count; i++ {
		if i > offset {
			cols = append(cols, strings.Repeat(" ", columnGap))
		}
		cols = append(cols, m.renderColumn(i, m.columns[i], colWidth, boardHeight))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cols...)
}

func (m *model) renderColumn(idx int, col board.Column, colWidth, boardHeight int) string {
	selected := idx == m.selCol
	cards := m.cards[col.ID]
	// A column already holding the dragged card is not a drop target: the
	// move would be a no-op. This also covers a refresh moving the card
	// under the drag — the card must never render twice in one column.
	dropTarget := m.dragging && idx == m.dragCol && !containsCard(cards, m.dragCardID)
	// The border box's Width/Height are outer sizes; the content area is two
	// cells narrower (and shorter).
	innerWidth := colWidth - 2

	// Header: emoji, name, and right-aligned card count, over a thick rule
	// that fades from orange (first column) to green (last column),
	// mirroring a card's journey through the pipeline.
	nameStyle := styles.SecondaryStyle
	if selected {
		nameStyle = styles.HighlightWhiteStyle
	}
	header := " " + columnLabel(col, nameStyle)
	// Right-aligned: a clickable + to create a card on the first column
	// (see plusButtonAt), then the card count rightmost.
	right := styles.MutedStyle.Render("("+strconv.Itoa(len(cards))+")") + " "
	if idx == 0 {
		right = styles.SuccessStyle.Bold(true).Render("+") + " " + right
	}
	header = toolcommon.TruncateText(header, innerWidth-lipgloss.Width(right)-1)
	header += strings.Repeat(" ", max(innerWidth-lipgloss.Width(header)-lipgloss.Width(right), 0)) + right
	rule := styles.BaseStyle.
		Foreground(columnHeaderColor(idx, len(m.columns))).
		Render(strings.Repeat("━", innerWidth))

	// Keep the selection visible by adjusting this column's scroll window.
	slots := m.visibleSlots()
	scroll := clamp(m.scroll[col.ID], 0, max(len(cards)-slots, 0))
	if selected {
		scroll = clamp(scroll, m.selRow-slots+1, m.selRow)
	}
	m.scroll[col.ID] = scroll

	// The drop target previews the dragged card at its insertion point: a
	// faded ghost takes the last slot and the window slides to the column's
	// tail, where a dropped card lands. The forced scroll is not persisted,
	// so a cancelled drag leaves the column's window untouched.
	ghost := ""
	if dropTarget {
		if card := m.cardByID(m.dragCardID); card != nil {
			ghost = m.renderGhost(card, innerWidth)
			slots--
			scroll = max(len(cards)-slots, 0)
		}
	}

	lines := []string{header, rule}
	// The tail scroll can hide cards above the window; without this line a
	// full column would collapse to just the ghost on short terminals.
	if ghost != "" && scroll > 0 {
		lines = append(lines, styles.MutedStyle.Render(fmt.Sprintf(" … %d more", scroll)))
	}
	end := min(scroll+slots, len(cards))
	for i := scroll; i < end; i++ {
		lines = append(lines, m.renderCard(cards[i], innerWidth, selected && i == m.selRow))
	}
	if ghost != "" {
		lines = append(lines, ghost)
	}
	switch {
	case len(cards) == 0 && selected:
		lines = append(lines, "", styles.MutedStyle.Italic(true).Render(" no cards"))
	case end < len(cards):
		lines = append(lines, styles.MutedStyle.Render(fmt.Sprintf(" … %d more", len(cards)-end)))
	}

	// Selected column: white; drag drop target: success green; others: a
	// dimmed secondary border.
	borderColor := darken(styles.BorderSecondary, 0.35)
	switch {
	case dropTarget:
		borderColor = styles.Success
	case selected:
		borderColor = styles.White
	}
	return styles.BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(colWidth).
		Height(boardHeight).
		Render(strings.Join(lines, "\n"))
}

// dragFade is how much a dragged card's colors blend toward the
// background: enough to clearly read as "in flight", still legible.
const dragFade = 0.55

// blend interpolates c1 toward c2 by the given fraction (0..1).
func blend(c1, c2 color.Color, f float64) color.Color {
	r1, g1, b1 := styles.ColorToRGB(c1)
	r2, g2, b2 := styles.ColorToRGB(c2)
	return styles.RGBToColor(r1+(r2-r1)*f, g1+(g2-g1)*f, b1+(b2-b1)*f)
}

// darken scales a color towards black by the given fraction (0..1).
func darken(c color.Color, f float64) color.Color {
	return blend(c, lipgloss.Color("#000000"), f)
}

// fade blends a color toward the theme background, approximating the
// transparency of a dragged card on terminals that cannot alpha-blend.
func fade(c color.Color) color.Color {
	return blend(c, styles.Background, dragFade)
}

// columnHeaderColor interpolates a column's rule color from the theme's
// warning color (orange-ish, first column) to its success color (green,
// last column).
func columnHeaderColor(idx, total int) color.Color {
	t := 0.0
	if total > 1 {
		t = float64(idx) / float64(total-1)
	}
	r1, g1, b1 := styles.ColorToRGB(styles.Warning)
	r2, g2, b2 := styles.ColorToRGB(styles.Success)
	return styles.RGBToColor(r1+(r2-r1)*t, g1+(g2-g1)*t, b1+(b2-b1)*t)
}

// columnLabel renders a column's emoji and name, without a stray gap for
// columns configured without an emoji.
func columnLabel(col board.Column, nameStyle lipgloss.Style) string {
	if col.Emoji == "" {
		return nameStyle.Render(sanitize(col.Name))
	}
	return sanitize(col.Emoji) + " " + nameStyle.Render(sanitize(col.Name))
}

// busyCount returns how many cards are starting or running.
func busyCount(cards []*board.Card) int {
	n := 0
	for _, c := range cards {
		if c.Status.Busy() {
			n++
		}
	}
	return n
}

func (m *model) renderCard(card *board.Card, colInnerWidth int, selected bool) string {
	textWidth := max(colInnerWidth-4, 1) // card border + padding

	// Like the web board, a card's border carries its status color; the
	// project badge keeps the project's accent so its work is still
	// recognizable at a glance.
	accent := m.projectColor(card.Project)
	titleStyle := styles.BaseStyle
	borderColor := statusColor(card.Status)
	if selected {
		titleStyle = styles.HighlightWhiteStyle
		borderColor = styles.BorderPrimary
	}

	// The card being dragged fades toward the background — a visible
	// "picked up" state while the pointer carries it to another column.
	dragged := m.dragging && card.ID == m.dragCardID
	if dragged {
		titleStyle = titleStyle.Foreground(fade(titleStyle.GetForeground()))
		accent = fade(accent)
		borderColor = fade(borderColor)
	}

	title1, title2 := splitTitle(sanitize(card.Title), textWidth)
	project := styles.BaseStyle.Foreground(accent).Render(toolcommon.TruncateText(sanitize("◆ "+card.Project), textWidth))

	content := strings.Join([]string{
		titleStyle.Render(title1),
		titleStyle.Render(title2),
		project,
		m.renderStatus(card.Status, textWidth, dragged),
	}, "\n")

	return styles.BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(colInnerWidth).
		Padding(0, 1).
		Render(content)
}

// containsCard reports whether one of the cards has the given ID.
func containsCard(cards []*board.Card, id string) bool {
	return slices.ContainsFunc(cards, func(c *board.Card) bool { return c.ID == id })
}

// renderGhost previews the dragged card in the drop-target column: a faded
// clone of the card at the column's tail, where the card lands on release.
func (m *model) renderGhost(card *board.Card, colInnerWidth int) string {
	textWidth := max(colInnerWidth-4, 1)
	style := styles.BaseStyle.Foreground(fade(styles.SecondaryStyle.GetForeground()))
	title1, title2 := splitTitle(sanitize(card.Title), textWidth)
	content := strings.Join([]string{
		style.Render(title1),
		style.Render(title2),
		style.Render(toolcommon.TruncateText(sanitize("◆ "+card.Project), textWidth)),
		"",
	}, "\n")
	return styles.BaseStyle.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(fade(styles.Success)).
		Width(colInnerWidth).
		Padding(0, 1).
		Render(content)
}

// splitTitle wraps a title onto at most two lines of the given width, word
// wrapping when possible and truncating the rest.
func splitTitle(title string, width int) (string, string) {
	words := strings.Fields(title)
	var line1 string
	for i, word := range words {
		candidate := word
		if line1 != "" {
			candidate = line1 + " " + word
		}
		if line1 != "" && lipgloss.Width(candidate) > width {
			return toolcommon.TruncateText(line1, width), toolcommon.TruncateText(strings.Join(words[i:], " "), width)
		}
		line1 = candidate
	}
	return toolcommon.TruncateText(line1, width), ""
}

// statusColor matches the web board's card tinting: starting/loading/
// attaching=blue, running=orange, paused=white, error=red, waiting=green.
func statusColor(status board.CardStatus) color.Color {
	switch status {
	case board.StatusStarting, board.StatusLoading, board.StatusAttaching:
		return styles.Info
	case board.StatusRunning:
		return styles.Warning
	case board.StatusPaused:
		return styles.White
	case board.StatusError:
		return styles.Error
	default: // waiting
		return styles.Success
	}
}

func (m *model) renderStatus(status board.CardStatus, width int, faded bool) string {
	fg := statusColor(status)
	if faded {
		fg = fade(fg)
	}
	style := styles.BaseStyle.Foreground(fg)
	spinner := spinnerFrames[m.frame%len(spinnerFrames)]
	switch status {
	case board.StatusStarting, board.StatusLoading, board.StatusAttaching:
		return style.Render(toolcommon.TruncateText(spinner+" "+string(status), width))
	case board.StatusRunning:
		return style.Render(toolcommon.TruncateText(spinner+" running", width))
	case board.StatusPaused:
		return style.Render(toolcommon.TruncateText("∥ paused", width))
	case board.StatusError:
		return style.Render(toolcommon.TruncateText("✗ failed", width))
	default: // waiting
		return style.Render(toolcommon.TruncateText("● ready", width))
	}
}

func (m *model) renderFooter() string {
	if m.dragging {
		if m.dragCol >= 0 && m.dragCol < len(m.columns) && m.dragCol != m.selCol {
			const hint = " Release to move the card to "
			name := strings.TrimSpace(m.columns[m.dragCol].Emoji + " " + m.columns[m.dragCol].Name)
			return styles.SuccessStyle.Render(hint + toolcommon.TruncateText(sanitize(name), max(m.width-len(hint)-1, 1)))
		}
		return styles.MutedStyle.Render(" Drop the card on another column to move it")
	}
	if m.flash != "" {
		style := styles.SuccessStyle
		if m.isErr {
			style = styles.ErrorStyle
		}
		return style.Render(" " + toolcommon.TruncateText(m.flash, max(m.width-2, 1)))
	}

	// Same look as the main TUI's status bar: highlighted keys with secondary
	// descriptions on the left, muted context on the right.
	hints := []string{
		"n", "new", "⏎", "attach", "d", "diff", "o", "editor", "s", "shell", "[ ] 1-9", "move",
		"x", "delete", "p", "projects", "c", "columns", "e", "prompt", "?", "help", "q", "quit",
	}
	parts := make([]string, 0, len(hints)/2)
	for i := 0; i < len(hints); i += 2 {
		parts = append(parts, styles.HighlightWhiteStyle.Render(hints[i])+" "+styles.SecondaryStyle.Render(hints[i+1]))
	}

	// Right side: where the selected card's work lives. Underlined and
	// clickable: a click copies the worktree path (see worktreeButtonAt).
	var details string
	if card := m.selectedCard(); card != nil {
		details = styles.MutedStyle.Underline(true).Render(toolcommon.TruncateText(
			sanitize(card.Agent+" · "+card.Branch), max(m.width/2, 0),
		)) + " "
	}

	left := " " + toolcommon.TruncateText(strings.Join(parts, "  "), max(m.width-lipgloss.Width(details)-2, 1))
	pad := max(m.width-lipgloss.Width(left)-lipgloss.Width(details), 1)
	m.wtStartX = lipgloss.Width(left) + pad
	m.wtEndX = m.wtStartX + lipgloss.Width(details)
	return left + strings.Repeat(" ", pad) + details
}

// worktreeButtonAt reports whether the coordinate hits the footer's card
// details, whose hitbox renderFooter records.
func (m *model) worktreeButtonAt(x, y int) bool {
	boardHeight, _ := m.boardSize()
	return m.flash == "" && y == boardTop+boardHeight && x >= m.wtStartX && x < m.wtEndX
}

// plusButtonAt reports whether the coordinate hits the first column's "+"
// button, rendered right-aligned on its header row, left of the card count.
// It mirrors the layout produced by renderColumn.
func (m *model) plusButtonAt(x, y int) bool {
	if len(m.columns) == 0 {
		return false
	}
	offset, _ := m.columnWindow()
	if offset != 0 || y != boardTop+1 {
		return false // first column scrolled out, or not the header row
	}
	_, colWidth := m.boardSize()
	// The header row ends with "+ (N) ": the + sits left of the count.
	countWidth := 2 + len(strconv.Itoa(len(m.cards[m.columns[0].ID])))
	px := colWidth - countWidth - 4
	return x >= px-1 && x <= px+1 // one cell of slack around the +
}

// columnAt maps an x/y terminal coordinate to the column under it. It
// mirrors the layout produced by renderBoard.
func (m *model) columnAt(x, y int) (int, bool) {
	if len(m.columns) == 0 {
		return 0, false
	}
	offset, count := m.columnWindow()
	boardHeight, colWidth := m.boardSize()
	if y < boardTop || y >= boardTop+boardHeight {
		return 0, false
	}
	col := offset + x/(colWidth+columnGap)
	if col >= offset+count || x%(colWidth+columnGap) >= colWidth {
		return 0, false
	}
	return col, true
}

// cardAt maps terminal coordinates to the (column, card) under them. It
// mirrors the layout produced by renderBoard — except the drop-target
// column mid-drag, whose window slides for the ghost preview. That
// divergence is safe: the only drag-time caller (handleRelease) just
// checks the hit against the dragged card, which sits in the source
// column, whose layout is unchanged.
func (m *model) cardAt(x, y int) (col, row int, ok bool) {
	col, ok = m.columnAt(x, y)
	if !ok {
		return 0, 0, false
	}

	// Rows above the first card: board top offset + column border (1) +
	// header rows.
	relY := y - boardTop - 1 - columnHeaderRows
	if relY < 0 {
		return 0, 0, false
	}
	slot := relY / cardHeight
	if slot >= m.visibleSlots() {
		return 0, 0, false
	}
	row = m.scroll[m.columns[col].ID] + slot
	if row >= len(m.cards[m.columns[col].ID]) {
		return 0, 0, false
	}
	return col, row, true
}
