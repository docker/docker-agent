package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	goa2a "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolSetRejectsPrivateIPForAgentCard(t *testing.T) {
	t.Parallel()

	toolSet := NewToolset("test", "http://127.0.0.1/.well-known/agent-card.json", nil)

	if err := toolSet.Start(t.Context()); err == nil {
		t.Fatal("Start() expected error")
	}
}

func TestToolSetStreamingWithAllowPrivateIPs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(testA2AHandler{})))
	t.Cleanup(server.Close)

	cardServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(goa2a.AgentCard{
			Name:               "test",
			Description:        "test",
			URL:                server.URL,
			Version:            "1.0.0",
			ProtocolVersion:    string(goa2a.Version),
			PreferredTransport: goa2a.TransportProtocolJSONRPC,
			Capabilities:       goa2a.AgentCapabilities{Streaming: true},
			DefaultInputModes:  []string{"text/plain"},
			DefaultOutputModes: []string{"text/plain"},
			Skills: []goa2a.AgentSkill{{
				ID:          "test",
				Name:        "test",
				Description: "test",
				Tags:        []string{"test"},
			}},
		})
	}))
	t.Cleanup(cardServer.Close)

	toolSet := NewToolset("test", cardServer.URL, nil, WithAllowPrivateIPs(true))

	if err := toolSet.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	toolList, err := toolSet.Tools(t.Context())
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(toolList) != 1 {
		t.Fatalf("Tools() returned %d tools, want 1", len(toolList))
	}

	result, err := toolList[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"message":"hello"}`}}, tools.NopRuntime{})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if result == nil || result.Output != "ok" {
		t.Fatalf("Handler() result = %+v, want output %q", result, "ok")
	}
}

type testA2AHandler struct{}

func (testA2AHandler) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	return queue.Write(ctx, goa2a.NewMessageForTask(goa2a.MessageRoleAgent, reqCtx, goa2a.TextPart{Text: "ok"}))
}

func (testA2AHandler) Cancel(context.Context, *a2asrv.RequestContext, eventqueue.Queue) error {
	return nil
}

func TestToolSetEmptyStreamReturnsError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(testEmptyA2AHandler{})))
	t.Cleanup(server.Close)

	cardServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(goa2a.AgentCard{
			Name:               "test",
			Description:        "test",
			URL:                server.URL,
			Version:            "1.0.0",
			ProtocolVersion:    string(goa2a.Version),
			PreferredTransport: goa2a.TransportProtocolJSONRPC,
			Capabilities:       goa2a.AgentCapabilities{Streaming: true},
			DefaultInputModes:  []string{"text/plain"},
			DefaultOutputModes: []string{"text/plain"},
			Skills: []goa2a.AgentSkill{{
				ID:          "test",
				Name:        "test",
				Description: "test",
				Tags:        []string{"test"},
			}},
		})
	}))
	t.Cleanup(cardServer.Close)

	toolSet := NewToolset("test", cardServer.URL, nil, WithAllowPrivateIPs(true))

	if err := toolSet.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	toolList, err := toolSet.Tools(t.Context())
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(toolList) != 1 {
		t.Fatalf("Tools() returned %d tools, want 1", len(toolList))
	}

	result, err := toolList[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"message":"hello"}`}}, tools.NopRuntime{})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if !result.IsError || result.Output != "No response from agent" {
		t.Fatalf("Handler() result = %+v, want output %q with IsError true", result, "No response from agent")
	}
}

type testEmptyA2AHandler struct{}

func (testEmptyA2AHandler) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	// Write a message with empty text so the HTTP stream closes cleanly.
	// extractText will return "", causing the handler to return ResultError("No response from agent").
	return queue.Write(ctx, goa2a.NewMessageForTask(goa2a.MessageRoleAgent, reqCtx, goa2a.TextPart{Text: ""}))
}

func (testEmptyA2AHandler) Cancel(context.Context, *a2asrv.RequestContext, eventqueue.Queue) error {
	return nil
}
