// Package tabbar provides a horizontal tab bar for the TUI.
package tabbar

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// tabBarHeight is the number of terminal rows the tab bar occupies.
	tabBarHeight = 1
	// fallbackWidth is used before a width has been assigned.
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
	ar        *animation.Runtime
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
	hideSingle  bool
	canSpawn    bool

	// lastEnsuredIdx tracks which active tab was last scrolled-to by
	// ensureActiveVisible. This prevents View() from overriding manual
	// scroll actions — ensureActiveVisible only runs when the active tab
	// actually changes.
	lastEnsuredIdx int

	drag          dragState
	buttonHover   animation.HoverFade[tabButton]
	hoveredButton tabButton
	plusPulse     animation.Transition

	visualGeneration uint64
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
		CloseTab: core.GetKeys().CloseTab,
	}
}

// New creates a new tab bar with the given max title length.
// If maxTitleLen is <= 0, the default (20) is used.
func New(ar *animation.Runtime, maxTitleLen int) *TabBar {
	if maxTitleLen <= 0 {
		maxTitleLen = defaultMaxTitleLen
	}
	return &TabBar{
		ar:             ar,
		width:          fallbackWidth,
		buttonHover:    animation.NewHoverFade[tabButton](ar, buttonHoverDuration, animation.EaseOutCubic),
		plusPulse:      ar.Transition(),
		keyMap:         DefaultKeyMap(),
		maxTitleLen:    maxTitleLen,
		canSpawn:       true,
		lastEnsuredIdx: noTab,
		drag:           dragState{dropIdx: noTab},
	}
}

// SetCanSpawn configures whether new tabs can be spawned (showing "+" and
// allowing the last remaining tab to be closed and replaced).
func (t *TabBar) SetCanSpawn(canSpawn bool) {
	if t.canSpawn == canSpawn {
		return
	}
	t.canSpawn = canSpawn
	t.resetPointerGeometry()
	t.visualGeneration++
}

func (t *TabBar) canCloseTab() bool { return len(t.tabs) > 1 || t.canSpawn }

// CanCloseTabWithKey reports whether the close shortcut applies.
func (t *TabBar) CanCloseTabWithKey() bool { return len(t.tabs) > 1 }

// SetWidth sets the available width for the tab bar.
func (t *TabBar) SetWidth(width int) {
	if t.width != width {
		t.width = max(0, width)
		t.resetPointerGeometry()
	}
}

// SetMaxTitleLength applies the title truncation limit immediately.
func (t *TabBar) SetMaxTitleLength(maxTitleLen int) {
	if maxTitleLen <= 0 {
		maxTitleLen = defaultMaxTitleLen
	}
	t.maxTitleLen = maxTitleLen
	t.lastEnsuredIdx = noTab
	t.resetPointerGeometry()
	t.clampScroll()
}

// SetTabs updates the list of tabs and active index.
func (t *TabBar) SetTabs(tabs []messages.TabInfo, activeIdx int) {
	if !sameTabOrder(t.tabs, tabs) {
		t.resetPointerGeometry()
	}

	if activeIdx != t.activeIdx {
		// Active tab changed — force ensureActiveVisible on next View.
		t.lastEnsuredIdx = noTab
	}
	t.tabs = tabs
	t.activeIdx = activeIdx
	t.clampScroll()
}

func sameTabOrder(old, next []messages.TabInfo) bool {
	if len(old) != len(next) {
		return false
	}
	for i, tab := range old {
		if tab.SessionID != next[i].SessionID {
			return false
		}
	}
	return true
}

func (t *TabBar) resetPointerGeometry() {
	t.ClearHover()
	t.zones = t.zones[:0]
	t.dragBounds = t.dragBounds[:0]
	t.drag = dragState{dropIdx: noTab}
}

// SetHideSingle controls whether a lone session needs a tab row.
func (t *TabBar) SetHideSingle(hide bool) {
	if hide != t.hideSingle {
		t.hideSingle = hide
		t.resetPointerGeometry()
		if t.Height() == 0 {
			t.CancelHover()
		}
	}
}

// Height returns the number of visible tab rows.
func (t *TabBar) Height() int {
	if len(t.tabs) == 0 || (t.hideSingle && len(t.tabs) == 1) {
		return 0
	}
	return tabBarHeight
}

// IsDragging returns true when a tab drag is in progress.
func (t *TabBar) IsDragging() bool {
	return t.drag.active
}

// VisualGeneration changes only when rendered tab geometry or drag styling changes.
func (t *TabBar) VisualGeneration() uint64 { return t.visualGeneration }

// Bindings returns consolidated key bindings for the help bar.
func (t *TabBar) Bindings() []key.Binding {
	return []key.Binding{
		t.keyMap.NewTab,
		core.GetKeys().CloseTab,
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

		case key.Matches(msg, core.GetKeys().CloseTab):
			if !t.CanCloseTabWithKey() {
				return nil
			}
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[t.activeIdx].SessionID})
		}

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			cmd := t.UpdateHover(msg.X)
			if t.hoveredButton.plus {
				t.visualGeneration++
				cmd = tea.Batch(cmd, t.plusPulse.Start(buttonPulseDuration, animation.EaseOutCubic))
			}
			return tea.Batch(cmd, t.handleLeftClickDown(msg.X))
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
		if len(t.tabs) > 1 && z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && !z.isClose {
			t.CancelHover()
			t.drag = dragState{
				active:  true,
				dragIdx: z.tabIdx,
				dropIdx: noTab,
				startX:  x,
			}
			t.visualGeneration++
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

	beforeVisualDrop := noTab
	if !t.drag.isNoOp() {
		beforeVisualDrop = t.drag.dropIdx
	}
	next := t.dropIndexForX(x)
	t.drag.dropIdx = next
	afterVisualDrop := noTab
	if !t.drag.isNoOp() {
		afterVisualDrop = next
	}
	if afterVisualDrop != beforeVisualDrop {
		t.visualGeneration++
	}
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
	t.visualGeneration++

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
	if !t.canCloseTab() {
		return nil
	}
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
		case z.isPlus && t.canSpawn:
			return core.CmdHandler(messages.SpawnSessionMsg{})
		case z.isClose && t.canCloseTab() && z.tabIdx >= 0 && z.tabIdx < len(t.tabs):
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		case z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && z.tabIdx != t.activeIdx:
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		}
		return nil
	}
	return nil
}

// View renders the tab bar as a single line: tab tab tab  +
func (t *TabBar) View() string {
	// Reset zones (reuse backing array if available).
	t.zones = t.zones[:0]
	if t.Height() == 0 || t.width == 0 {
		return ""
	}

	availWidth := t.width
	plusReserve := 0
	if t.canSpawn {
		plusReserve = plusButtonWidth
		if availWidth < plusButtonWidth {
			t.zones = append(t.zones, clickZone{endX: availWidth, tabIdx: noTab, isPlus: true})
			return t.plusStyle().Render("+" + strings.Repeat(" ", availWidth-1))
		}
	}

	showClose := t.canCloseTab()

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
		allTabs[i] = renderTab(info, t.maxTitleLen, role, t.ar.Now(), t.hoverFraction(tabButton{sessionID: info.SessionID}), showClose)
		totalWidth += allTabs[i].Width()
	}
	totalWidth += plusReserve

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
	arrowStyle := lipgloss.NewStyle().Foreground(chromeFg)
	// Attention arrow style: warning-colored and bold so off-screen attention tabs are obvious.
	attnArrowStyle := lipgloss.NewStyle().Foreground(styles.EnsureContrast(styles.Warning, styles.Background)).Bold(true)

	var line strings.Builder
	var cursor int

	// Left scroll arrow — highlight if any off-screen tab to the left needs attention.
	if needsScroll && t.scrollOffset > 0 && availWidth >= plusReserve+scrollArrowWidth {
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
		rightReserve := plusReserve
		if needsScroll && i < len(allTabs)-1 && availWidth-cursor >= plusReserve+scrollArrowWidth {
			rightReserve += scrollArrowWidth
		}

		// Account for drop indicator width when checking available space.
		extra := 0
		if visualDrop >= 0 && i >= visualDrop {
			extra = dropIndicatorWidth
		}

		budget := availWidth - cursor - extra - rightReserve
		if budget <= 0 || (tabW > budget && i > t.scrollOffset) {
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
		text := ansi.Truncate(tab.View(), budget, "")
		end := cursor + ansi.StringWidth(text)
		mainEnd := min(end, cursor+tab.MainZoneEnd())
		if showClose && end-mainEnd >= lipgloss.Width(closeButtonText) {
			t.zones = append(t.zones, clickZone{startX: mainEnd, endX: end, tabIdx: i, isClose: true})
		} else {
			mainEnd = end
		}
		t.zones = append(t.zones, clickZone{startX: cursor, endX: mainEnd, tabIdx: i})

		line.WriteString(text)
		cursor = end
		lastVisibleIdx = i
	}

	// Drop indicator after the last visible tab.
	if visualDrop == lastVisibleIdx+1 && cursor+dropIndicatorWidth+plusReserve <= availWidth {
		line.WriteString(dropLine)
		cursor += dropIndicatorWidth
	}

	// Right scroll arrow — highlight if any off-screen tab to the right needs attention.
	if needsScroll && lastVisibleIdx < len(allTabs)-1 && cursor+scrollArrowWidth+plusReserve <= availWidth {
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
	if t.canSpawn {
		plus := t.plusStyle().Render(plusButtonText)
		line.WriteString(plus)
		t.zones = append(t.zones, clickZone{
			startX: cursor, endX: cursor + plusButtonWidth,
			tabIdx: noTab, isPlus: true,
		})
	}

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
