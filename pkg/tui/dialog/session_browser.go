package dialog

import (
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/gitroot"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// sessionBrowserKeyMap defines key bindings for the session browser
type sessionBrowserKeyMap struct {
	Up              key.Binding
	Down            key.Binding
	Enter           key.Binding
	Escape          key.Binding
	Star            key.Binding
	FilterStar      key.Binding
	FilterWorkspace key.Binding
	CopyID          key.Binding
	Delete          key.Binding
}

// Session browser dialog dimension constants
const (
	sessionBrowserListOverhead = 12 // title(1) + space(1) + input(1) + separator(1) + separator(1) + id(1) + space(1) + help(1) + borders(2) + extra(2)
	sessionBrowserListStartY   = 6  // border(1) + padding(1) + title(1) + space(1) + input(1) + separator(1)
	sessionBrowserDirMaxLen    = 28 // max display length of a session's working dir in a list row
)

const (
	sessionBrowserHeaderWorkspace = "This workspace"
	sessionBrowserHeaderElsewhere = "Other locations"
)

// browserRow is one visual line of the session list: either a section
// header (header != "") or the session at filtered[sessionIdx].
type browserRow struct {
	header     string
	sessionIdx int
}

// workspaceMatcher reports whether a session's working directory belongs to
// the workspace the browser was opened from. Paths are normalized with
// filepath.Clean and EvalSymlinks so symlinked variants of the same
// directory (e.g. /tmp vs /private/tmp on macOS) compare equal, then
// resolved to their git repository root (worktree-aware) so sessions from
// any subdirectory or worktree of one repository group together.
// Normalization results are cached because many sessions share the same
// directory and EvalSymlinks touches the filesystem.
type workspaceMatcher struct {
	current string
	cache   map[string]string
}

func newWorkspaceMatcher(workspaceDir string) *workspaceMatcher {
	m := &workspaceMatcher{cache: make(map[string]string)}
	m.current = m.normalize(workspaceDir)
	return m
}

func (m *workspaceMatcher) enabled() bool { return m.current != "" }

func (m *workspaceMatcher) matches(dir string) bool {
	if !m.enabled() {
		return false
	}
	normalized := m.normalize(dir)
	if normalized == "" {
		return false
	}
	return workspacePathsEqual(normalized, m.current)
}

func (m *workspaceMatcher) normalize(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if cached, ok := m.cache[dir]; ok {
		return cached
	}
	normalized := filepath.Clean(dir)
	// Best effort: the recorded directory may no longer exist.
	if resolved, err := filepath.EvalSymlinks(normalized); err == nil {
		normalized = resolved
	}
	if root := gitroot.Root(normalized); root != "" {
		// The root comes from .git metadata and may itself be a symlinked
		// variant of the path (e.g. /var vs /private/var on macOS).
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		normalized = root
	}
	m.cache[dir] = normalized
	return normalized
}

// workspacePathsEqual compares normalized paths, ignoring case on platforms
// whose filesystems are typically case-insensitive.
func workspacePathsEqual(a, b string) bool {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// workspaceDisplayDir returns the directory shown in the "This workspace"
// header. Since sessions group by repository, a dir inside a git repo (or
// worktree) displays as the repository root. Symlinks are left unresolved so
// the label stays close to what the user typed, but when the raw path is not
// in a repo the symlink-resolved path is tried too, keeping the header
// consistent with the matcher's grouping key.
func workspaceDisplayDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	dir = filepath.Clean(dir)
	if root := gitroot.Root(dir); root != "" {
		return root
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		if root := gitroot.Root(resolved); root != "" {
			return root
		}
	}
	return dir
}

type sessionBrowserDialog struct {
	BaseDialog

	textInput  input.Text
	sessions   []session.Summary
	filtered   []session.Summary
	selected   int
	scrollview *scrollview.Model
	keyMap     sessionBrowserKeyMap
	openedAt   time.Time // when dialog was opened, for stable time display
	starFilter int       // 0 = all, 1 = starred only, 2 = unstarred only

	// Workspace grouping state
	workspace       *workspaceMatcher
	workspaceDir    string // workspace root shown in the section header
	workspaceFilter int    // 0 = all, 1 = this workspace only, 2 = other locations only
	rows            []browserRow
	rowForSession   []int // filtered index -> row index
	workspaceCount  int
	elsewhereCount  int

	// Double-click detection
	lastClickTime  time.Time
	lastClickIndex int
}

// NewSessionBrowserDialog creates a new session browser dialog.
// workspaceDir is the directory of the active session; sessions started
// there are grouped first and can be filtered with the workspace filter.
func NewSessionBrowserDialog(sessions []session.Summary, workspaceDir string) Dialog {
	ti := input.NewText()
	ti.Placeholder = "Type to search sessions…"
	ti.Focus()
	ti.CharLimit = 100
	ti.SetWidth(50)

	// Filter out empty sessions (sessions without a title)
	nonEmptySessions := make([]session.Summary, 0, len(sessions))
	for _, s := range sessions {
		if s.Title != "" {
			nonEmptySessions = append(nonEmptySessions, s)
		}
	}

	d := &sessionBrowserDialog{
		textInput:    ti,
		sessions:     nonEmptySessions,
		scrollview:   scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
		workspace:    newWorkspaceMatcher(workspaceDir),
		workspaceDir: workspaceDisplayDir(workspaceDir),
		keyMap: sessionBrowserKeyMap{
			Up:              key.NewBinding(key.WithKeys("up", "ctrl+k")),
			Down:            key.NewBinding(key.WithKeys("down", "ctrl+j")),
			Enter:           key.NewBinding(key.WithKeys("enter")),
			Escape:          key.NewBinding(key.WithKeys("esc")),
			Star:            key.NewBinding(key.WithKeys("ctrl+s")),
			FilterStar:      key.NewBinding(key.WithKeys("ctrl+f")),
			FilterWorkspace: key.NewBinding(key.WithKeys("ctrl+g")),
			CopyID:          key.NewBinding(key.WithKeys("ctrl+y")),
			Delete:          key.NewBinding(key.WithKeys("ctrl+d")),
		},
		openedAt: time.Now(),
	}
	// Initialize filtered list
	d.filterSessions()
	return d
}

func (d *sessionBrowserDialog) Init() tea.Cmd {
	return nil
}

func (d *sessionBrowserDialog) CursorInput() input.Cursor {
	return &d.textInput
}

func (d *sessionBrowserDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Scrollview handles mouse click/motion/release, wheel, and pgup/pgdn/home/end
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		var cmd tea.Cmd
		d.textInput, cmd = d.textInput.Update(msg)
		d.filterSessions()
		return d, cmd

	case tea.MouseClickMsg:
		// Scrollbar clicks already handled above; this handles list item clicks
		if msg.Button == tea.MouseLeft {
			if idx := d.mouseYToSessionIndex(msg.Y); idx >= 0 {
				now := time.Now()
				if idx == d.lastClickIndex && now.Sub(d.lastClickTime) < styles.DoubleClickThreshold {
					d.selected = idx
					d.lastClickTime = time.Time{}
					return d, tea.Sequence(
						core.CmdHandler(CloseDialogMsg{}),
						core.CmdHandler(messages.LoadSessionMsg{SessionID: d.filtered[d.selected].ID}),
					)
				}
				d.selected = idx
				d.lastClickTime = now
				d.lastClickIndex = idx
			}
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, core.CmdHandler(CloseDialogMsg{})

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
			if d.selected >= 0 && d.selected < len(d.filtered) {
				return d, tea.Sequence(
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(messages.LoadSessionMsg{SessionID: d.filtered[d.selected].ID}),
				)
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Star):
			if d.selected >= 0 && d.selected < len(d.filtered) {
				sessionID := d.filtered[d.selected].ID
				for i := range d.sessions {
					if d.sessions[i].ID == sessionID {
						d.sessions[i].Starred = !d.sessions[i].Starred
						break
					}
				}
				for i := range d.filtered {
					if d.filtered[i].ID == sessionID {
						d.filtered[i].Starred = !d.filtered[i].Starred
						break
					}
				}
				return d, core.CmdHandler(messages.ToggleSessionStarMsg{SessionID: sessionID})
			}
			return d, nil

		case key.Matches(msg, d.keyMap.FilterStar):
			d.starFilter = (d.starFilter + 1) % 3
			d.filterSessions()
			return d, nil

		case key.Matches(msg, d.keyMap.FilterWorkspace):
			if d.workspace.enabled() {
				d.workspaceFilter = (d.workspaceFilter + 1) % 3
				d.filterSessions()
			}
			return d, nil

		case key.Matches(msg, d.keyMap.CopyID):
			if d.selected >= 0 && d.selected < len(d.filtered) {
				sessionID := d.filtered[d.selected].ID
				_ = clipboard.WriteAll(sessionID)
				return d, notification.SuccessCmd("Session ID copied to clipboard.")
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Delete):
			if d.selected >= 0 && d.selected < len(d.filtered) {
				sessionID := d.filtered[d.selected].ID
				d.sessions = slices.DeleteFunc(d.sessions, func(s session.Summary) bool {
					return s.ID == sessionID
				})
				d.filterSessions()
				return d, core.CmdHandler(messages.DeleteSessionMsg{SessionID: sessionID})
			}
			return d, nil

		default:
			var cmd tea.Cmd
			d.textInput, cmd = d.textInput.Update(msg)
			d.filterSessions()
			return d, cmd
		}
	}

	return d, nil
}

func (d *sessionBrowserDialog) filterSessions() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))
	// IDs are matched with dashes stripped so partial UUIDs paste-match.
	idQuery := strings.ReplaceAll(query, "-", "")

	var workspace, elsewhere []session.Summary
	for _, sess := range d.sessions {
		switch d.starFilter {
		case 1:
			if !sess.Starred {
				continue
			}
		case 2:
			if sess.Starred {
				continue
			}
		}

		inWorkspace := d.workspace.matches(sess.WorkingDir)
		switch d.workspaceFilter {
		case 1:
			if !inWorkspace {
				continue
			}
		case 2:
			if inWorkspace {
				continue
			}
		}

		if query != "" {
			title := sess.Title
			if title == "" {
				title = "Untitled"
			}
			if !strings.Contains(strings.ToLower(title), query) && !matchesSessionID(sess.ID, idQuery) {
				continue
			}
		}

		if inWorkspace {
			workspace = append(workspace, sess)
		} else {
			elsewhere = append(elsewhere, sess)
		}
	}

	// Current-workspace sessions come first; each group keeps its recency order.
	d.workspaceCount = len(workspace)
	d.elsewhereCount = len(elsewhere)
	d.filtered = append(workspace, elsewhere...)
	d.rebuildRows()

	if d.selected >= len(d.filtered) {
		d.selected = max(0, len(d.filtered)-1)
	}
	// Keep the scrollview's totalHeight in sync so EnsureLineVisible and the
	// scrollbar clamp correctly even before View() runs.
	d.scrollview.SetContent(nil, len(d.rows))
	d.scrollview.SetScrollOffset(0)
}

// matchesSessionID reports whether the session ID contains the query,
// case-insensitively and ignoring "-" characters on both sides.
func matchesSessionID(id, idQuery string) bool {
	if idQuery == "" {
		return false
	}
	id = strings.ReplaceAll(strings.ToLower(id), "-", "")
	return strings.Contains(id, idQuery)
}

// rebuildRows lays out the filtered sessions as visual rows. Section headers
// are added only in the ungrouped-filter view, when the workspace is known
// and both groups are non-empty; otherwise the list stays flat.
func (d *sessionBrowserDialog) rebuildRows() {
	d.rows = d.rows[:0]
	d.rowForSession = d.rowForSession[:0]

	showHeaders := d.workspaceFilter == 0 && d.workspace.enabled() &&
		d.workspaceCount > 0 && d.elsewhereCount > 0

	appendSession := func(i int) {
		d.rowForSession = append(d.rowForSession, len(d.rows))
		d.rows = append(d.rows, browserRow{sessionIdx: i})
	}

	if !showHeaders {
		for i := range d.filtered {
			appendSession(i)
		}
		return
	}

	d.rows = append(d.rows, browserRow{header: sessionBrowserHeaderWorkspace, sessionIdx: -1})
	for i := range d.workspaceCount {
		appendSession(i)
	}
	d.rows = append(d.rows, browserRow{header: sessionBrowserHeaderElsewhere, sessionIdx: -1})
	for i := d.workspaceCount; i < len(d.filtered); i++ {
		appendSession(i)
	}
}

// ensureSelectedVisible scrolls so the selected session is on screen. When
// the row just above it is a section header, the header is kept visible too
// so reaching the first session of a group reveals its title.
func (d *sessionBrowserDialog) ensureSelectedVisible() {
	if d.selected < 0 || d.selected >= len(d.rowForSession) {
		return
	}
	row := d.rowForSession[d.selected]
	start := row
	if row > 0 && d.rows[row-1].header != "" {
		start = row - 1
	}
	d.scrollview.EnsureRangeVisible(start, row)
}

// mouseYToSessionIndex converts a mouse Y position to a session index in the filtered list.
// Returns -1 if the position is not on a session (outside the list or on a section header).
func (d *sessionBrowserDialog) mouseYToSessionIndex(y int) int {
	dialogRow, _ := d.Position()
	visLines := d.scrollview.VisibleHeight()
	listStartY := dialogRow + sessionBrowserListStartY

	if y < listStartY || y >= listStartY+visLines {
		return -1
	}
	lineInView := y - listStartY
	rowIdx := d.scrollview.ScrollOffset() + lineInView
	if rowIdx < 0 || rowIdx >= len(d.rows) {
		return -1
	}
	return d.rows[rowIdx].sessionIdx
}

func (d *sessionBrowserDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(85, 60, 120)
	maxHeight = min(d.Height()*70/100, 30)
	contentWidth = dialogWidth - 6 - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *sessionBrowserDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)

	regionWidth := contentWidth + d.scrollview.ReservedCols()
	visibleLines := d.scrollview.VisibleHeight()

	// Set scrollview position for mouse hit-testing (auto-computed from dialog position)
	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+sessionBrowserListStartY)

	// Tell the scrollview the total content height; pass nil for lines
	// because we render only the visible window below. Rendering every row
	// on every keystroke is the dominant cost when there are many sessions.
	// The follow-up SetScrollOffset call re-clamps the offset against the
	// (possibly shrunk) total — it is intentionally not a no-op.
	total := len(d.rows)
	d.scrollview.SetContent(nil, total)
	d.scrollview.SetScrollOffset(d.scrollview.ScrollOffset())

	var scrollableContent string
	if total == 0 {
		// Empty state: render manually so "No sessions found" is centered
		emptyLines := []string{"", styles.DialogContentStyle.
			Italic(true).Align(lipgloss.Center).Width(contentWidth).
			Render("No sessions found")}
		for len(emptyLines) < visibleLines {
			emptyLines = append(emptyLines, "")
		}
		scrollableContent = d.scrollview.ViewWithLines(emptyLines)
	} else {
		offset := d.scrollview.ScrollOffset()
		end := min(offset+visibleLines, total)
		windowLines := make([]string, 0, end-offset)
		for i := offset; i < end; i++ {
			row := d.rows[i]
			if row.header != "" {
				windowLines = append(windowLines, d.renderSectionHeader(row.header, contentWidth))
			} else {
				windowLines = append(windowLines, d.renderSession(d.filtered[row.sessionIdx], row.sessionIdx == d.selected, contentWidth))
			}
		}
		scrollableContent = d.scrollview.ViewWithLines(windowLines)
	}

	// Build title with session count and optional filter indicators.
	// Show "filtered/total" when a search or filter reduces the list.
	var countLabel string
	if len(d.filtered) == len(d.sessions) {
		countLabel = strconv.Itoa(len(d.sessions))
	} else {
		countLabel = fmt.Sprintf("%d/%d", len(d.filtered), len(d.sessions))
	}
	title := fmt.Sprintf("Sessions (%s)", countLabel)
	switch d.starFilter {
	case 1:
		title += " " + styles.StarredStyle.Render("★")
	case 2:
		title += " " + styles.UnstarredStyle.Render("☆")
	}
	switch d.workspaceFilter {
	case 1:
		title += " " + styles.StarredStyle.Render("⌂")
	case 2:
		title += " " + styles.UnstarredStyle.Render("⌂")
	}

	var filterDesc string
	switch d.starFilter {
	case 0:
		filterDesc = "all"
	case 1:
		filterDesc = "★ only"
	case 2:
		filterDesc = "☆ only"
	}

	var idFooter string
	if d.selected >= 0 && d.selected < len(d.filtered) {
		idFooter = styles.MutedStyle.Render("ID: ") + styles.SecondaryStyle.Render(d.filtered[d.selected].ID)
	}

	secondHelpLine := []string{"enter", "load"}
	if d.workspace.enabled() {
		var workspaceDesc string
		switch d.workspaceFilter {
		case 0:
			workspaceDesc = "all dirs"
		case 1:
			workspaceDesc = "this dir"
		case 2:
			workspaceDesc = "other dirs"
		}
		secondHelpLine = append(secondHelpLine, "ctrl+g", workspaceDesc)
	}
	secondHelpLine = append(secondHelpLine, "esc", "close")

	content := NewContent(regionWidth).
		AddTitle(title).
		AddSpace().
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(scrollableContent).
		AddSeparator().
		AddContent(idFooter).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "ctrl+s", "star", "ctrl+f", filterDesc, "ctrl+y", "copy id", "ctrl+d", "delete").
		AddHelpKeys(secondHelpLine...).
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

// SetSize sets the dialog dimensions and configures the scrollview region.
func (d *sessionBrowserDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	_, maxHeight, contentWidth := d.dialogSize()
	regionWidth := contentWidth + d.scrollview.ReservedCols()
	visibleLines := max(1, maxHeight-sessionBrowserListOverhead)
	d.scrollview.SetSize(regionWidth, visibleLines)
	return cmd
}

func (d *sessionBrowserDialog) renderSession(sess session.Summary, selected bool, maxWidth int) string {
	titleStyle, timeStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	if selected {
		titleStyle, timeStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
	}

	title := sess.Title
	if title == "" {
		title = "Untitled"
	}

	suffix := fmt.Sprintf(" • (%d msg) • %s", sess.NumMessages, d.timeAgo(sess.CreatedAt))
	if dir := d.sessionDirLabel(sess); dir != "" {
		suffix += " • " + dir
	}

	starWidth := 3
	maxTitleLen := max(1, maxWidth-lipgloss.Width(suffix)-starWidth)
	if r := []rune(title); len(r) > maxTitleLen {
		title = string(r[:maxTitleLen-1]) + "…"
	}

	return styles.StarIndicator(sess.Starred) + titleStyle.Render(title) + timeStyle.Render(suffix)
}

// sessionDirLabel returns the abbreviated directory shown next to sessions
// that belong to a different workspace than the current one. Sessions from
// the current workspace need no label, and sessions with no recorded
// directory (pre-migration or API-created) have none to show.
func (d *sessionBrowserDialog) sessionDirLabel(sess session.Summary) string {
	dir := strings.TrimSpace(sess.WorkingDir)
	if dir == "" || d.workspace.matches(dir) {
		return ""
	}
	return truncatePath(pathx.ShortenHome(dir), sessionBrowserDirMaxLen)
}

// renderSectionHeader renders a workspace group header. The current-workspace
// header includes the abbreviated directory when it fits.
func (d *sessionBrowserDialog) renderSectionHeader(header string, maxWidth int) string {
	count := d.elsewhereCount
	if header == sessionBrowserHeaderWorkspace {
		count = d.workspaceCount
	}
	label := fmt.Sprintf("%s (%d)", header, count)

	var dir string
	if header == sessionBrowserHeaderWorkspace && d.workspaceDir != "" {
		available := maxWidth - lipgloss.Width(label) - 3 // " · " separator
		if available >= 8 {
			dir = " · " + truncatePath(pathx.ShortenHome(d.workspaceDir), available)
		}
	}

	return styles.MutedStyle.Bold(true).Render(label) + styles.MutedStyle.Render(dir)
}

func (d *sessionBrowserDialog) timeAgo(t time.Time) string {
	elapsed := d.openedAt.Sub(t)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
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

func (d *sessionBrowserDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}
