package dialog

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// dirSection identifies which section of the picker is active.
type dirSection int

const (
	sectionBrowse dirSection = iota
	sectionRecent
	sectionPinned
)

// dirEntryKind distinguishes the different types of entries in the picker.
type dirEntryKind int

const (
	entryPinnedDir dirEntryKind = iota
	entryRecentDir
	entryUseThisDir
	entryParentDir
	entryDir
)

type dirEntry struct {
	name string
	path string
	kind dirEntryKind
}

// truncatePath shortens a path to fit within maxLen, prefixing with "…" when truncated.
func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	return "…" + path[len(path)-(maxLen-1):]
}

const (
	// Dialog sizing
	dirPickerWidthPercent  = 80
	dirPickerMinWidth      = 50
	dirPickerMaxWidth      = 100
	dirPickerHeightPercent = 70
	dirPickerMaxHeight     = 150

	// Dialog chrome dimensions (from styles.DialogStyle: Border + Padding(1,2))
	dirPickerBorderWidth    = 1 // lipgloss.RoundedBorder() is 1 cell per side
	dirPickerHorizPadding   = 2 // DialogStyle Padding(1, 2) horizontal
	dirPickerVertPadding    = 1 // DialogStyle Padding(1, 2) vertical
	dirPickerHorizChrome    = (dirPickerBorderWidth + dirPickerHorizPadding) * 2
	dirPickerContentOffsetX = dirPickerBorderWidth + dirPickerHorizPadding
	dirPickerContentOffsetY = dirPickerBorderWidth + dirPickerVertPadding

	// Content rows above the list (pinned/recent sections):
	// title(1) + titleGap(1) + tabs(1) + space(1) = 4
	dirPickerHeaderRows = 4

	// Additional content rows for browse section:
	// filterInput(1) + space(1) = 2
	dirPickerBrowseFilterRows = 2

	// Content rows below the list: space(1) + helpKeys(1) = 2
	dirPickerFooterRows = 2

	// Total vertical overhead = chrome + header + footer + section-specific rows
	dirPickerOverheadSimple = dirPickerContentOffsetY*2 + dirPickerHeaderRows + dirPickerFooterRows
	dirPickerOverheadBrowse = dirPickerOverheadSimple + dirPickerBrowseFilterRows

	// Y offset from dialog top to the list area
	dirPickerListStartSimple = dirPickerContentOffsetY + dirPickerHeaderRows
	dirPickerListStartBrowse = dirPickerListStartSimple + dirPickerBrowseFilterRows

	dirPickerMaxRecentDirs = 5

	// Entry rendering
	dirPickerStarPrefixWidth   = 2 // "★ " or "☆ " — star + space
	dirPickerIndentPrefixWidth = 2 // "  " — two-space indent for non-starred entries
	dirPickerFolderIconWidth   = 3 // "📁 " — emoji + space (occupies 2 cells + 1 space, but measured as 3)
	dirPickerTabGap            = 4 // spaces between tab labels

	dirPickerFilterCharLimit = 256
)

// tabRegion stores the X range of a rendered tab for mouse hit-testing.
type tabRegion struct {
	xStart, xEnd int
	section      dirSection
}

type workingDirPickerDialog struct {
	BaseDialog

	textInput textinput.Model
	section   dirSection

	// Pinned section state
	pinnedEntries  []dirEntry
	pinnedSelected int
	pinnedScroll   *scrollview.Model

	// Recent section state
	recentEntries  []dirEntry
	recentSelected int
	recentScroll   *scrollview.Model

	// Browse section state
	currentDir     string
	browseEntries  []dirEntry
	browseFiltered []dirEntry
	browseSelected int
	browseScroll   *scrollview.Model
	browseErr      error

	// Shared state
	recentDirs   []string
	favoriteDirs []string
	favoriteSet  map[string]bool
	tuiStore     *tuistate.Store
	keyMap       pickerKeyMap

	// Tab click regions (recomputed each render)
	tabRegions []tabRegion

	// Double-click detection
	lastClickTime  time.Time
	lastClickIndex int
}

// NewWorkingDirPickerDialog creates a new working directory picker dialog.
// recentDirs provides a list of recently used directories to show.
// favoriteDirs provides a list of pinned directories to show.
// store is used for persisting favorite directory changes (may be nil).
// sessionWorkingDir is the working directory of the active session; when non-empty
// it is used as the initial browse directory instead of the process working directory.
func NewWorkingDirPickerDialog(recentDirs, favoriteDirs []string, store *tuistate.Store, sessionWorkingDir string) Dialog {
	ti := textinput.New()
	ti.Placeholder = "Type to filter directories…"
	ti.Focus()
	ti.CharLimit = dirPickerFilterCharLimit
	ti.SetWidth(dirPickerMinWidth)

	cwd := sessionWorkingDir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "/"
		}
	}

	favSet := make(map[string]bool, len(favoriteDirs))
	for _, d := range favoriteDirs {
		favSet[d] = true
	}

	// Remove favorites, current dir, and empty paths from recent dirs
	var filteredRecent []string
	for _, d := range recentDirs {
		if d != "" && !favSet[d] && d != cwd {
			filteredRecent = append(filteredRecent, d)
		}
	}

	d := &workingDirPickerDialog{
		textInput:    ti,
		section:      sectionBrowse,
		currentDir:   cwd,
		recentDirs:   filteredRecent,
		favoriteDirs: favoriteDirs,
		favoriteSet:  favSet,
		tuiStore:     store,
		keyMap:       defaultPickerKeyMap(),
		pinnedScroll: scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
		recentScroll: scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
		browseScroll: scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
	}

	d.rebuildPinnedEntries()
	d.rebuildRecentEntries()
	d.loadBrowseDirectory()

	return d
}

func (d *workingDirPickerDialog) rebuildPinnedEntries() {
	d.pinnedEntries = nil

	for _, dir := range d.favoriteDirs {
		if dir == "" {
			continue
		}
		d.pinnedEntries = append(d.pinnedEntries, dirEntry{name: dir, path: dir, kind: entryPinnedDir})
	}

	if d.pinnedSelected >= len(d.pinnedEntries) {
		d.pinnedSelected = max(0, len(d.pinnedEntries)-1)
	}
}

func (d *workingDirPickerDialog) rebuildRecentEntries() {
	d.recentEntries = nil

	for i, dir := range d.recentDirs {
		if i >= dirPickerMaxRecentDirs {
			break
		}
		if dir == "" || dir == d.currentDir {
			continue
		}
		d.recentEntries = append(d.recentEntries, dirEntry{name: dir, path: dir, kind: entryRecentDir})
	}

	slices.SortFunc(d.recentEntries, func(a, b dirEntry) int {
		return strings.Compare(a.path, b.path)
	})

	if d.recentSelected >= len(d.recentEntries) {
		d.recentSelected = max(0, len(d.recentEntries)-1)
	}
}

func (d *workingDirPickerDialog) loadBrowseDirectory() {
	d.browseEntries = nil
	d.browseFiltered = nil
	d.browseSelected = 0
	d.browseErr = nil
	d.browseScroll.SetScrollOffset(0)

	// Current directory entry (select to use)
	d.browseEntries = append(d.browseEntries, dirEntry{
		name: d.currentDir,
		path: d.currentDir,
		kind: entryUseThisDir,
	})

	if d.currentDir != "/" {
		d.browseEntries = append(d.browseEntries, dirEntry{
			name: "..",
			path: filepath.Dir(d.currentDir),
			kind: entryParentDir,
		})
	}

	var shouldIgnore func(string) bool
	if vcsMatcher, err := fsx.NewVCSMatcher(d.currentDir); err == nil && vcsMatcher != nil {
		shouldIgnore = vcsMatcher.ShouldIgnore
	}

	dirEntries, err := os.ReadDir(d.currentDir)
	if err != nil {
		d.browseErr = err
		d.browseFiltered = d.browseEntries
		return
	}

	for _, entry := range dirEntries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(d.currentDir, entry.Name())
		if shouldIgnore != nil && shouldIgnore(fullPath) {
			continue
		}
		d.browseEntries = append(d.browseEntries, dirEntry{
			name: entry.Name() + "/",
			path: fullPath,
			kind: entryDir,
		})
	}

	d.browseFiltered = d.browseEntries
}

func (d *workingDirPickerDialog) Init() tea.Cmd {
	return textinput.Blink
}

func (d *workingDirPickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	activeScroll := d.activeScrollview()
	if handled, cmd := activeScroll.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		if d.section == sectionBrowse {
			var cmd tea.Cmd
			d.textInput, cmd = d.textInput.Update(msg)
			d.filterBrowseEntries()
			return d, cmd
		}
		return d, nil

	case tea.MouseClickMsg:
		return d.handleMouseClick(msg)

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, core.CmdHandler(CloseDialogMsg{})

		case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
			d.cycleSectionForward()
			return d, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
			d.cycleSectionBackward()
			return d, nil

		case key.Matches(msg, d.keyMap.Up):
			d.moveUp()
			return d, nil

		case key.Matches(msg, d.keyMap.Down):
			d.moveDown()
			return d, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
			for range d.pageSize() {
				d.moveUp()
			}
			return d, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
			for range d.pageSize() {
				d.moveDown()
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Enter):
			cmd := d.handleSelection()
			return d, cmd

		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+p"))):
			if d.pinHelpLabel() != "" {
				d.toggleFavorite()
			}
			return d, nil

		default:
			if d.section == sectionBrowse {
				var cmd tea.Cmd
				d.textInput, cmd = d.textInput.Update(msg)
				d.filterBrowseEntries()
				return d, cmd
			}
			return d, nil
		}
	}

	return d, nil
}

func (d *workingDirPickerDialog) activeScrollview() *scrollview.Model {
	return d.activeSection().scroll
}

// dirSectionState bundles the entries, selection, and scrollview of one
// section of the working-directory picker. activeSection returns a snapshot
// referencing the active section's state so navigation helpers don't have
// to repeat a switch on d.section.
type dirSectionState struct {
	entries  []dirEntry
	selected *int
	scroll   *scrollview.Model
}

func (d *workingDirPickerDialog) activeSection() dirSectionState {
	switch d.section {
	case sectionPinned:
		return dirSectionState{d.pinnedEntries, &d.pinnedSelected, d.pinnedScroll}
	case sectionRecent:
		return dirSectionState{d.recentEntries, &d.recentSelected, d.recentScroll}
	default:
		return dirSectionState{d.browseFiltered, &d.browseSelected, d.browseScroll}
	}
}

// dirPickerSectionOrder is the cycle order used by tab and shift-tab.
var dirPickerSectionOrder = []dirSection{sectionBrowse, sectionRecent, sectionPinned}

func (d *workingDirPickerDialog) cycleSectionForward()  { d.cycleSection(+1) }
func (d *workingDirPickerDialog) cycleSectionBackward() { d.cycleSection(-1) }

func (d *workingDirPickerDialog) cycleSection(delta int) {
	n := len(dirPickerSectionOrder)
	if i := slices.Index(dirPickerSectionOrder, d.section); i >= 0 {
		d.section = dirPickerSectionOrder[(i+delta+n)%n]
	}
	d.updateSectionFocus()
}

func (d *workingDirPickerDialog) updateSectionFocus() {
	if d.section == sectionBrowse {
		d.textInput.Focus()
	} else {
		d.textInput.Blur()
	}
}

func (d *workingDirPickerDialog) moveUp() {
	s := d.activeSection()
	if *s.selected > 0 {
		*s.selected--
		s.scroll.EnsureLineVisible(*s.selected)
	}
}

func (d *workingDirPickerDialog) moveDown() {
	s := d.activeSection()
	if *s.selected < len(s.entries)-1 {
		*s.selected++
		s.scroll.EnsureLineVisible(*s.selected)
	}
}

func (d *workingDirPickerDialog) handleSelection() tea.Cmd {
	s := d.activeSection()
	if *s.selected < 0 || *s.selected >= len(s.entries) {
		return nil
	}
	entry := s.entries[*s.selected]

	// Browsing into directories doesn't close the dialog.
	if d.section == sectionBrowse {
		switch entry.kind {
		case entryParentDir, entryDir:
			d.currentDir = entry.path
			d.textInput.SetValue("")
			d.loadBrowseDirectory()
			return nil
		}
	}

	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(messages.SpawnSessionMsg{WorkingDir: entry.path}),
	)
}

func (d *workingDirPickerDialog) toggleFavorite() {
	if d.tuiStore == nil {
		return
	}
	togglePath, ok := d.selectedTogglePath()
	if !ok {
		return
	}

	isFav, err := d.tuiStore.ToggleFavoriteDir(context.Background(), togglePath)
	if err != nil {
		return
	}

	if isFav {
		d.favoriteSet[togglePath] = true
		d.favoriteDirs = append(d.favoriteDirs, togglePath)
		d.recentDirs = removeFromSlice(d.recentDirs, togglePath)
	} else {
		delete(d.favoriteSet, togglePath)
		d.favoriteDirs = removeFromSlice(d.favoriteDirs, togglePath)
	}

	savedPinnedPath := ""
	if d.pinnedSelected >= 0 && d.pinnedSelected < len(d.pinnedEntries) {
		savedPinnedPath = d.pinnedEntries[d.pinnedSelected].path
	}

	d.rebuildPinnedEntries()
	d.rebuildRecentEntries()

	// Restore pinned selection to same path if possible.
	if savedPinnedPath != "" {
		for i, e := range d.pinnedEntries {
			if e.path == savedPinnedPath {
				d.pinnedSelected = i
				break
			}
		}
	}
}

// selectedTogglePath returns the currently selected entry's path if it can
// be pinned/unpinned (false otherwise).
func (d *workingDirPickerDialog) selectedTogglePath() (string, bool) {
	s := d.activeSection()
	if *s.selected < 0 || *s.selected >= len(s.entries) {
		return "", false
	}
	entry := s.entries[*s.selected]
	if d.section == sectionBrowse && entry.kind == entryParentDir {
		return "", false
	}
	return entry.path, true
}

// removeFromSlice removes all occurrences of val from s.
func removeFromSlice(s []string, val string) []string {
	var result []string
	for _, v := range s {
		if v != val {
			result = append(result, v)
		}
	}
	return result
}

func (d *workingDirPickerDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return d, nil
	}

	// Check tab clicks
	if clicked := d.tabClickTarget(msg.X, msg.Y); clicked >= 0 {
		d.setSection(dirSection(clicked))
		return d, nil
	}

	entryIdx := d.mouseYToEntryIndex(msg.Y)
	if entryIdx < 0 {
		return d, nil
	}

	// Check if the click lands on the star/pin column
	if d.isStarClick(msg.X, entryIdx) {
		d.setSelected(entryIdx)
		if d.pinHelpLabel() != "" {
			d.toggleFavorite()
		}
		return d, nil
	}

	now := time.Now()
	if entryIdx == d.lastClickIndex && now.Sub(d.lastClickTime) < styles.DoubleClickThreshold {
		d.setSelected(entryIdx)
		d.lastClickTime = time.Time{}
		cmd := d.handleSelection()
		return d, cmd
	}

	d.setSelected(entryIdx)
	d.lastClickTime = now
	d.lastClickIndex = entryIdx

	return d, nil
}

// isStarClick returns true if the click X coordinate falls within the star prefix
// column for the given entry index, and the entry supports pinning.
func (d *workingDirPickerDialog) isStarClick(x, entryIdx int) bool {
	_, dialogCol := d.Position()
	starStartX := dialogCol + dirPickerContentOffsetX
	starEndX := starStartX + dirPickerStarPrefixWidth

	if x < starStartX || x >= starEndX {
		return false
	}

	switch d.section {
	case sectionPinned:
		return true
	case sectionBrowse:
		if entryIdx >= 0 && entryIdx < len(d.browseFiltered) {
			kind := d.browseFiltered[entryIdx].kind
			return kind == entryUseThisDir || kind == entryDir
		}
	}
	return false
}

func (d *workingDirPickerDialog) setSection(s dirSection) {
	d.section = s
	d.updateSectionFocus()
}

// tabClickTarget returns the section index if the click is on a tab, or -1.
func (d *workingDirPickerDialog) tabClickTarget(x, y int) int {
	dialogRow, dialogCol := d.Position()
	const rowsBeforeTabs = 2 // title(1) + titleGap(1)
	tabY := dialogRow + dirPickerContentOffsetY + rowsBeforeTabs
	if y != tabY {
		return -1
	}

	contentX := x - (dialogCol + dirPickerContentOffsetX)

	for _, r := range d.tabRegions {
		if contentX >= r.xStart && contentX < r.xEnd {
			return int(r.section)
		}
	}
	return -1
}

func (d *workingDirPickerDialog) setSelected(idx int) {
	*d.activeSection().selected = idx
}

func (d *workingDirPickerDialog) mouseYToEntryIndex(y int) int {
	dialogRow, _ := d.Position()
	_, maxHeight, _ := d.dialogSize()

	listStartY := dialogRow + d.listStartOffset()
	listEndY := listStartY + maxHeight - d.sectionOverhead()

	if y < listStartY || y >= listEndY {
		return -1
	}

	s := d.activeSection()
	entryIdx := s.scroll.ScrollOffset() + (y - listStartY)
	if entryIdx < 0 || entryIdx >= len(s.entries) {
		return -1
	}
	return entryIdx
}

// listStartOffset is the Y offset (relative to the dialog's top) at which
// the active section's list begins.
func (d *workingDirPickerDialog) listStartOffset() int {
	if d.section == sectionBrowse {
		return dirPickerListStartBrowse
	}
	return dirPickerListStartSimple
}

// sectionOverhead is the total non-list chrome (header + footer + filter row
// for browse) of the active section.
func (d *workingDirPickerDialog) sectionOverhead() int {
	if d.section == sectionBrowse {
		return dirPickerOverheadBrowse
	}
	return dirPickerOverheadSimple
}

func (d *workingDirPickerDialog) filterBrowseEntries() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))
	if query == "" {
		d.browseFiltered = d.browseEntries
		d.browseSelected = 0
		d.browseScroll.SetScrollOffset(0)
		return
	}

	d.browseFiltered = nil
	for _, entry := range d.browseEntries {
		// Always include current dir and parent
		if entry.kind == entryUseThisDir || entry.kind == entryParentDir {
			d.browseFiltered = append(d.browseFiltered, entry)
			continue
		}
		if strings.Contains(strings.ToLower(entry.name), query) ||
			strings.Contains(strings.ToLower(entry.path), query) {
			d.browseFiltered = append(d.browseFiltered, entry)
		}
	}

	if d.browseSelected >= len(d.browseFiltered) {
		d.browseSelected = 0
	}
	d.browseScroll.SetScrollOffset(0)
}

func (d *workingDirPickerDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = max(min(d.Width()*dirPickerWidthPercent/100, dirPickerMaxWidth), dirPickerMinWidth)
	maxHeight = min(d.Height()*dirPickerHeightPercent/100, dirPickerMaxHeight)
	contentWidth = dialogWidth - dirPickerHorizChrome - d.pinnedScroll.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *workingDirPickerDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)
	regionWidth := contentWidth + d.pinnedScroll.ReservedCols()

	builder := NewContent(regionWidth).
		AddTitle("New Session: Select Working Directory").
		AddSpace().
		AddContent(d.renderTabs(regionWidth)).
		AddSpace()

	if d.section == sectionBrowse {
		builder.AddContent(d.textInput.View()).AddSpace()
	}

	builder.
		AddContent(d.renderActiveList(contentWidth)).
		AddSpace().
		AddHelpKeys(d.helpKeys()...)

	return styles.DialogStyle.Width(dialogWidth).Render(builder.Build())
}

// renderActiveList renders the list area for the currently active section.
func (d *workingDirPickerDialog) renderActiveList(contentWidth int) string {
	switch d.section {
	case sectionPinned:
		return d.renderPinnedList(contentWidth)
	case sectionRecent:
		return d.renderRecentList(contentWidth)
	default:
		return d.renderBrowseList(contentWidth)
	}
}

// helpKeys returns the key/label pairs displayed at the bottom of the dialog.
func (d *workingDirPickerDialog) helpKeys() []string {
	keys := []string{"↑/↓", "navigate", "tab/shift+tab", "section", "enter", "select"}
	if label := d.pinHelpLabel(); label != "" {
		keys = append(keys, "ctrl+p", label)
	}
	return append(keys, "esc", "cancel")
}

func (d *workingDirPickerDialog) renderTabs(width int) string {
	activeStyle := styles.HighlightWhiteStyle.Underline(true)
	inactiveStyle := styles.MutedStyle
	countStyle := styles.MutedStyle
	activeCountStyle := styles.SecondaryStyle

	type tabInfo struct {
		visualWidth int
		rendered    string
		section     dirSection
	}

	renderTab := func(label string, count int, active bool, section dirSection) tabInfo {
		style := inactiveStyle
		cStyle := countStyle
		if active {
			style = activeStyle
			cStyle = activeCountStyle
		}
		s := style.Render(label)
		vw := lipgloss.Width(s)
		if count > 0 {
			countStr := " " + cStyle.Render("("+strconv.Itoa(count)+")")
			s += countStr
			vw += lipgloss.Width(countStr)
		}
		return tabInfo{visualWidth: vw, rendered: s, section: section}
	}

	tabs := []tabInfo{
		renderTab("Browse", 0, d.section == sectionBrowse, sectionBrowse),
		renderTab("Recent", len(d.recentEntries), d.section == sectionRecent, sectionRecent),
		renderTab("Pinned", len(d.pinnedEntries), d.section == sectionPinned, sectionPinned),
	}

	totalWidth := 0
	for i, t := range tabs {
		totalWidth += t.visualWidth
		if i < len(tabs)-1 {
			totalWidth += dirPickerTabGap
		}
	}

	padLeft := max(0, (width-totalWidth)/2)

	d.tabRegions = d.tabRegions[:0]
	xPos := padLeft
	for i, t := range tabs {
		d.tabRegions = append(d.tabRegions, tabRegion{
			xStart:  xPos,
			xEnd:    xPos + t.visualWidth,
			section: t.section,
		})
		xPos += t.visualWidth
		if i < len(tabs)-1 {
			xPos += dirPickerTabGap
		}
	}

	var parts []string
	for i, t := range tabs {
		parts = append(parts, t.rendered)
		if i < len(tabs)-1 {
			parts = append(parts, strings.Repeat(" ", dirPickerTabGap))
		}
	}

	line := strings.Join(parts, "")
	return styles.BaseStyle.Width(width).Align(lipgloss.Center).Render(line)
}

func (d *workingDirPickerDialog) renderPinnedList(contentWidth int) string {
	lines := make([]string, 0, len(d.pinnedEntries))
	for i, entry := range d.pinnedEntries {
		lines = append(lines, d.renderPinnedEntry(entry, i == d.pinnedSelected, contentWidth))
	}
	d.placeScrollview(d.pinnedScroll, dirPickerListStartSimple)
	d.pinnedScroll.SetContent(lines, len(lines))

	if len(d.pinnedEntries) == 0 {
		return d.renderListPlaceholder(d.pinnedScroll, []string{
			"",
			centeredItalic("No pinned directories", contentWidth),
			"",
			centeredMuted("Use ctrl+p in Browse to pin directories", contentWidth),
		})
	}
	return d.pinnedScroll.View()
}

func (d *workingDirPickerDialog) renderBrowseList(contentWidth int) string {
	lines := make([]string, 0, len(d.browseFiltered))
	for i, entry := range d.browseFiltered {
		lines = append(lines, d.renderBrowseEntry(entry, i == d.browseSelected, contentWidth))
	}
	d.placeScrollview(d.browseScroll, dirPickerListStartBrowse)
	d.browseScroll.SetContent(lines, len(lines))

	switch {
	case d.browseErr != nil:
		return d.renderListPlaceholder(d.browseScroll, []string{
			"",
			centeredError(d.browseErr.Error(), contentWidth),
		})
	case len(d.browseFiltered) == 0:
		return d.renderListPlaceholder(d.browseScroll, []string{
			"",
			centeredItalic("No directories found", contentWidth),
		})
	default:
		return d.browseScroll.View()
	}
}

func (d *workingDirPickerDialog) renderPinnedEntry(entry dirEntry, selected bool, maxWidth int) string {
	nameStyle := styles.PaletteUnselectedActionStyle
	if selected {
		nameStyle = styles.PaletteSelectedActionStyle
	}

	prefix := styles.StarredStyle.Render("★") + " "
	availableWidth := maxWidth - dirPickerStarPrefixWidth
	displayPath := truncatePath(entry.path, availableWidth)

	return prefix + nameStyle.Render(displayPath)
}

func (d *workingDirPickerDialog) renderBrowseEntry(entry dirEntry, selected bool, maxWidth int) string {
	nameStyle := styles.PaletteUnselectedActionStyle
	if selected {
		nameStyle = styles.PaletteSelectedActionStyle
	}

	availableWidth := maxWidth - dirPickerStarPrefixWidth

	switch entry.kind {
	case entryUseThisDir:
		prefix := styles.StarIndicator(d.favoriteSet[entry.path])
		suffixText := "  (use this dir)"
		suffix := styles.MutedStyle.Render(suffixText)
		name := truncatePath(entry.path, availableWidth-len(suffixText))
		return prefix + nameStyle.Render(name) + suffix

	case entryParentDir:
		indent := strings.Repeat(" ", dirPickerIndentPrefixWidth)
		return indent + nameStyle.Render("..")

	case entryDir:
		prefix := styles.StarIndicator(d.favoriteSet[entry.path])
		icon := "📁 "
		name := entry.name
		nameLimit := availableWidth - dirPickerFolderIconWidth
		if len(name) > nameLimit {
			name = name[:nameLimit-1] + "…"
		}
		return prefix + nameStyle.Render(icon+name)
	}

	return ""
}

func (d *workingDirPickerDialog) renderRecentList(contentWidth int) string {
	lines := make([]string, 0, len(d.recentEntries))
	for i, entry := range d.recentEntries {
		lines = append(lines, d.renderRecentEntry(entry, i == d.recentSelected, contentWidth))
	}
	d.placeScrollview(d.recentScroll, dirPickerListStartSimple)
	d.recentScroll.SetContent(lines, len(lines))

	if len(d.recentEntries) == 0 {
		return d.renderListPlaceholder(d.recentScroll, []string{
			"",
			centeredItalic("No recent directories", contentWidth),
		})
	}
	return d.recentScroll.View()
}

// placeScrollview anchors sv to the dialog's content rectangle for accurate
// mouse hit-testing. listStartY is the Y offset from the dialog's top to
// the first row of the list area.
func (d *workingDirPickerDialog) placeScrollview(sv *scrollview.Model, listStartY int) {
	dialogRow, dialogCol := d.Position()
	sv.SetPosition(dialogCol+dirPickerContentOffsetX, dialogRow+listStartY)
}

// renderListPlaceholder fills sv's visible area with the supplied lines plus
// blank padding so the dialog doesn't shrink while a list is empty.
func (d *workingDirPickerDialog) renderListPlaceholder(sv *scrollview.Model, lines []string) string {
	visLines := sv.VisibleHeight()
	out := make([]string, 0, max(visLines, len(lines)))
	out = append(out, lines...)
	for len(out) < visLines {
		out = append(out, "")
	}
	return sv.ViewWithLines(out)
}

// centeredItalic formats msg as a centred italic placeholder of the given width.
func centeredItalic(msg string, width int) string {
	return styles.DialogContentStyle.Italic(true).Align(lipgloss.Center).Width(width).Render(msg)
}

// centeredMuted formats msg as a centred muted placeholder of the given width.
func centeredMuted(msg string, width int) string {
	return styles.MutedStyle.Align(lipgloss.Center).Width(width).Render(msg)
}

// centeredError formats msg as a centred error placeholder of the given width.
func centeredError(msg string, width int) string {
	return styles.ErrorStyle.Align(lipgloss.Center).Width(width).Render(msg)
}

func (d *workingDirPickerDialog) renderRecentEntry(entry dirEntry, selected bool, maxWidth int) string {
	nameStyle := styles.PaletteUnselectedActionStyle
	if selected {
		nameStyle = styles.PaletteSelectedActionStyle
	}

	availableWidth := maxWidth - dirPickerIndentPrefixWidth
	displayPath := truncatePath(entry.path, availableWidth)
	indent := strings.Repeat(" ", dirPickerIndentPrefixWidth)

	return indent + nameStyle.Render(displayPath)
}

func (d *workingDirPickerDialog) pinHelpLabel() string {
	switch d.section {
	case sectionPinned:
		return "unpin"
	case sectionBrowse:
		if d.browseSelected >= 0 && d.browseSelected < len(d.browseFiltered) {
			entry := d.browseFiltered[d.browseSelected]
			if entry.kind == entryParentDir {
				return ""
			}
			if d.favoriteSet[entry.path] {
				return "unpin"
			}
		}
	}
	return "pin"
}

func (d *workingDirPickerDialog) pageSize() int {
	_, maxHeight, _ := d.dialogSize()
	return max(1, maxHeight-d.sectionOverhead())
}

// SetSize sets the dialog dimensions and configures both scrollview regions.
func (d *workingDirPickerDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	_, maxHeight, contentWidth := d.dialogSize()
	regionWidth := contentWidth + d.pinnedScroll.ReservedCols()

	pinnedVis := max(1, maxHeight-dirPickerOverheadSimple)
	d.pinnedScroll.SetSize(regionWidth, pinnedVis)
	d.recentScroll.SetSize(regionWidth, pinnedVis)

	browseVis := max(1, maxHeight-dirPickerOverheadBrowse)
	d.browseScroll.SetSize(regionWidth, browseVis)

	return cmd
}

func (d *workingDirPickerDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}
