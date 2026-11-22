package messages

import (
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func (m *model) stickyWorkEnabled() bool {
	return m.work.enabled && m.height >= 4 && m.contentWidth() >= 16 && !m.selection.hasRange() && len(m.lineOffsets) == len(m.views)
}

type stickyWorkRows struct {
	inset  int
	spacer bool
}

// stickyWorkAtOffset reserves viewport rows without adding transcript rows.
// scrollOffset remains the first body row.
func (m *model) stickyWorkAtOffset(offset int) (anchor int, block *workblock.Model, rows stickyWorkRows) {
	if !m.stickyWorkEnabled() || offset < 0 || offset >= m.totalHeight {
		return 0, nil, stickyWorkRows{}
	}
	position := m.offsetUpperBound(offset) - 1
	if position < 0 {
		return 0, nil, stickyWorkRows{}
	}
	source := m.displaySource(position)
	if offset >= m.work.ends[source] {
		source = m.workPreviewGapReplyAt(offset)
		if source < 0 {
			return 0, nil, stickyWorkRows{}
		}
	}
	exchangeIndex, ok := m.work.projection.owner[source]
	if !ok {
		return 0, nil, stickyWorkRows{}
	}
	exchange := m.work.projection.exchanges[exchangeIndex]
	if exchange.boundary < 0 || len(exchange.work) == 0 {
		return 0, nil, stickyWorkRows{}
	}
	anchor = exchange.work[0]
	block = m.work.blocks[anchor]
	if block == nil {
		return 0, nil, stickyWorkRows{}
	}
	headerEnd := m.lineOffsets[anchor] + 1
	end := m.work.ends[anchor]
	if n := len(exchange.response); n != 0 {
		end = max(end, m.work.ends[exchange.response[n-1]])
	}
	preview := block.PreviewLineCount()
	if headerEnd > offset || (preview == 0 && end-headerEnd < 6) {
		return 0, nil, stickyWorkRows{}
	}
	// Leave at least one body row and don't duplicate preview rows still in the body.
	inset := 1 + min(preview, offset-headerEnd, m.height-2)
	if end-offset < min(3, max(1, m.height-inset-1)) {
		return 0, nil, stickyWorkRows{}
	}
	spacer := false
	if preview > 0 && offset >= m.work.ends[anchor] {
		if m.isWorkPreviewMargin(source, offset-m.lineOffsets[source], m.renderedLine(offset)) {
			// The body already separates the preview. Keep the
			// next body row visible as well when the inset fills the screen.
			inset = min(inset, m.height-2)
		} else if inset < m.height-1 {
			inset++
			spacer = true
		}
	}
	return anchor, block, stickyWorkRows{inset: inset, spacer: spacer}
}

func blankTranscriptLine(line string) bool { return strings.TrimSpace(ansi.Strip(line)) == "" }

func (m *model) isWorkPreviewMargin(source, local int, line string) bool {
	// The reserved action row still separates the preview from reply prose
	// when Copy paints its right edge. Hover must not change the geometry.
	if local >= 0 {
		if view, ok := m.views[source].(message.Model); ok && view.AssistantActionRow() == local {
			return true
		}
	}
	return blankTranscriptLine(line)
}

func (m *model) stickyWork() (int, *workblock.Model, bool) {
	anchor, block, rows := m.stickyWorkAtOffset(m.scrollOffset)
	return anchor, block, rows.inset > 0
}

func (m *model) stickyWorkAt(x, y int) (int, *workblock.Model, bool) {
	if y != m.yPos || x < m.xPos || x >= m.xPos+m.contentWidth() {
		return 0, nil, false
	}
	return m.stickyWork()
}

func (m *model) stickyWorkInset() int {
	_, _, rows := m.stickyWorkAtOffset(m.scrollOffset)
	return rows.inset
}

func (m *model) bodyHeight() int { return max(0, m.height-m.stickyWorkInset()) }

// Match the inset at the resulting bottom, not the inset at the reader's offset.
func (m *model) stickyWorkBottomReserve(total int) int {
	if total > m.height && m.stickyWorkEnabled() {
		fallback := 0
		for reserve := min(5, m.height-1); reserve > 0; reserve-- {
			if _, _, rows := m.stickyWorkAtOffset(total - m.height + reserve); rows.inset == reserve {
				return reserve
			} else if rows.inset < reserve {
				fallback = reserve
			}
		}
		// Reusing a body blank can make the inset jump past equality.
		if _, _, rows := m.stickyWorkAtOffset(total - m.height); rows.inset > 0 {
			return fallback
		}
	}
	return 0
}

func (m *model) activateStickyWork(block *workblock.Model) tea.Cmd {
	anchor, _, _ := m.stickyWork()
	_, inWork := m.compactWorkRowAt(m.scrollOffset)
	if !block.Expanded() || inWork {
		m.userHasScrolled = true
		block.SetExpandedImmediately(!block.Expanded())
		m.renderDirty = true
		m.invalidateView()
		m.updateScrollState()
		m.setScrollOffset(m.lineOffsets[anchor])
		return block.TakeAnimationCmd()
	}
	// Folding from a reply preserves its body anchor and follow mode.
	reader := m.captureCompactWorkAnchor()
	block.SetExpandedImmediately(false)
	m.renderDirty = true
	m.invalidateView()
	m.updateScrollState()
	m.restoreCompactWorkAnchor(reader)
	return block.TakeAnimationCmd()
}

func (m *model) selectionMouseToLineCol(x, y int) (int, int) {
	m.updateScrollState()
	inset := m.stickyWorkInset()
	bodyY := y - m.yPos - inset
	if inset > 0 && bodyY < 0 {
		return m.scrollOffset, 0
	}
	if line := m.scrollOffset + max(0, bodyY); m.workPreviewGapReplyAt(line) >= 0 {
		return line + 1, 0
	}
	return m.mouseToLineCol(x, y)
}

func (m *model) syncTranscriptScrollbar() {
	m.scrollview.SetContentWithTrailingSlack(m.renderedLines, m.totalHeight, m.bottomSlack)
	m.scrollview.SetScrollOffset(m.scrollOffset)
}

type compactWorkAnchor struct {
	source *types.Message
	block  *workblock.Model
	local  int
	index  int
	gap    bool
}

func (m *model) captureCompactWorkAnchor() compactWorkAnchor {
	if !m.userHasScrolled || len(m.lineOffsets) == 0 {
		return compactWorkAnchor{}
	}
	if reply := m.workPreviewGapReplyAt(m.scrollOffset); reply >= 0 {
		return compactWorkAnchor{source: m.work.sig[reply].msg, index: reply, gap: true}
	}
	position := m.offsetUpperBound(m.scrollOffset) - 1
	if position < 0 {
		return compactWorkAnchor{}
	}
	source := m.displaySource(position)
	local := m.scrollOffset - m.lineOffsets[source]
	var block *workblock.Model
	if item, ok := m.compactWorkRangeAt(m.scrollOffset); ok {
		local = m.scrollOffset - item.start
		if child, childLocal, ok := item.block.SourceAt(local); ok {
			source, local = child, childLocal
		} else {
			source, block = item.anchor, item.block
		}
	}
	if m.work.enabled && source < len(m.work.sig) {
		return compactWorkAnchor{source: m.work.sig[source].msg, block: block, local: local, index: source}
	}
	if source < len(m.messages) {
		return compactWorkAnchor{source: m.messages[source], local: local, index: source}
	}
	return compactWorkAnchor{}
}

func (m *model) restoreCompactWorkAnchor(anchor compactWorkAnchor) {
	if anchor.source == nil {
		return
	}
	source := anchor.index
	if source >= len(m.messages) || m.messages[source] != anchor.source {
		source = slices.Index(m.messages, anchor.source)
	}
	if source < 0 || source >= len(m.lineOffsets) {
		return
	}
	line := m.lineOffsets[source] + max(0, anchor.local)
	if anchor.gap && m.workPreviewGapReplyAt(line-1) == source {
		m.setScrollOffset(line - 1)
		return
	}
	if owner, grouped := m.compactWorkAnchor(source); grouped {
		line = m.lineOffsets[owner]
		block := m.work.blocks[owner]
		if block == anchor.block {
			line += min(anchor.local, max(0, m.work.ends[owner]-line-1))
		} else {
			for _, row := range block.Rows() {
				if row.SourceIndex == source {
					line += row.Start + min(anchor.local, row.End-row.Start-1)
					break
				}
			}
		}
	} else if end := m.compactWorkSourceEnd(source); end > m.lineOffsets[source] {
		line = min(line, end-1)
	}
	m.setScrollOffset(line)
}

func (m *model) compactWorkSourceEnd(source int) int {
	if m.work.enabled {
		return m.work.ends[source]
	}
	if source+1 < len(m.lineOffsets) {
		return m.lineOffsets[source+1]
	}
	return m.totalHeight
}

func (m *model) compactWorkRangeAt(line int) (compactWorkRange, bool) {
	i := sort.Search(len(m.work.ranges), func(i int) bool { return m.work.ranges[i].start+m.work.ranges[i].height > line })
	if i < len(m.work.ranges) && line >= m.work.ranges[i].start {
		return m.work.ranges[i], true
	}
	return compactWorkRange{}, false
}
