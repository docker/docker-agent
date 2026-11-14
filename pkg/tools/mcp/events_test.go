package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type eventRuntime struct {
	tools.NopRuntime

	messages chan string
}

func (r *eventRuntime) Supports(capability tools.Capability) bool {
	return capability == tools.CapabilityRecall
}

func (r *eventRuntime) Recall(_ context.Context, message string) error {
	r.messages <- message
	return nil
}

func TestEventsDemoIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := newStdioCmdClient("go", []string{"run", "../../../examples/mcp-events"}, nil, "")
	client.setExperimentalEvents(true)
	_, err := client.Initialize(ctx, nil)
	require.NoError(t, err)
	defer client.Close(ctx)
	localTools := client.eventTools("demo")
	require.Len(t, localTools, 3)
	rt := &eventRuntime{messages: make(chan string, 10)}
	result, err := localTools[0].Handler(ctx, tools.ToolCall{}, rt)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "demo.message")
	result, err = localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message","arguments":{"interval_seconds":1,"count":2}}`}}, rt)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "event-1")
	for range 2 {
		select {
		case message := <-rt.messages:
			assert.Contains(t, message, "untrusted external data")
			assert.Contains(t, message, `"name":"demo.message"`)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Eventually(t, func() bool {
		client.events.mu.Lock()
		defer client.events.mu.Unlock()
		return len(client.events.subscriptions) == 0
	}, 5*time.Second, 10*time.Millisecond)
	_, err = localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message","arguments":{"interval_seconds":-1}}`}}, rt)
	require.ErrorContains(t, err, "interval_seconds")
	_, err = localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"missing"}`}}, rt)
	require.ErrorContains(t, err, "does not support push")
	_, err = localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message"}`}}, tools.NopRuntime{})
	require.ErrorIs(t, err, tools.ErrRecallNotSupported)
	_, err = localTools[1].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"demo.message","arguments":{"interval_seconds":10,"count":100}}`}}, rt)
	require.NoError(t, err)
	_, err = localTools[2].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"id":"event-3"}`}}, rt)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		client.events.mu.Lock()
		defer client.events.mu.Unlock()
		return len(client.events.subscriptions) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestEventToolsAbsentForOrdinaryServer(t *testing.T) {
	client := newStdioCmdClient("go", []string{"run", "./testdata/testserver.go"}, nil, "")
	// A missing session must never advertise event tools.
	assert.Empty(t, client.eventTools("test"))
}

type eventTestConnection struct {
	gomcp.Connection

	mu      sync.Mutex
	written jsonrpc.Message
}

func (c *eventTestConnection) Write(_ context.Context, msg jsonrpc.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = msg
	return nil
}

func TestEventCorrelation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := &eventSubscription{name: "demo.message", cancel: cancel, messages: make(chan *jsonrpc.Request, 1)}
	events := &eventClient{subscriptions: map[string]*eventSubscription{"event-1": sub}}
	conn := &eventTestConnection{}
	adapter := &eventConnection{Connection: conn, events: events}
	msg, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":12,"method":"events/stream","params":{"name":"demo.message","_meta":{"docker-agent/eventSubscription":"event-1"}}}`))
	require.NoError(t, err)
	require.NoError(t, adapter.Write(ctx, msg))
	assert.Equal(t, "12", sub.wireID)
	wire, err := jsonrpc.EncodeMessage(conn.written)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), localSubscriptionMeta)
	notification := func(id int) *jsonrpc.Request {
		params, marshalErr := json.Marshal(map[string]any{"_meta": map[string]any{subscriptionIDMeta: id}})
		require.NoError(t, marshalErr)
		return &jsonrpc.Request{Method: "notifications/events/active", Params: params}
	}
	events.receive(notification(99))
	assert.Empty(t, sub.messages)
	events.receive(notification(12))
	assert.Len(t, sub.messages, 1)
	events.receive(notification(12))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestEventConnectionCloseCancelsSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	events := &eventClient{subscriptions: map[string]*eventSubscription{"one": {cancel: cancel}}}
	events.close()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.True(t, events.closed)
}

func TestEventsDemoBuild(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", t.TempDir()+"/events-demo", "../../../examples/mcp-events")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

var _ io.Closer = (*eventConnection)(nil)
