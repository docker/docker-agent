package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/contextbar"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	composerMinRows      = 1
	editorShrinkDelay    = 500 * time.Millisecond
	editorShrinkDuration = animation.ShortDuration
)

type composerHeight struct {
	rows, target, from                                                       int
	manualRows                                                               int // zero means automatic; keep the choice across window clamps
	extraRows, chromeRows, columnWidth                                       int
	bannerRows, separatorRows, tabRows, stripRows, footerRows, indicatorRows int
	historyValue                                                             string
	focusMotion                                                              bool
	motion                                                                   animation.Transition
}

func (m *appModel) composerChromeRows() int {
	c := &m.composer
	c.separatorRows, c.tabRows, c.stripRows, c.footerRows, c.indicatorRows = 0, 0, 0, 0, 0
	remaining := max(0, m.height-styles.EditorStyle.GetVerticalFrameSize()-2)
	if !m.leanMode {
		c.separatorRows = min(1, remaining)
		remaining -= c.separatorRows
		c.footerRows = min(m.statusBar.Height(), remaining)
		remaining -= c.footerRows
		c.tabRows = min(m.tabBar.Height(), remaining)
		remaining -= c.tabRows
		c.stripRows = min(contextbar.Height, remaining)
	} else if m.activeTab.chatPage.IsWorking() || m.activeTab.sessionState.PauseState() != service.PauseNone {
		c.indicatorRows = min(1, remaining)
	}
	if c.tabRows == 0 && m.tabBar != nil {
		m.tabBar.CancelHover()
	}
	return c.separatorRows + c.tabRows + c.stripRows + c.footerRows + c.indicatorRows
}

func (m *appModel) prepareComposerBanner(chrome int) {
	if bar := m.attachmentBar(); bar != nil {
		// Leave a transcript row, history search and the editable cursor row.
		limit := m.height - chrome - styles.EditorStyle.GetVerticalFrameSize() - m.composerExtraRows() - 2
		bar.SetBounds(m.width, min(6, max(0, limit)))
		if m.focusedPanel == PanelAttachments && (m.activeTab.editor.BannerHeight() == 0 || !bar.CanExpand()) {
			m.setPanelFocus(PanelEditor)
		}
	}
}

func (m *appModel) composerExtraRows() int {
	if m.activeTab.editor.IsHistorySearchActive() {
		return 1
	}
	return 0
}

func (m *appModel) composerRowLimits(chrome, extra int) (floor, ceiling int) {
	// When chrome and editable rows compete, keep the real cursor row.
	available := max(1, m.height-chrome-extra-m.activeTab.editor.BannerHeight()-styles.EditorStyle.GetVerticalFrameSize()-1)
	ceiling = min(available, max(composerMinRows, (m.height-6)/2-1))
	return min(composerMinRows, ceiling), ceiling
}

func (m *appModel) composerTextTarget(width, floor, ceiling int) int {
	if m.focusedPanel == PanelContent {
		return floor
	}
	rows := m.composer.manualRows
	if rows == 0 {
		if measurer, ok := m.activeTab.editor.(editor.ContentLineCounter); ok {
			rows = measurer.ContentLineCount(width)
		}
	}
	return min(max(floor, rows), ceiling)
}

func (m *appModel) settleComposerHeight(target, ceiling int, focusChanged bool) tea.Cmd {
	c := &m.composer
	if c.rows > ceiling {
		c.motion.Cancel()
		c.rows = ceiling
	}
	if c.historyValue != "" && c.historyValue != m.activeTab.editor.Value() {
		c.historyValue = ""
	}
	if !focusChanged && c.focusMotion && c.motion.Running() && c.target == target {
		return nil
	}
	if c.rows == 0 || target == c.rows || m.paneHidden ||
		(!focusChanged && (c.manualRows != 0 || (target > c.rows && c.historyValue == ""))) {
		c.motion.Cancel()
		c.rows, c.target = target, target
		c.historyValue = ""
		return nil
	}
	duration, ease := editorShrinkDuration, animation.EaseOutCubic
	if target < c.rows && m.focusedPanel != PanelContent && !focusChanged {
		duration, ease = editorShrinkDelay+editorShrinkDuration, delayedEditorShrink
	}
	if c.motion.Running() && c.target == target && c.motion.Duration() == duration {
		return nil
	}
	c.from, c.target, c.focusMotion = c.rows, target, focusChanged
	if !c.motion.Running() {
		c.motion.SetRuntime(m.ar)
	}
	return c.motion.Start(duration, ease)
}

func delayedEditorShrink(t float64) float64 {
	hold := float64(editorShrinkDelay) / float64(editorShrinkDelay+editorShrinkDuration)
	return animation.EaseOutCubic(max(0, (t-hold)/(1-hold)))
}

func (m *appModel) syncComposerHeight() tea.Cmd {
	return m.syncComposerHeightForFocus(false)
}

func (m *appModel) syncComposerHeightForFocus(focusChanged bool) tea.Cmd {
	if !m.ready || m.height <= 0 {
		return nil
	}
	chrome, extra := m.composerChromeRows(), m.composerExtraRows()
	m.prepareComposerBanner(chrome)
	floor, ceiling := m.composerRowLimits(chrome, extra)
	c := &m.composer
	historyInterrupted := c.historyValue != "" && c.historyValue != m.activeTab.editor.Value()
	width := m.composerEditorWidth()
	banner := m.activeTab.editor.BannerHeight()
	target := m.composerTextTarget(width, floor, ceiling)
	if banner == c.bannerRows && width == c.columnWidth && !focusChanged && !historyInterrupted && target == c.target && chrome == c.chromeRows && extra == c.extraRows && c.rows <= ceiling {
		return nil
	}
	return m.resizeComposer(chrome, extra, target, ceiling, focusChanged)
}

func (m *appModel) resizeComposer(chrome, extra, target, ceiling int, focusChanged bool) tea.Cmd {
	m.composer.chromeRows, m.composer.extraRows = chrome, extra
	m.composer.bannerRows, m.composer.columnWidth = m.activeTab.editor.BannerHeight(), m.composerEditorWidth()
	cmd := m.settleComposerHeight(target, ceiling, focusChanged)
	return tea.Batch(cmd, m.layoutComposer())
}

func (m *appModel) tickComposerHeight(tick animation.TickMsg) tea.Cmd {
	c := &m.composer
	if !c.motion.Running() {
		return nil
	}
	c.motion.Tick()
	if !c.motion.Running() {
		c.historyValue = ""
		c.focusMotion = false
	}
	rows := c.motion.Lerp(c.from, c.target)
	if rows == c.rows {
		return nil
	}
	c.rows = rows
	tick.MarkDirty()
	return m.layoutComposer()
}

func (m *appModel) layoutComposer() tea.Cmd {
	c := &m.composer
	cmd := m.activeTab.editor.SetSize(m.composerEditorWidth(), c.rows+c.extraRows)
	_, editorHeight := m.activeTab.editor.GetSize()
	c.bannerRows = m.activeTab.editor.BannerHeight()
	m.contentHeight = max(0, m.height-c.chromeRows-editorHeight-c.bannerRows)
	chatCmd := m.activeTab.chatPage.SetSize(m.width, m.contentHeight)
	d := m.composerDock()
	m.tabBar.SetWidth(d.tabWidth())
	m.ensureContextBar()
	if m.contextBar != nil {
		m.contextBar.SetWidth(d.stripWidth())
	}
	m.completions.SetAnchor(d.tabOrigin(), d.width, max(0, m.height-d.bannerTop))
	if !m.leanMode {
		m.tour.SetSize(m.width, m.height, m.contentHeight)
	}
	return tea.Batch(cmd, chatCmd)
}

func (m *appModel) composerEditorWidth() int {
	return m.width - styles.EditorStyle.GetHorizontalFrameSize()
}

func (m *appModel) resetComposerHeight() {
	m.composer.motion.Cancel()
	m.composer = composerHeight{}
}
