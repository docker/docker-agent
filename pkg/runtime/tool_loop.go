package runtime

import (
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// toolCallLoopSink observes dispatch results without retaining their payloads.
type toolCallLoopSink struct {
	EventSink

	results sync.Map
}

func (s *toolCallLoopSink) Emit(event Event) {
	if response, ok := event.(*ToolCallResponseEvent); ok {
		exempt := response.ToolDefinition.AllowRepeatedCalls && response.Result != nil && !response.Result.IsError
		s.results.Store(response.ToolCallID, exempt)
	}
	s.EventSink.Emit(event)
}

// recall tells the sink of the calls of a batch whose results are in the
// session already: the batch is taken up again, and their responses were
// emitted before it was cut off. results are the messages the turn has
// added since its answer. A call ID says nothing outside its own answer:
// another turn, or a sub-agent, may have used the same one.
func (s *toolCallLoopSink) recall(results []session.Message, calls []tools.ToolCall, agentTools []tools.Tool) {
	repeatable := make(map[string]bool, len(calls))
	for _, call := range calls {
		for _, tool := range agentTools {
			if tool.Name == call.Function.Name {
				repeatable[call.ID] = tool.AllowRepeatedCalls
				break
			}
		}
	}
	for _, msg := range results {
		if msg.Message.Role != chat.MessageRoleTool {
			continue
		}
		if allowed, ok := repeatable[msg.Message.ToolCallID]; ok {
			s.results.Store(msg.Message.ToolCallID, allowed && !msg.Message.IsError)
		}
	}
}

func (s *toolCallLoopSink) exemptCalls(calls []tools.ToolCall) []string {
	var ids []string
	for _, call := range calls {
		if exempt, ok := s.results.Load(call.ID); ok && exempt == true {
			ids = append(ids, call.ID)
		}
	}
	return ids
}
