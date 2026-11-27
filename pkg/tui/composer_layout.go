package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// composerDock is the geometry shared by painting, completion and pointer routing.
type composerDock struct {
	width                                                            int
	bannerTop, separatorTop, tabsTop, editorTop, stripTop, footerTop int
}

func (m *appModel) composerDock() composerDock {
	d := composerDock{width: m.width, bannerTop: m.contentHeight}
	d.separatorTop = d.bannerTop + m.activeTab.editor.BannerHeight()
	d.tabsTop = d.separatorTop + m.composer.separatorRows
	d.editorTop = d.tabsTop + m.composer.tabRows + m.composer.indicatorRows
	_, height := m.activeTab.editor.GetSize()
	d.stripTop = d.editorTop + height
	d.footerTop = d.stripTop + m.composer.stripRows
	return d
}

func (d composerDock) containsX(x int) bool { return x >= 0 && x < d.width }

func (d composerDock) tabOrigin() int { return min(styles.EditorHMargin, d.width/2) }
func (d composerDock) containsTabX(x int) bool {
	return x >= d.tabOrigin() && x < d.tabOrigin()+d.tabWidth()
}

func (d composerDock) tabWidth() int { return max(0, d.width-2*d.tabOrigin()) }

func (d composerDock) stripOrigin() int {
	if d.width <= styles.EditorStyle.GetHorizontalFrameSize()+2 {
		return 0
	}
	return d.tabOrigin()
}

func (d composerDock) stripWidth() int { return max(0, d.width-2*d.stripOrigin()) }

func (m *appModel) attachmentBar() editor.AttachmentBar {
	return attachmentBarOf(m.activeTab.editor)
}

func attachmentBarOf(ed editor.Editor) editor.AttachmentBar {
	if p, ok := ed.(interface{ Banner() editor.AttachmentBar }); ok {
		return p.Banner()
	}
	return nil
}

func (m *appModel) editorMouse(x, y int) (int, int) {
	d := m.composerDock()
	frame := styles.EditorStyle
	if d.width > frame.GetHorizontalFrameSize()+2 {
		x -= frame.GetMarginLeft() + frame.GetPaddingLeft()
	}
	return x, y - d.editorTop - frame.GetPaddingTop()
}

func composerRows(view string, x, width, height int) string {
	rows := make([]string, 0, height)
	for row := range strings.SplitSeq(view, "\n") {
		if len(rows) == height {
			break
		}
		rows = append(rows, strings.Repeat(" ", max(0, x))+ansi.Truncate(row, width, ""))
	}
	for len(rows) < height {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

func (m *appModel) composerView() string {
	d := m.composerDock()
	var rows []string
	add := func(view string, x, width, height int) {
		if height > 0 {
			rows = append(rows, composerRows(view, x, width, height))
		}
	}
	if bar := m.attachmentBar(); bar != nil {
		add(bar.View(), 0, d.width, d.separatorTop-d.bannerTop)
	}
	if m.composer.separatorRows > 0 {
		add(m.renderResizeHandle(d.width), 0, d.width, m.composer.separatorRows)
	}
	if m.composer.tabRows > 0 {
		add(m.tabBar.View(), d.tabOrigin(), d.tabWidth(), m.composer.tabRows)
	}
	if m.composer.indicatorRows > 0 {
		add(m.renderLeanWorkingIndicator(), 0, d.width, m.composer.indicatorRows)
	}
	add(m.activeTab.editor.View(), 0, d.width, d.stripTop-d.editorTop)
	if m.composer.stripRows > 0 {
		add(m.contextBar.View(), d.stripOrigin(), d.stripWidth(), m.composer.stripRows)
	}
	return strings.Join(rows, "\n")
}

func (m *appModel) setPanelFocus(panel FocusedPanel) tea.Cmd {
	if m.focusedPanel == panel {
		return nil
	}
	if bar := m.attachmentBar(); bar != nil {
		bar.SetFocused(panel == PanelAttachments)
	}
	m.focusedPanel = panel
	m.statusBar.InvalidateCache()
	if panel == PanelContent {
		m.activeTab.editor.Blur()
		return m.activeTab.chatPage.FocusMessages()
	}
	m.activeTab.chatPage.BlurMessages()
	if panel == PanelAttachments {
		return m.activeTab.editor.Blur()
	}
	return m.activeTab.editor.Focus()
}
