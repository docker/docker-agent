package acp

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

type toolCallKey struct {
	agent string
	id    string
}

type toolCallState struct {
	id       acp.ToolCallId
	rejected bool
	result   *runtime.ToolCallResponseEvent
}

type toolCallTracker struct {
	active map[toolCallKey]*toolCallState
}

func (t *toolCallTracker) report(ctx context.Context, a *Agent, s *Session, agentName string, call tools.ToolCall, definition tools.Tool, status acp.ToolCallStatus) (*toolCallState, error) {
	if call.ID == "" {
		return nil, errors.New("tool call ID is required")
	}
	key := toolCallKey{agent: agentName, id: call.ID}
	state, exists := t.active[key]
	if exists && state.rejected && status == acp.ToolCallStatusInProgress {
		// Out-of-band rejections can synthesize a start without executing anything.
		return state, nil
	}
	if !exists {
		state = &toolCallState{id: acp.ToolCallId(uuid.NewV4().String())}
		if t.active == nil {
			t.active = make(map[toolCallKey]*toolCallState)
		}
	}
	call.ID = string(state.id)
	workingDir, _ := s.workspaceSnapshot()
	var update acp.SessionUpdate
	if exists {
		fields := buildToolCallUpdate(call, definition, status, workingDir)
		update = acp.UpdateToolCall(state.id,
			acp.WithUpdateTitle(*fields.Title), acp.WithUpdateKind(*fields.Kind),
			acp.WithUpdateStatus(status), acp.WithUpdateRawInput(fields.RawInput), acp.WithUpdateLocations(fields.Locations),
		)
	} else {
		update = buildToolCallStart(call, definition, workingDir)
		update.ToolCall.Status = status
	}
	if err := a.sendUpdate(ctx, s.id, update); err != nil {
		return nil, err
	}
	t.active[key] = state
	return state, nil
}

func (t *toolCallTracker) complete(ctx context.Context, a *Agent, s *Session, event *runtime.ToolCallResponseEvent) error {
	if event.ToolCallID == "" {
		return errors.New("tool call ID is required")
	}
	key := toolCallKey{agent: event.AgentName, id: event.ToolCallID}
	state, exists := t.active[key]
	if !exists {
		state = &toolCallState{id: acp.ToolCallId(uuid.NewV4().String())}
	}
	mappedEvent := *event
	mappedEvent.ToolCallID = string(state.id)
	update := buildToolCallComplete(&mappedEvent)
	if !exists {
		fields := update.ToolCallUpdate
		title := event.ToolDefinition.Annotations.Title
		if title == "" {
			title = event.ToolDefinition.Name
		}
		if title == "" {
			title = "Tool call"
		}
		update = acp.StartToolCall(state.id, title,
			acp.WithStartKind(determineToolKind(event.ToolDefinition.Name, event.ToolDefinition)),
			acp.WithStartStatus(*fields.Status), acp.WithStartContent(fields.Content), acp.WithStartRawOutput(fields.RawOutput),
		)
	}
	// Never retry a terminal write: a writer error can mean partial delivery.
	delete(t.active, key)
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return a.sendUpdate(sendCtx, s.id, update)
}

func (t *toolCallTracker) retainResult(event runtime.Event) {
	if response, ok := event.(*runtime.ToolCallResponseEvent); ok {
		if state := t.active[toolCallKey{agent: response.AgentName, id: response.ToolCallID}]; state != nil {
			state.result = response
		}
	}
}

func (t *toolCallTracker) interrupt(ctx context.Context, a *Agent, s *Session) error {
	for key, state := range t.active {
		if state.result != nil {
			if err := t.complete(ctx, a, s, state.result); err != nil {
				return err
			}
			continue
		}
		update := acp.UpdateToolCall(state.id,
			acp.WithUpdateStatus(acp.ToolCallStatusFailed),
			acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Tool call interrupted before a result was available; side effects may have occurred."))}),
		)
		if err := a.sendUpdate(ctx, s.id, update); err != nil {
			return err
		}
		delete(t.active, key)
	}
	return nil
}
