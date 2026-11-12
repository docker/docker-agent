package ui

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

// ToolResult is the runtime-free data needed to finish a tool call view.
type ToolResult struct {
	Response       string
	Result         *tools.ToolCallResult
	AgentName      string
	ToolDefinition tools.Tool
	Images         []InlineImage
}

// ToolTracker holds the render state of in-flight tool calls, keyed by id and
// kept in call order so the conversation shows them as they arrive.
type ToolTracker struct {
	byID  map[string]*ToolView
	order []string
}

func NewToolTracker() *ToolTracker {
	return &ToolTracker{byID: map[string]*ToolView{}}
}

// Reset clears all tracked tool calls.
func (t *ToolTracker) Reset() {
	t.byID = map[string]*ToolView{}
	t.order = nil
}

// Empty reports whether there are no tracked tool calls.
func (t *ToolTracker) Empty() bool { return len(t.order) == 0 }

// Get returns a tracked tool call by id.
func (t *ToolTracker) Get(id string) *ToolView { return t.byID[id] }

// Len reports the number of tracked tool calls.
func (t *ToolTracker) Len() int { return len(t.order) }

// ByIDLen reports the number of tracked tool-call ids.
func (t *ToolTracker) ByIDLen() int { return len(t.byID) }

// ForEach visits the tracked tools in call order, skipping nil entries.
func (t *ToolTracker) ForEach(fn func(*ToolView)) {
	for _, id := range t.order {
		if tv := t.byID[id]; tv != nil {
			fn(tv)
		}
	}
}

// Remove deletes a tracked tool call by id.
func (t *ToolTracker) Remove(id string) {
	if id == "" {
		return
	}
	delete(t.byID, id)
	t.order = slices.DeleteFunc(t.order, func(s string) bool { return s == id })
}

// Upsert creates or updates a tracked tool call. Argument fragments streamed
// while the call is still pending are concatenated.
func (t *ToolTracker) Upsert(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status tuitypes.ToolStatus) {
	id := ToolViewID(toolCall)
	tv := t.byID[id]
	if tv == nil {
		tv = NewToolView(agentName, toolCall, toolDef, status)
		t.byID[id] = tv
		t.order = append(t.order, id)
		return
	}

	msg := tv.message
	if msg == nil {
		msg = NewToolView(agentName, toolCall, toolDef, status).message
		tv.message = msg
		return
	}

	if agentName != "" {
		msg.Sender = agentName
	}
	if toolDef.Name != "" || toolCall.Function.Name != "" {
		definition := EnsureToolDefinition(toolCall, toolDef)
		if definition.Annotations.Title == "" && definition.Name == msg.ToolDefinition.Name {
			definition.Annotations.Title = msg.ToolDefinition.Annotations.Title
		}
		msg.ToolDefinition = definition
	}
	msg.SetToolStatus(status)
	if toolCall.ID != "" {
		msg.ToolCall.ID = toolCall.ID
	}
	if toolCall.Type != "" {
		msg.ToolCall.Type = toolCall.Type
	}
	if toolCall.Function.Name != "" {
		msg.ToolCall.Function.Name = toolCall.Function.Name
	}
	if toolCall.Function.Arguments != "" {
		if status == tuitypes.ToolStatusPending {
			msg.ToolCall.Function.Arguments += toolCall.Function.Arguments
		} else {
			msg.ToolCall.Function.Arguments = toolCall.Function.Arguments
		}
	}
}

// Finish marks a tool call complete and returns an immutable snapshot. It
// returns nil when there is nothing to render.
func (t *ToolTracker) Finish(id string, result ToolResult) *ToolView {
	tv := t.byID[id]
	if tv == nil {
		toolCall := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: result.ToolDefinition.Name}}
		tv = NewToolView(result.AgentName, toolCall, result.ToolDefinition, tuitypes.ToolStatusCompleted)
	}
	if tv.message == nil {
		return nil
	}

	status := tuitypes.ToolStatusCompleted
	if result.Result != nil && result.Result.IsError {
		status = tuitypes.ToolStatusError
	}
	tv.message.SetToolStatus(status)
	definition := EnsureToolDefinition(tv.message.ToolCall, result.ToolDefinition)
	if definition.Annotations.Title == "" && definition.Name == tv.message.ToolDefinition.Name {
		definition.Annotations.Title = tv.message.ToolDefinition.Annotations.Title
	}
	tv.message.ToolDefinition = definition
	tv.message.Content = strings.ReplaceAll(result.Response, "\t", "    ")
	tv.message.ToolResult = result.Result.WithoutPayload()
	tv.images = result.Images

	msg := *tv.message
	snapshot := &ToolView{message: &msg, images: tv.images, renderers: tv.renderers, writeArgs: tv.writeArgs, fullFilePreview: tv.fullFilePreview}
	t.Remove(id)
	return snapshot
}

func (t *ToolTracker) Complete(id string, result ToolResult) {
	order := slices.Clone(t.order)
	if !slices.Contains(order, id) {
		order = append(order, id)
	}
	view := t.Finish(id, result)
	if view == nil {
		return
	}
	t.byID[id] = view
	t.order = order
}

// DrainCompleted commits only the completed prefix, preserving invocation order.
func (t *ToolTracker) DrainCompleted() []*ToolView {
	var views []*ToolView
	for len(t.order) > 0 {
		id := t.order[0]
		view := t.byID[id]
		if view == nil || view.message == nil || view.message.ToolStatus != tuitypes.ToolStatusCompleted && view.message.ToolStatus != tuitypes.ToolStatusError {
			break
		}
		views = append(views, view)
		t.Remove(id)
	}
	return views
}

// FinalizeAll marks every in-flight tool with a terminal status, returns
// immutable snapshots in call order, and clears the tracker.
func (t *ToolTracker) FinalizeAll(status tuitypes.ToolStatus) []*ToolView {
	views := make([]*ToolView, 0, len(t.order))
	t.ForEach(func(tv *ToolView) {
		if tv.message == nil {
			return
		}
		if tv.message.ToolStatus == tuitypes.ToolStatusPending || tv.message.ToolStatus == tuitypes.ToolStatusRunning {
			tv.message.SetToolStatus(status)
		}
		if tv.message.ToolStatus == tuitypes.ToolStatusError && tv.message.Content == "" {
			tv.message.Content = "Tool call ended before a result was received."
		}
		msg := *tv.message
		views = append(views, &ToolView{message: &msg, images: tv.images, renderers: tv.renderers, writeArgs: tv.writeArgs, fullFilePreview: tv.fullFilePreview})
	})
	t.Reset()
	return views
}

// ToolViewID returns a stable id for a tool call view.
func ToolViewID(toolCall tools.ToolCall) string {
	if toolCall.ID != "" {
		return toolCall.ID
	}
	return toolCall.Function.Name
}
