package a2a

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"testing"

	a2av1 "github.com/a2aproject/a2a-go/v2/a2a"
	a2asrvv1 "github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/docker/docker-agent/pkg/tools"
)

// `docker agent serve a2a` publishes A2A 1.0 cards (supportedInterfaces, no top-level url).
func TestToolSetCallsA2AV1Server(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	card := &a2av1.AgentCard{
		Name:                "test",
		Description:         "test",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2av1.AgentInterface{a2av1.NewAgentInterface(server.URL+"/invoke", a2av1.TransportProtocolJSONRPC)},
		Capabilities:        a2av1.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2av1.AgentSkill{{ID: "test", Name: "test", Description: "test", Tags: []string{"test"}}},
	}
	mux.Handle(a2asrvv1.WellKnownAgentCardPath, a2asrvv1.NewStaticAgentCardHandler(card))
	mux.Handle("/invoke", a2asrvv1.NewJSONRPCHandler(a2asrvv1.NewHandler(testV1Executor{})))

	toolSet := NewToolset("test", server.URL, nil, WithAllowPrivateIPs(true))
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

type testV1Executor struct{}

func (testV1Executor) Execute(_ context.Context, execCtx *a2asrvv1.ExecutorContext) iter.Seq2[a2av1.Event, error] {
	return func(yield func(a2av1.Event, error) bool) {
		yield(a2av1.NewMessageForTask(a2av1.MessageRoleAgent, execCtx, a2av1.NewTextPart("ok")), nil)
	}
}

func (testV1Executor) Cancel(context.Context, *a2asrvv1.ExecutorContext) iter.Seq2[a2av1.Event, error] {
	return func(func(a2av1.Event, error) bool) {}
}
