package acp

import (
	"context"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/harness"
)

type collectSink struct {
	events []harness.Event
}

func (c *collectSink) Emit(e harness.Event) {
	c.events = append(c.events, e)
}

func newClient(sink harness.EventSink) *acpClient {
	return &acpClient{
		runID:  "test-run",
		events: sink,
	}
}

func TestSessionUpdateTextChunk(t *testing.T) {
	sink := &collectSink{}
	client := newClient(sink)

	text := "hello world"
	msgID := "msg-001"
	err := client.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.SessionUpdate{
			AgentMessageChunk: &acpsdk.SessionUpdateAgentMessageChunk{
				MessageId: &msgID,
				Content: acpsdk.ContentBlock{
					Text: &acpsdk.ContentBlockText{Text: text},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(sink.events))
	}
	delta, ok := sink.events[0].(harness.TextDelta)
	if !ok {
		t.Fatalf("expected TextDelta, got %T", sink.events[0])
	}
	if delta.Delta != text {
		t.Errorf("Delta = %q, want %q", delta.Delta, text)
	}
	if delta.MessageID != msgID {
		t.Errorf("MessageID = %q, want %q", delta.MessageID, msgID)
	}
}

func TestSessionUpdateThoughtChunk(t *testing.T) {
	sink := &collectSink{}
	client := newClient(sink)

	thought := "let me think..."
	err := client.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.SessionUpdate{
			AgentThoughtChunk: &acpsdk.SessionUpdateAgentThoughtChunk{
				Content: acpsdk.ContentBlock{
					Text: &acpsdk.ContentBlockText{Text: thought},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(sink.events))
	}
	delta, ok := sink.events[0].(harness.ReasoningDelta)
	if !ok {
		t.Fatalf("expected ReasoningDelta, got %T", sink.events[0])
	}
	if delta.Delta != thought {
		t.Errorf("Delta = %q, want %q", delta.Delta, thought)
	}
}

func TestSessionUpdateToolCallRunning(t *testing.T) {
	sink := &collectSink{}
	client := newClient(sink)

	err := client.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.SessionUpdate{
			ToolCall: &acpsdk.SessionUpdateToolCall{
				ToolCallId:    "tc-001",
				Title:         "Read file",
				Status:        acpsdk.ToolCallStatusInProgress,
				SessionUpdate: "toolCall",
			},
		},
	})
	if err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(sink.events))
	}
	start, ok := sink.events[0].(harness.ToolCallStart)
	if !ok {
		t.Fatalf("expected ToolCallStart, got %T", sink.events[0])
	}
	if start.ToolCallID != "tc-001" {
		t.Errorf("ToolCallID = %q, want tc-001", start.ToolCallID)
	}
}

func TestSessionUpdateToolCallCompleted(t *testing.T) {
	sink := &collectSink{}
	client := newClient(sink)

	err := client.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.SessionUpdate{
			ToolCall: &acpsdk.SessionUpdateToolCall{
				ToolCallId:    "tc-001",
				Title:         "Read file",
				Status:        acpsdk.ToolCallStatusCompleted,
				SessionUpdate: "toolCall",
			},
		},
	})
	if err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(sink.events))
	}
	end, ok := sink.events[0].(harness.ToolCallEnd)
	if !ok {
		t.Fatalf("expected ToolCallEnd, got %T", sink.events[0])
	}
	if end.ToolCallID != "tc-001" {
		t.Errorf("ToolCallID = %q, want tc-001", end.ToolCallID)
	}
}

func TestSessionUpdateEmpty(t *testing.T) {
	sink := &collectSink{}
	client := newClient(sink)

	err := client.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.SessionUpdate{},
	})
	if err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}
	if len(sink.events) != 0 {
		t.Errorf("expected 0 events for empty update, got %d", len(sink.events))
	}
}

func TestEventTimestamps(t *testing.T) {
	before := time.Now()
	sink := &collectSink{}
	client := newClient(sink)

	_ = client.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.SessionUpdate{
			AgentMessageChunk: &acpsdk.SessionUpdateAgentMessageChunk{
				Content: acpsdk.ContentBlock{
					Text: &acpsdk.ContentBlockText{Text: "hi"},
				},
			},
		},
	})
	after := time.Now()

	if len(sink.events) == 0 {
		t.Fatal("no events")
	}
	at := sink.events[0].EventTime()
	if at.Before(before) || at.After(after) {
		t.Errorf("EventTime %v not in range [%v, %v]", at, before, after)
	}
}
