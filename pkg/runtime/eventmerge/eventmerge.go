// Package eventmerge joins the streaming delta events a runtime emits (one
// per model token or per tool output write) into fewer, larger deltas.
//
// Only consecutive deltas that extend the same message or tool call are
// joined, so the merged stream carries exactly the same content, in the same
// order, as the original one. Consumers that concatenate deltas see no
// difference other than the chunk boundaries.
package eventmerge

import (
	"cmp"
	"strings"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// Mergeable reports whether event is a streaming delta that [Merge] can join
// with its neighbours.
func Mergeable(event any) bool {
	switch event.(type) {
	case *runtime.AgentChoiceEvent,
		*runtime.AgentChoiceReasoningEvent,
		*runtime.PartialToolCallEvent,
		*runtime.ToolCallOutputEvent:
		return true
	default:
		return false
	}
}

// Merge joins each run of consecutive deltas that extend the same message or
// tool call into a single event. Any other value passes through unchanged and
// ends the current run. A delta that has nothing to merge with is returned
// as-is, not copied.
//
// Each run is built with a single strings.Builder, so merging N chunks costs
// O(N) rather than the O(N^2) of repeated string concatenation.
func Merge[E any](events []E) []E {
	if len(events) < 2 {
		return events
	}

	result := make([]E, 0, len(events))
	for i := 0; i < len(events); i++ {
		merged, consumed := mergeRun(events[i], events[i+1:])
		result = append(result, merged)
		i += consumed
	}
	return result
}

// mergeRun merges first with the deltas directly following it. It returns
// the merged event and how many of rest it consumed.
func mergeRun[E any](first E, rest []E) (E, int) {
	switch ev := any(first).(type) {
	case *runtime.AgentChoiceEvent:
		return mergeAgentChoiceRun(ev, rest)
	case *runtime.AgentChoiceReasoningEvent:
		return mergeAgentChoiceReasoningRun(ev, rest)
	case *runtime.PartialToolCallEvent:
		return mergePartialToolCallRun(ev, rest)
	case *runtime.ToolCallOutputEvent:
		return mergeToolCallOutputRun(ev, rest)
	default:
		return first, 0
	}
}

func mergeAgentChoiceRun[E any](first *runtime.AgentChoiceEvent, rest []E) (E, int) {
	n, total := 0, len(first.Content)
	for _, msg := range rest {
		next, ok := any(msg).(*runtime.AgentChoiceEvent)
		if !ok || next.AgentName != first.AgentName || next.SessionID != first.SessionID || next.MessageID != first.MessageID {
			break
		}
		total += len(next.Content)
		n++
	}
	if n == 0 {
		return as[E](first), 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Content)
	for _, msg := range rest[:n] {
		b.WriteString(any(msg).(*runtime.AgentChoiceEvent).Content)
	}
	return as[E](&runtime.AgentChoiceEvent{
		Type:         first.Type,
		SessionID:    first.SessionID,
		MessageID:    first.MessageID,
		Content:      b.String(),
		AgentContext: first.AgentContext,
	}), n
}

func mergeAgentChoiceReasoningRun[E any](first *runtime.AgentChoiceReasoningEvent, rest []E) (E, int) {
	n, total := 0, len(first.Content)
	for _, msg := range rest {
		next, ok := any(msg).(*runtime.AgentChoiceReasoningEvent)
		if !ok || next.AgentName != first.AgentName || next.SessionID != first.SessionID || next.MessageID != first.MessageID {
			break
		}
		total += len(next.Content)
		n++
	}
	if n == 0 {
		return as[E](first), 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Content)
	for _, msg := range rest[:n] {
		b.WriteString(any(msg).(*runtime.AgentChoiceReasoningEvent).Content)
	}
	return as[E](&runtime.AgentChoiceReasoningEvent{
		Type:         first.Type,
		SessionID:    first.SessionID,
		MessageID:    first.MessageID,
		Content:      b.String(),
		AgentContext: first.AgentContext,
	}), n
}

func mergeToolCallOutputRun[E any](first *runtime.ToolCallOutputEvent, rest []E) (E, int) {
	n, total := 0, len(first.Output)
	for _, msg := range rest {
		next, ok := any(msg).(*runtime.ToolCallOutputEvent)
		if !ok || next.ToolCallID != first.ToolCallID {
			break
		}
		total += len(next.Output)
		n++
	}
	if n == 0 {
		return as[E](first), 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Output)
	for _, msg := range rest[:n] {
		b.WriteString(any(msg).(*runtime.ToolCallOutputEvent).Output)
	}
	return as[E](&runtime.ToolCallOutputEvent{
		Type:           first.Type,
		ToolCallID:     first.ToolCallID,
		ToolDefinition: first.ToolDefinition,
		Output:         b.String(),
		AgentContext:   first.AgentContext,
	}), n
}

func mergePartialToolCallRun[E any](first *runtime.PartialToolCallEvent, rest []E) (E, int) {
	n, total := 0, len(first.ToolCall.Function.Arguments)
	for _, msg := range rest {
		next, ok := any(msg).(*runtime.PartialToolCallEvent)
		if !ok || next.ToolCall.ID != first.ToolCall.ID {
			break
		}
		total += len(next.ToolCall.Function.Arguments)
		n++
	}
	if n == 0 {
		return as[E](first), 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.ToolCall.Function.Arguments)

	name := first.ToolCall.Function.Name
	toolDef := first.ToolDefinition
	for _, msg := range rest[:n] {
		next := any(msg).(*runtime.PartialToolCallEvent)
		b.WriteString(next.ToolCall.Function.Arguments)
		// The name and definition may only arrive on a later delta.
		name = cmp.Or(name, next.ToolCall.Function.Name)
		toolDef = cmp.Or(toolDef, next.ToolDefinition)
	}
	toolCall := first.ToolCall
	toolCall.Function = tools.FunctionCall{Name: name, Arguments: b.String()}
	return as[E](&runtime.PartialToolCallEvent{
		Type:           first.Type,
		ToolCall:       toolCall,
		ToolDefinition: toolDef,
		AgentContext:   first.AgentContext,
	}), n
}

// deltaSize is the number of content bytes a delta carries.
func deltaSize(event any) int {
	switch ev := event.(type) {
	case *runtime.AgentChoiceEvent:
		return len(ev.Content)
	case *runtime.AgentChoiceReasoningEvent:
		return len(ev.Content)
	case *runtime.PartialToolCallEvent:
		return len(ev.ToolCall.Function.Arguments)
	case *runtime.ToolCallOutputEvent:
		return len(ev.Output)
	default:
		return 0
	}
}

// as converts a delta back to the slice's element type. It cannot fail: the
// value was either taken from that slice or is the same delta type as one
// that was.
func as[E any](event any) E {
	return event.(E)
}
