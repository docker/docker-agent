//go:build js && wasm

package main

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/embeddedchat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// eventProjector flattens the runtime's event stream into the JS events the
// host sees (see the package doc for the shapes). It remembers the active
// agent so an in-place handoff, which the runtime only reveals through the
// next AgentInfo, is still reported as a handoff.
type eventProjector struct {
	agent string
}

// project returns the JS events for one embedded chat event; most map to
// one event, many to none.
func (p *eventProjector) project(ev embeddedchat.Event) []map[string]any {
	switch {
	case ev.Err != nil:
		return one(map[string]any{"type": "error", "message": ev.Err.Error()})
	case ev.Done:
		return one(map[string]any{"type": "finish", "reason": "stop"})
	case ev.Elicitation != nil:
		e := ev.Elicitation
		return one(map[string]any{
			"type":                  "elicitation",
			"id":                    e.ElicitationID,
			"server_elicitation_id": e.ServerElicitationID,
			"message":               e.Message,
			"mode":                  e.Mode,
			"url":                   e.URL,
			"schema":                plain(e.Schema),
			"meta":                  plain(e.Meta),
		})
	}

	switch e := ev.RuntimeEvent.(type) {
	case *runtime.AgentChoiceEvent:
		if e.Content == "" {
			return nil
		}
		return one(map[string]any{"type": "delta", "content": e.Content})
	case *runtime.AgentChoiceReasoningEvent:
		if e.Content == "" {
			return nil
		}
		return one(map[string]any{"type": "delta", "reasoning": e.Content})
	case *runtime.PartialToolCallEvent:
		return one(map[string]any{
			"type":      "tool_call_delta",
			"id":        e.ToolCall.ID,
			"name":      e.ToolCall.Function.Name,
			"arguments": e.ToolCall.Function.Arguments,
		})
	case *runtime.ToolCallEvent:
		return one(toolCallEvent("tool_call", e.ToolCall))
	case *runtime.ToolCallConfirmationEvent:
		return one(toolCallEvent("tool_confirmation", e.ToolCall))
	case *runtime.ToolCallOutputEvent:
		if e.Output == "" {
			return nil
		}
		return one(map[string]any{"type": "tool_output", "id": e.ToolCallID, "name": e.ToolDefinition.Name, "output": e.Output})
	case *runtime.ToolCallResponseEvent:
		return one(map[string]any{
			"type":     "tool_result",
			"id":       e.ToolCallID,
			"name":     e.ToolDefinition.Name,
			"output":   e.Response,
			"is_error": e.Result != nil && e.Result.IsError,
		})
	case *runtime.HookBlockedEvent:
		return one(map[string]any{"type": "tool_blocked", "id": e.ToolCall.ID, "name": e.ToolCall.Function.Name, "reason": e.Message})
	case *runtime.AgentSwitchingEvent:
		p.agent = e.ToAgent
		return one(map[string]any{"type": "handoff", "from": e.FromAgent, "to": e.ToAgent})
	case *runtime.AgentInfoEvent:
		from := p.agent
		p.agent = e.AgentName
		if from == "" || from == e.AgentName {
			return nil
		}
		return one(map[string]any{"type": "handoff", "from": from, "to": e.AgentName})
	case *runtime.ModelFallbackEvent:
		return one(map[string]any{
			"type":    "fallback",
			"from":    e.FailedModel,
			"to":      e.FallbackModel,
			"attempt": e.Attempt,
			"reason":  e.Reason,
		})
	case *runtime.TokenUsageEvent:
		// Per model call, like the original API; the session totals ride along.
		if e.Usage == nil || e.Usage.LastMessage == nil {
			return nil
		}
		return one(map[string]any{
			"type":           "usage",
			"input_tokens":   e.Usage.LastMessage.InputTokens,
			"output_tokens":  e.Usage.LastMessage.OutputTokens,
			"context_length": e.Usage.ContextLength,
			"context_limit":  e.Usage.ContextLimit,
			"cost":           e.Usage.Cost,
		})
	case *runtime.WarningEvent:
		return one(map[string]any{"type": "warning", "message": e.Message})
	}
	return nil
}

func one(event map[string]any) []map[string]any { return []map[string]any{event} }

func toolCallEvent(kind string, call tools.ToolCall) map[string]any {
	return map[string]any{"type": kind, "id": call.ID, "name": call.Function.Name, "args": call.Function.Arguments}
}

// plain re-encodes an arbitrary Go value (an MCP elicitation schema, meta)
// into the maps, slices and scalars js.ValueOf accepts.
func plain(v any) any {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}
