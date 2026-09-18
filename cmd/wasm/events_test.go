//go:build js && wasm

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/embeddedchat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func raw(event runtime.Event) embeddedchat.Event { return embeddedchat.Event{RuntimeEvent: event} }

func TestProjectContentAndControlEvents(t *testing.T) {
	var p eventProjector
	assert.Equal(t, []map[string]any{{"type": "delta", "content": "hi"}}, p.project(raw(runtime.AgentChoice("root", "s", "hi"))))
	assert.Nil(t, p.project(raw(runtime.AgentChoice("root", "s", ""))))
	assert.Equal(t, []map[string]any{{"type": "delta", "reasoning": "hmm"}}, p.project(raw(runtime.AgentChoiceReasoning("root", "s", "hmm"))))
	assert.Equal(t, []map[string]any{{"type": "error", "message": "boom"}}, p.project(embeddedchat.Event{Err: errors.New("boom")}))
	assert.Equal(t, []map[string]any{{"type": "finish", "reason": "stop"}}, p.project(embeddedchat.Event{Done: true}))
	assert.Equal(t, []map[string]any{{"type": "warning", "message": "careful"}}, p.project(raw(runtime.Warning("careful", "root"))))
	assert.Nil(t, p.project(raw(runtime.StreamStarted("s", "root"))), "lifecycle events are not forwarded")
}

func TestProjectToolEvents(t *testing.T) {
	var p eventProjector
	call := tools.ToolCall{ID: "c1", Function: tools.FunctionCall{Name: "echo", Arguments: `{"text":"x"}`}}
	def := tools.Tool{Name: "echo"}

	assert.Equal(t, []map[string]any{{"type": "tool_call_delta", "id": "c1", "name": "echo", "arguments": `{"text":"x"}`}}, p.project(raw(runtime.PartialToolCall(call, def, "root"))))
	assert.Equal(t, []map[string]any{{"type": "tool_call", "id": "c1", "name": "echo", "args": `{"text":"x"}`}}, p.project(raw(runtime.ToolCall(call, def, "root"))))
	assert.Equal(t, []map[string]any{{"type": "tool_confirmation", "id": "c1", "name": "echo", "args": `{"text":"x"}`}}, p.project(raw(runtime.ToolCallConfirmation(call, def, "root", nil))))
	assert.Equal(t, []map[string]any{{"type": "tool_output", "id": "c1", "name": "echo", "output": "line"}}, p.project(raw(runtime.ToolCallOutput("c1", def, "line", "root"))))
	assert.Equal(t, []map[string]any{{"type": "tool_result", "id": "c1", "name": "echo", "output": "bad", "is_error": true}}, p.project(raw(runtime.ToolCallResponse("c1", def, tools.ResultError("bad"), "bad", "root"))))
	assert.Equal(t, []map[string]any{{"type": "tool_blocked", "id": "c1", "name": "echo", "reason": "denied"}}, p.project(raw(runtime.HookBlocked(call, def, "denied", "root"))))
}

func TestProjectHandoffsFromAgentInfo(t *testing.T) {
	var p eventProjector
	assert.Nil(t, p.project(raw(runtime.AgentInfo("root", "mock/root", "", ""))), "the first agent is not a handoff")
	assert.Nil(t, p.project(raw(runtime.AgentInfo("root", "mock/root", "", ""))), "same agent again is not a handoff")
	assert.Equal(t, []map[string]any{{"type": "handoff", "from": "root", "to": "helper"}}, p.project(raw(runtime.AgentInfo("helper", "mock/helper", "", ""))))

	assert.Equal(t, []map[string]any{{"type": "handoff", "from": "helper", "to": "worker"}}, p.project(raw(runtime.AgentSwitching(true, "helper", "worker"))))
	assert.Nil(t, p.project(raw(runtime.AgentInfo("worker", "mock/worker", "", ""))), "the AgentInfo following a switch is not a second handoff")
	assert.Equal(t, []map[string]any{{"type": "handoff", "from": "worker", "to": "helper"}}, p.project(raw(runtime.AgentSwitching(false, "worker", "helper"))))
}

func TestProjectFallbackUsageAndElicitation(t *testing.T) {
	var p eventProjector
	assert.Equal(t, []map[string]any{{"type": "fallback", "from": "a", "to": "b", "attempt": 2, "reason": "down"}}, p.project(raw(runtime.ModelFallback("root", "a", "b", "down", 2, 3))))
	assert.Equal(t, []map[string]any{{
		"type": "usage", "input_tokens": int64(10), "output_tokens": int64(5), "context_length": int64(150), "context_limit": int64(1000), "cost": 0.5,
	}}, p.project(raw(runtime.NewTokenUsageEvent("s", "root", &runtime.Usage{
		InputTokens: 100, OutputTokens: 50, ContextLength: 150, ContextLimit: 1000, Cost: 0.5,
		LastMessage: &runtime.MessageUsage{Usage: chat.Usage{InputTokens: 10, OutputTokens: 5}},
	}))))
	assert.Nil(t, p.project(raw(runtime.NewTokenUsageEvent("s", "root", &runtime.Usage{InputTokens: 100}))), "session-only snapshots carry no per-call usage")
	assert.Nil(t, p.project(raw(runtime.NewTokenUsageEvent("s", "root", nil))))

	schema := map[string]any{"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string"}}}
	event := runtime.ElicitationRequest("Sign in", "url", schema, "https://auth.example.com", "el-1", "srv-1", "s", map[string]any{"authorize_url": "https://auth.example.com"}, "root").(*runtime.ElicitationRequestEvent)
	projected := p.project(embeddedchat.Event{RuntimeEvent: event, Elicitation: event})
	require.Len(t, projected, 1)
	assert.Equal(t, "elicitation", projected[0]["type"])
	assert.Equal(t, "el-1", projected[0]["id"])
	assert.Equal(t, "srv-1", projected[0]["server_elicitation_id"])
	assert.Equal(t, "url", projected[0]["mode"])
	assert.Equal(t, "https://auth.example.com", projected[0]["url"])
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string"}}}, projected[0]["schema"])
	assert.Equal(t, map[string]any{"authorize_url": "https://auth.example.com"}, projected[0]["meta"])
}
