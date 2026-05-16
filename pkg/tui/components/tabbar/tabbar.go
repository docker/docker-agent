// Package tabbar provides a horizontal tab bar for the TUI.
package tabbar

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// tabBarHeight is the number of terminal rows the tab bar occupies.
	tabBarHeight = 1
	// fallbackWidth is used when the terminal width is unknown or zero.
	fallbackWidth = 200
	// scrollArrowWidth is the visual width of a scroll indicator.
	scrollArrowWidth = 2
	// scrollLeftText is the left scroll arrow content.
	scrollLeftText = "◀ "
	// scrollRightText is the right scroll arrow content.
	scrollRightText = " ▶"
	// plusButtonWidth is the visual width of the "+" button.
	plusButtonWidth = 3
	// plusButtonText is the "+" button content.
	plusButtonText = " + "
	// noTab is the sentinel value for click zones that don't map to a tab.
	noTab = -1

	// dropIndicator is the vertical bar shown between tabs during drag-and-drop.
	dropIndicator = "┃"

	// dragThreshold is the minimum mouse movement (in columns) to distinguish
	// a drag from a click.
	dragThreshold = 3

	// dropIndicatorWidth is the rendered width of dropIndicator.
	dropIndicatorWidth = 1
)

// clickZone records where a clickable element is on the tab bar.
type clickZone struct {
	startX, endX  int
	tabIdx        int // index into tabs (noTab for non-tab zones)
	isPlus        bool
	isClose       bool
	isScrollLeft  bool
	isScrollRight bool
}

// dragState tracks an in-progress tab drag-and-drop operation.
type dragState struct {
	active  bool
	dragIdx int // index of the tab being dragged
	dropIdx int // insertion index (-1 = no valid drop target)
	startX  int // mouse X at drag start
}

// isNoOp returns true when the drop would leave the tab in its original position.
func (d dragState) isNoOp() bool {
	return d.dropIdx == noTab || d.dropIdx == d.dragIdx || d.dropIdx == d.dragIdx+1
}

// tabBound records the original (non-shifted) left/right edges of a tab.
type tabBound struct {
	start, end int
}

// TabBar renders a horizontal bar of session tabs with click and keyboard support.
type TabBar struct {
	tabs      []messages.TabInfo
	activeIdx int
	width     int
	keyMap    KeyMap

	scrollOffset int
	zones        []clickZone

	// dragBounds stores tab positions as-if no drop indicator were rendered,
	// so midpoint calculations during drag don't jitter when the indicator
	// shifts zones by 1 column.
	dragBounds []tabBound

	// maxTitleLen is the maximum display length for tab titles.
	// Configurable via user settings; defaults to the constant in tab.go.
	maxTitleLen int

	// lastEnsuredIdx tracks which active tab was last scrolled-to by
	// ensureActiveVisible. This prevents View() from overriding manual
	// scroll actions — ensureActiveVisible only runs when the active tab
	// actually changes.
	lastEnsuredIdx int

	// animFrame is the current animation frame from the global coordinator,
	// used to cycle the running indicator on active streaming tabs.
	animFrame int

	drag dragState
}

// KeyMap defines key bindings for the tab bar.
type KeyMap struct {
	NewTab   key.Binding
	NextTab  key.Binding
	PrevTab  key.Binding
	CloseTab key.Binding
}

// DefaultKeyMap returns the default tab bar key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		NewTab: key.NewBinding(
			key.WithKeys("ctrl+t"),
			key.WithHelp("Ctrl+t", "new tab"),
		),
		NextTab: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("Ctrl+n", "next tab"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("Ctrl+p", "prev tab"),
		),
		CloseTab: key.NewBinding(
			key.WithKeys("ctrl+w"),
			key.WithHelp("Ctrl+W", "close tab"),
		),
	}
}

// New creates a new tab bar with the given max title length.
// If maxTitleLen is <= 0, the default (20) is used.
func New(maxTitleLen int) *TabBar {
	if maxTitleLen <= 0 {
		maxTitleLen = defaultMaxTitleLen
	}
	return &TabBar{
		keyMap:         DefaultKeyMap(),
		maxTitleLen:    maxTitleLen,
		lastEnsuredIdx: noTab,
		drag:           dragState{dropIdx: noTab},
	}
}

// SetCloseTabEnabled enables or disables the close-tab key binding.
// When disabled, Ctrl+W passes through to the editor for word deletion.
func (t *TabBar) SetCloseTabEnabled(v bool) {
	t.keyMap.CloseTab.SetEnabled(v)
}

// SetWidth sets the available width for the tab bar.
func (t *TabBar) SetWidth(width int) {
	t.width = width
}

// SetTabs updates the list of tabs and active index.
func (t *TabBar) SetTabs(tabs []messages.TabInfo, activeIdx int) {
	if activeIdx != t.activeIdx {
		// Active tab changed — force ensureActiveVisible on next View.
		t.lastEnsuredIdx = noTab
	}
	t.tabs = tabs
	t.activeIdx = activeIdx
	t.clampScroll()
}

// SetAnimFrame updates the animation frame for the running indicator.
func (t *TabBar) SetAnimFrame(frame int) {
	t.animFrame = frame
}

// Height returns the height of the tab bar.
// Returns 0 when there is a single tab (no bar needed).
func (t *TabBar) Height() int {
	if len(t.tabs) <= 1 {
		return 0
	}
	return tabBarHeight
}

// IsDragging returns true when a tab drag is in progress.
func (t *TabBar) IsDragging() bool {
	return t.drag.active
}

// Bindings returns consolidated key bindings for the help bar.
func (t *TabBar) Bindings() []key.Binding {
	return []key.Binding{
		key.NewBinding(
			key.WithKeys("ctrl+t", "ctrl+w"),
			key.WithHelp("Ctrl+t/w", "new/close tab"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+p", "ctrl+n"),
			key.WithHelp("Ctrl+p/n", "prev/next tab"),
		),
	}
}

// Update handles messages and returns commands.
func (t *TabBar) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, t.keyMap.NewTab):
			return core.CmdHandler(messages.SpawnSessionMsg{})

		case key.Matches(msg, t.keyMap.NextTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			nextIdx := (t.activeIdx + 1) % len(t.tabs)
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[nextIdx].SessionID})

		case key.Matches(msg, t.keyMap.PrevTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			prevIdx := t.activeIdx - 1
			if prevIdx < 0 {
				prevIdx = len(t.tabs) - 1
			}
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[prevIdx].SessionID})

		case key.Matches(msg, t.keyMap.CloseTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[t.activeIdx].SessionID})
		}

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return t.handleLeftClickDown(msg.X)
		}
		if msg.Button == tea.MouseMiddle {
			return t.handleMiddleClick(msg.X)
		}
		return t.handleClick(msg.X)

	case tea.MouseMotionMsg:
		return t.handleMouseMotion(msg.X)

	case tea.MouseReleaseMsg:
		return t.handleMouseRelease(msg.X)
	}

	return nil
}

// handleLeftClickDown initiates a drag or handles a normal click.
func (t *TabBar) handleLeftClickDown(x int) tea.Cmd {
	for _, z := range t.zones {
		if x < z.startX || x >= z.endX {
			continue
		}
		if z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && !z.isClose {
			t.drag = dragState{
				active:  true,
				dragIdx: z.tabIdx,
				dropIdx: noTab,
				startX:  x,
			}
			return nil
		}
		break
	}
	return t.handleClick(x)
}

// handleMouseMotion updates the drop target during a drag.
func (t *TabBar) handleMouseMotion(x int) tea.Cmd {
	if !t.drag.active {
		return nil
	}

	t.drag.dropIdx = t.dropIndexForX(x)
	return nil
}

// handleMouseRelease completes a drag or falls back to a click.
func (t *TabBar) handleMouseRelease(x int) tea.Cmd {
	if !t.drag.active {
		return nil
	}

	from := t.drag.dragIdx
	to := t.drag.dropIdx
	startX := t.drag.startX
	noop := t.drag.isNoOp()
	t.drag = dragState{dropIdx: noTab}

	// If we never moved far enough to get a valid drop, treat as a click.
	if noop {
		if abs(x-startX) < dragThreshold {
			return t.handleClick(x)
		}
		return nil
	}

	// Convert insertion index to final position after removal.
	finalTo := to
	if to > from {
		finalTo--
	}

	return core.CmdHandler(messages.ReorderTabMsg{FromIdx: from, ToIdx: finalTo})
}

// dropIndexForX determines the insertion index for the current mouse X
// position by checking which tab's midpoint the cursor has passed.
// Uses the stable (indicator-free) bounds to avoid jitter.
func (t *TabBar) dropIndexForX(x int) int {
	for i, b := range t.dragBounds {
		// Map local dragBounds index back to global tab index.
		tabIdx := t.scrollOffset + i
		mid := (b.start + b.end) / 2
		if x < mid {
			return tabIdx
		}
	}

	if len(t.tabs) > 0 {
		return t.scrollOffset + len(t.dragBounds)
	}
	return noTab
}

// handleMiddleClick closes the tab under the cursor on middle-click.
func (t *TabBar) handleMiddleClick(x int) tea.Cmd {
	for _, z := range t.zones {
		if x < z.startX || x >= z.endX {
			continue
		}
		if z.tabIdx >= 0 && z.tabIdx < len(t.tabs) {
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		}
		return nil
	}
	return nil
}

// handleClick uses the click zones computed during the last View() call.
func (t *TabBar) handleClick(x int) tea.Cmd {
	for _, z := range t.zones {
		if x < z.startX || x >= z.endX {
			continue
		}

		switch {
		case z.isScrollLeft:
			t.scrollOffset = max(0, t.scrollOffset-1)
			return nil
		case z.isScrollRight:
			t.scrollOffset = min(len(t.tabs)-1, t.scrollOffset+1)
			return nil
		case z.isPlus:
			return core.CmdHandler(messages.SpawnSessionMsg{})
		case z.isClose && z.tabIdx >= 0 && z.tabIdx < len(t.tabs):
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		case z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && z.tabIdx != t.activeIdx:
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		}
		return nil
	}
	return nil
}

// View renders the tab bar as a single line: tab tab tab  +
// Returns empty string when there is a single tab.
func (t *TabBar) View() string {
	if len(t.tabs) <= 1 {
		return ""
	}

	// Reset zones (reuse backing array if available).
	if t.zones != nil {
		t.zones = t.zones[:0]
	}

	availWidth := t.width
	if availWidth <= 0 {
		availWidth = fallbackWidth
	}

	// Pre-render all tabs.
	allTabs := make([]Tab, len(t.tabs))
	totalWidth := 0
	for i, info := range t.tabs {
		role := dragRoleNone
		if t.drag.active {
			if i == t.drag.dragIdx {
				role = dragRoleSource
			} else {
				role = dragRoleBystander
			}
		}
		allTabs[i] = renderTab(info, t.maxTitleLen, t.animFrame, role)
		totalWidth += allTabs[i].Width()
	}
	totalWidth += plusButtonWidth

	needsScroll := totalWidth > availWidth

	if !needsScroll {
		// Reset persistent scroll position once everything fits, so the next
		// time the bar overflows again the user starts from the leftmost
		// tab. The mutation lives here, rather than in SetTabs/SetWidth,
		// because only the rendering pass knows the actual tab widths.
		t.scrollOffset = 0 //rubocop:disable Lint/TUIViewPurity // intentional reset; see comment above
	} else if t.activeIdx != t.lastEnsuredIdx {
		// Only auto-scroll when the active tab changes (e.g. tab switch),
		// not on every render — otherwise manual scroll via arrows is undone.
		t.ensureActiveVisible(allTabs, availWidth)
		t.lastEnsuredIdx = t.activeIdx //rubocop:disable Lint/TUIViewPurity // memo to avoid re-running ensureActiveVisible on every render
	}

	// Compute "+" and arrow colors dynamically from the terminal background.
	chromeFg := styles.MutedContrastFg(styles.Background)
	plusStyle := lipgloss.NewStyle().Foreground(chromeFg)
	arrowStyle := lipgloss.NewStyle().Foreground(chromeFg)
	// Attention arrow style: warning-colored and bold so off-screen attention tabs are obvious.
	attnArrowStyle := lipgloss.NewStyle().Foreground(styles.EnsureContrast(styles.Warning, styles.Background)).Bold(true)

	var line strings.Builder
	var cursor int

	// Left scroll arrow — highlight if any off-screen tab to the left needs attention.
	if needsScroll && t.scrollOffset > 0 {
		style := arrowStyle
		if t.hasAttentionInRange(0, t.scrollOffset) {
			style = attnArrowStyle
		}
		arrow := style.Render(scrollLeftText)
		line.WriteString(arrow)
		t.zones = append(t.zones, clickZone{
			startX: cursor, endX: cursor + scrollArrowWidth,
			tabIdx: noTab, isScrollLeft: true,
		})
		cursor += scrollArrowWidth
	}

	// Compute the drop indicator once if dragging.
	var dropLine string
	visualDrop := noTab
	if t.drag.active && !t.drag.isNoOp() {
		dropFg := styles.EnsureContrast(styles.TabAccentFg, styles.Background)
		dropLine = lipgloss.NewStyle().Foreground(dropFg).Render(dropIndicator)
		visualDrop = t.drag.dropIdx
	}

	// Visible tabs — each tab starts with its own accent bar, no divider needed.
	// We compute stable (indicator-free) tab bounds for drag hit testing
	// alongside the actual rendered zones which may be shifted by the indicator.
	if t.dragBounds != nil {
		t.dragBounds = t.dragBounds[:0]
	}
	stableCursor := cursor // tracks position without indicator offset
	lastVisibleIdx := t.scrollOffset - 1
	for i := t.scrollOffset; i < len(allTabs); i++ {
		tab := allTabs[i]
		tabW := tab.Width()

		// Reserve space for possible right arrow and "+" button.
		rightReserve := plusButtonWidth
		if needsScroll && i < len(allTabs)-1 {
			rightReserve += scrollArrowWidth
		}

		// Account for drop indicator width when checking available space.
		extra := 0
		if visualDrop >= 0 && i >= visualDrop {
			extra = dropIndicatorWidth
		}

		if cursor+tabW+extra+rightReserve > availWidth && i > t.scrollOffset {
			break
		}

		// Record stable (indicator-free) bounds for drag midpoint calculation.
		t.dragBounds = append(t.dragBounds, tabBound{start: stableCursor, end: stableCursor + tabW})
		stableCursor += tabW

		// Insert the drop indicator before this tab if this is the visual drop position.
		if visualDrop == i {
			line.WriteString(dropLine)
			cursor += dropIndicatorWidth
		}

		// Register click zones: main area + close area.
		mainEnd := cursor + tab.MainZoneEnd()
		t.zones = append(t.zones,
			clickZone{startX: cursor, endX: mainEnd, tabIdx: i},
			clickZone{startX: mainEnd, endX: cursor + tabW, tabIdx: i, isClose: true},
		)

		line.WriteString(tab.View())
		cursor += tabW
		lastVisibleIdx = i
	}

	// Drop indicator after the last visible tab.
	if visualDrop == lastVisibleIdx+1 {
		line.WriteString(dropLine)
		cursor += dropIndicatorWidth
	}

	// Right scroll arrow — highlight if any off-screen tab to the right needs attention.
	if needsScroll && lastVisibleIdx < len(allTabs)-1 {
		style := arrowStyle
		if t.hasAttentionInRange(lastVisibleIdx+1, len(t.tabs)) {
			style = attnArrowStyle
		}
		arrow := style.Render(scrollRightText)
		line.WriteString(arrow)
		t.zones = append(t.zones, clickZone{
			startX: cursor, endX: cursor + scrollArrowWidth,
			tabIdx: noTab, isScrollRight: true,
		})
		cursor += scrollArrowWidth
	}

	// "+" button.
	plus := plusStyle.Render(plusButtonText)
	line.WriteString(plus)
	t.zones = append(t.zones, clickZone{
		startX: cursor, endX: cursor + plusButtonWidth,
		tabIdx: noTab, isPlus: true,
	})

	return line.String()
}

// ensureActiveVisible adjusts scrollOffset so the active tab is visible.
func (t *TabBar) ensureActiveVisible(tabs []Tab, availWidth int) {
	if t.activeIdx < t.scrollOffset {
		t.scrollOffset = t.activeIdx
	}

	for t.scrollOffset < t.activeIdx {
		usedWidth := 0
		if t.scrollOffset > 0 {
			usedWidth += scrollArrowWidth
		}

		fits := false
		for i := t.scrollOffset; i < len(tabs); i++ {
			usedWidth += tabs[i].Width()

			if i == t.activeIdx {
				rightReserve := plusButtonWidth
				if i < len(tabs)-1 {
					rightReserve += scrollArrowWidth
				}
				if usedWidth+rightReserve <= availWidth {
					fits = true
				}
				break
			}
		}

		if fits {
			break
		}
		t.scrollOffset++
	}
}

// hasAttentionInRange returns true if any tab in [start, end) needs attention.
func (t *TabBar) hasAttentionInRange(start, end int) bool {
	for i := start; i < end && i < len(t.tabs); i++ {
		if t.tabs[i].NeedsAttention {
			return true
		}
	}
	return false
}

// clampScroll ensures scrollOffset is within valid bounds.
func (t *TabBar) clampScroll() {
	if t.scrollOffset >= len(t.tabs) {
		t.scrollOffset = max(0, len(t.tabs)-1)
	}
	if t.activeIdx < t.scrollOffset {
		t.scrollOffset = t.activeIdx
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
