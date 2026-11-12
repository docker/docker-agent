package runtime

import (
	"sync"

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

func (s *toolCallLoopSink) exemptCalls(calls []tools.ToolCall) []string {
	var ids []string
	for _, call := range calls {
		if exempt, ok := s.results.Load(call.ID); ok && exempt == true {
			ids = append(ids, call.ID)
		}
	}
	return ids
}
