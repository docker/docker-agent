package messages

import (
	"sort"

	"github.com/docker/docker-agent/pkg/tui/types"
)

// Work moves as a display slot; source indices and source-owned state stay put.
func (m *model) syncWorkOrder() {
	m.work.order, m.work.positions = nil, nil
	for _, exchange := range m.work.projection.exchanges {
		if len(exchange.work) == 0 {
			continue
		}
		anchor := exchange.work[0]
		first := anchor
		for _, source := range exchange.response {
			if m.messages[source].Type == types.MessageTypeAssistant {
				first = min(first, source)
			}
		}
		if first == anchor {
			continue
		}
		if m.work.order == nil {
			m.work.order = make([]int, len(m.views))
			for i := range m.work.order {
				m.work.order[i] = i
			}
		}
		copy(m.work.order[first+1:anchor+1], m.work.order[first:anchor])
		m.work.order[first] = anchor
	}
	if len(m.work.order) != 0 {
		m.work.positions = make([]int, len(m.work.order))
		for i, source := range m.work.order {
			m.work.positions[source] = i
		}
	}
}

func (m *model) displaySource(position int) int {
	if len(m.work.order) != 0 {
		return m.work.order[position]
	}
	return position
}

func (m *model) displayPosition(source int) int {
	if len(m.work.positions) != 0 {
		return m.work.positions[source]
	}
	return source
}

func (m *model) offsetUpperBound(line int) int {
	offsets := m.displayOffsets()
	return sort.Search(len(offsets), func(i int) bool { return offsets[i] > line })
}

func (m *model) displayOffsets() []int {
	if len(m.work.order) != 0 {
		return m.work.offsets
	}
	return m.lineOffsets
}

func (m *model) displayNeedsSeparator(position int) bool {
	source := m.displaySource(position)
	if !m.work.enabled {
		return m.needsSeparator(source)
	}
	for nextPosition := position + 1; nextPosition < len(m.views); nextPosition++ {
		next := m.displaySource(nextPosition)
		if m.hiddenWorkSource(next) {
			continue
		}
		if anchor, grouped := m.compactWorkAnchor(next); grouped && anchor != next {
			continue
		}
		if m.work.blocks[next] != nil || (m.work.transient != nil && next == m.work.transientSource) {
			return true
		}
		if msg := m.messages[next]; msg.Type == types.MessageTypeAssistant && (msg.Content != "" || len(msg.AssistantMedia) != 0) {
			// The reply already owns a normally invisible action row.
			return false
		}
		return m.work.blocks[source] != nil || m.messages[source].Type != types.MessageTypeToolCall ||
			m.messages[next].Type != types.MessageTypeToolCall || m.needsSeparator(source) && next == source+1
	}
	return false
}

// workBeforeReply finds the preceding visible Work preview, ignoring the
// sources whose rows have moved inside it.
func (m *model) workBeforeReply(position int) int {
	if !m.work.enabled || m.messages[m.displaySource(position)].Type != types.MessageTypeAssistant {
		return -1
	}
	for p := position - 1; p >= 0; p-- {
		source := m.displaySource(p)
		if m.hiddenWorkSource(source) {
			continue
		}
		if anchor, grouped := m.compactWorkAnchor(source); grouped && anchor != source {
			continue
		}
		if block := m.work.blocks[source]; block != nil && block.PreviewLineCount() > 0 {
			return source
		}
		break
	}
	return -1
}

func (m *model) workPreviewGapReplyAt(line int) int {
	if !m.work.enabled || line < 0 || m.renderedLine(line) != "" {
		return -1
	}
	position := m.offsetUpperBound(line)
	if position < len(m.lineOffsets) {
		reply := m.displaySource(position)
		if m.lineOffsets[reply] == line+1 {
			if work := m.workBeforeReply(position); work >= 0 && m.work.ends[work] == line {
				return reply
			}
		}
	}
	return -1
}
