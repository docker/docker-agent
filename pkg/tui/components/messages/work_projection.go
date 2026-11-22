package messages

import (
	"slices"
	"strings"

	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workExchange is an ephemeral display grouping. Source messages remain in
// model.messages; these indices only describe their compact presentation.
type workExchange struct {
	boundary int
	// afterAgentBoundary excludes reply-only segments from the transient Work header.
	afterAgentBoundary bool
	work               []int
	response           []int
}

type workProjection struct {
	exchanges []workExchange
	bySource  map[int]int
	owner     map[int]int
}

// replyGroup is one logical assistant answer: adjacent assistant rows that no
// activity row separates. Its prose decides whether it counts toward the limit.
type replyGroup struct {
	sources  []int
	segment  int
	hasProse bool
}

// projectWorkWithVisibleReplies maps source messages into human-bounded
// exchanges. Reasoning, tool activity and results belong to Work. Within an
// exchange, the newest visible prose answers stay in the transcript; older
// prose answers fold into Work. Answers without prose always stay visible.
// The session opener stays visible outside Work.
func projectWorkWithVisibleReplies(messages []*types.Message, visible tuimessages.VisibleReplies, opener *types.Message, ordinaryOnly map[*types.Message]bool) workProjection {
	projection := workProjection{bySource: make(map[int]int), owner: make(map[int]int)}
	limit := visibleReplyLimit(visible)
	openerIndex := -1
	if opener != nil {
		openerIndex = slices.Index(messages, opener)
	}
	current := -1
	var fallback *replyGroup
	var groups []*replyGroup
	var groupsByID map[[2]string]*replyGroup
	firstSegment := 0
	var fallbackSender string

	startSegment := func(boundary int, agentBoundary bool) {
		projection.exchanges = append(projection.exchanges, workExchange{boundary: boundary, afterAgentBoundary: agentBoundary})
		current = len(projection.exchanges) - 1
		groupsByID = nil
		fallback = nil
	}
	finishExchange := func() {
		if current < 0 {
			return
		}
		remaining := 0
		for _, group := range groups {
			if group.hasProse {
				remaining++
			}
		}
		for _, group := range groups {
			segment := &projection.exchanges[group.segment]
			if group.hasProse && limit > 0 && remaining > limit {
				segment.work = append(segment.work, group.sources...)
			} else {
				segment.response = append(segment.response, group.sources...)
			}
			if group.hasProse {
				remaining--
			}
		}
		for i := firstSegment; i <= current; i++ {
			segment := &projection.exchanges[i]
			slices.Sort(segment.work)
			slices.Sort(segment.response)
			for _, source := range segment.work {
				projection.bySource[source], projection.owner[source] = i, i
			}
			for _, source := range segment.response {
				projection.owner[source] = i
			}
		}
		groups = nil
		firstSegment = current + 1
	}
	appendWork := func(source int) {
		projection.exchanges[current].work = append(projection.exchanges[current].work, source)
	}
	appendResponse := func(source int) {
		projection.exchanges[current].response = append(projection.exchanges[current].response, source)
	}

	for i, msg := range messages {
		if msg == nil || ordinaryOnly[msg] {
			continue
		}
		if msg.Type == types.MessageTypeUser {
			finishExchange()
			startSegment(i, false)
			continue
		}
		if current < 0 {
			startSegment(-1, false)
		}
		if msg.Type == types.MessageTypeAgentReturn || msg.Type == types.MessageTypeAgentHandoff || msg.Type == types.MessageTypeAgentDelegation {
			startSegment(i, true)
			continue
		}
		switch msg.Type {
		case types.MessageTypeAssistant:
			if i == openerIndex {
				// Only the opener row is pinned; the group that follows keeps the normal limit.
				fallback = nil
				appendResponse(i)
				continue
			}
			group := fallback
			// Prefer upstream's persisted message identity when it is available.
			// Live/legacy rows fall back to contiguous prose from the same sender.
			if msg.MessageID != "" {
				fallback = nil
				if groupsByID == nil {
					groupsByID = make(map[[2]string]*replyGroup)
				}
				key := [2]string{msg.SessionID, msg.MessageID}
				group = groupsByID[key]
				if group == nil {
					group = &replyGroup{segment: current}
					groupsByID[key] = group
					groups = append(groups, group)
				}
			} else if group == nil || fallbackSender != msg.Sender {
				group = &replyGroup{segment: current}
				fallback, fallbackSender = group, msg.Sender
				groups = append(groups, group)
			}
			group.sources = append(group.sources, i)
			group.hasProse = group.hasProse || hasVisibleWorkProse(msg.Content)
		case types.MessageTypeAssistantReasoningBlock:
			fallback = nil
			appendWork(i)
		case types.MessageTypeToolCall:
			// Confirmation prompts need the user, so they stay in the transcript.
			if msg.ToolStatus != types.ToolStatusConfirmation {
				fallback = nil
				appendWork(i)
			}
		case types.MessageTypeToolResult, types.MessageTypeShellOutput:
			appendWork(i)
		}
	}
	finishExchange()
	return projection
}

// visibleReplyLimit maps the policy to the number of prose answers kept visible.
// Unknown or empty policies keep the default of three.
func visibleReplyLimit(visible tuimessages.VisibleReplies) int {
	switch visible {
	case tuimessages.VisibleRepliesLatest:
		return 1
	case tuimessages.VisibleRepliesFive:
		return 5
	case tuimessages.VisibleRepliesAll:
		return -1
	default:
		return 3
	}
}

// sessionOpenerEligible reports whether a row can open the session: a human
// message, or an assistant reply with prose.
func sessionOpenerEligible(msg *types.Message) bool {
	if msg == nil {
		return false
	}
	switch msg.Type {
	case types.MessageTypeUser:
		return true
	case types.MessageTypeAssistant:
		return hasVisibleWorkProse(msg.Content)
	}
	return false
}

// resolveSessionOpener keeps an established opener while it remains an eligible
// source row, so a later prose change cannot replace it. Otherwise the first
// eligible row in content order becomes the opener.
func resolveSessionOpener(messages []*types.Message, current *types.Message) *types.Message {
	if current != nil && slices.Contains(messages, current) && sessionOpenerEligible(current) {
		return current
	}
	for _, msg := range messages {
		if sessionOpenerEligible(msg) {
			return msg
		}
	}
	return nil
}

func hasVisibleWorkProse(content string) bool {
	return strings.TrimSpace(content) != ""
}
