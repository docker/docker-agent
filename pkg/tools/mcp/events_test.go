package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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

	messages  chan string
	recallErr func(message string) error
}

func (r *eventRuntime) Supports(capability tools.Capability) bool {
	return capability == tools.CapabilityRecall
}

func (r *eventRuntime) Recall(_ context.Context, message string) error {
	if r.recallErr != nil {
		if err := r.recallErr(message); err != nil {
			return err
		}
	}
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

	// Large numeric IDs (>= 1,000,000) must not drift to scientific notation.
	bigCtx, bigCancel := context.WithCancel(t.Context())
	defer bigCancel()
	bigSub := &eventSubscription{name: "demo.message", cancel: bigCancel, messages: make(chan *jsonrpc.Request, 2)}
	bigEvents := &eventClient{subscriptions: map[string]*eventSubscription{"event-big": bigSub}}
	bigMsg, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1000000,"method":"events/stream","params":{"name":"demo.message","_meta":{"docker-agent/eventSubscription":"event-big"}}}`))
	require.NoError(t, err)
	bigConn := &eventTestConnection{}
	require.NoError(t, (&eventConnection{Connection: bigConn, events: bigEvents}).Write(bigCtx, bigMsg))
	assert.Equal(t, "1000000", bigSub.wireID)
	bigEvents.receive(notification(1000000))
	assert.Len(t, bigSub.messages, 1)

	// Oversized raw notifications (including unknown fields) cancel before enqueueing.
	oversizedParams, err := json.Marshal(map[string]any{
		"_meta":   map[string]any{subscriptionIDMeta: 1000000},
		"unknown": strings.Repeat("x", maxEventPayloadBytes+1),
	})
	require.NoError(t, err)
	bigEvents.receive(&jsonrpc.Request{Method: "notifications/events/event", Params: oversizedParams})
	require.ErrorIs(t, bigCtx.Err(), context.Canceled)
	assert.Len(t, bigSub.messages, 1)
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

func mustEventNotification(t *testing.T, method, wireID string, fields map[string]any) *jsonrpc.Request {
	t.Helper()
	fields["_meta"] = map[string]any{subscriptionIDMeta: wireID}
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return &jsonrpc.Request{Method: method, Params: raw}
}

func waitNoSubscriptions(t *testing.T, events *eventClient) {
	t.Helper()
	require.Eventually(t, func() bool {
		events.mu.Lock()
		defer events.mu.Unlock()
		return len(events.subscriptions) == 0
	}, 2*time.Second, 5*time.Millisecond)
}

func bindWireID(events *eventClient, p *eventParams, wireID string) {
	token, _ := p.Meta[localSubscriptionMeta].(string)
	events.mu.Lock()
	defer events.mu.Unlock()
	if sub := events.subscriptions[token]; sub != nil {
		sub.wireID = wireID
	}
}

func TestEventStateMachineConfirmationTimeout(t *testing.T) {
	events := &eventClient{
		subscriptions:  make(map[string]*eventSubscription),
		idleTimeout:    25 * time.Millisecond,
		retryBaseDelay: 5 * time.Millisecond,
		retryMaxDelay:  10 * time.Millisecond,
		streamCall: func(ctx context.Context, _ *gomcp.ClientSession, _ *eventParams) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	rt := &eventRuntime{messages: make(chan string, 4)}
	_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
	require.ErrorContains(t, err, "confirmation timed out")
	waitNoSubscriptions(t, events)
}

func TestEventStateMachineIdleTimeoutReopensAndDeduplicates(t *testing.T) {
	var attempts atomic.Int32
	var cursorsMu sync.Mutex
	var cursors []string

	var events *eventClient
	events = &eventClient{
		subscriptions:  make(map[string]*eventSubscription),
		idleTimeout:    40 * time.Millisecond,
		retryBaseDelay: 5 * time.Millisecond,
		retryMaxDelay:  10 * time.Millisecond,
		streamCall: func(ctx context.Context, _ *gomcp.ClientSession, p *eventParams) error {
			attempt := attempts.Add(1)
			cursorsMu.Lock()
			if p.Cursor != nil {
				cursors = append(cursors, *p.Cursor)
			} else {
				cursors = append(cursors, "")
			}
			cursorsMu.Unlock()
			bindWireID(events, p, "w1")
			now := time.Now().UTC().Format(time.RFC3339Nano)
			switch attempt {
			case 1:
				events.receive(mustEventNotification(t, "notifications/events/active", "w1", map[string]any{"cursor": "c1"}))
				events.receive(mustEventNotification(t, "notifications/events/event", "w1", map[string]any{
					"eventId": "e1", "name": "demo.message", "timestamp": now, "cursor": "c2", "data": map[string]any{"seq": 1},
				}))
				<-ctx.Done()
				return ctx.Err()
			case 2:
				events.receive(mustEventNotification(t, "notifications/events/active", "w1", map[string]any{"cursor": "c2"}))
				// Duplicate eventId must be suppressed.
				events.receive(mustEventNotification(t, "notifications/events/event", "w1", map[string]any{
					"eventId": "e1", "name": "demo.message", "timestamp": now, "cursor": "c2", "data": map[string]any{"seq": 1},
				}))
				events.receive(mustEventNotification(t, "notifications/events/event", "w1", map[string]any{
					"eventId": "e2", "name": "demo.message", "timestamp": now, "cursor": "c3", "data": map[string]any{"seq": 2},
				}))
				return nil
			default:
				t.Errorf("unexpected extra attempt %d", attempt)
				return nil
			}
		},
	}

	rt := &eventRuntime{messages: make(chan string, 4)}
	id, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
	require.NoError(t, err)
	assert.Equal(t, "event-1", id)

	waitNoSubscriptions(t, events)
	close(rt.messages)
	var received []string
	for msg := range rt.messages {
		received = append(received, msg)
	}
	require.Len(t, received, 2)
	assert.Contains(t, received[0], `"eventId":"e1"`)
	assert.Contains(t, received[1], `"eventId":"e2"`)
	assert.Equal(t, []string{"", "c2"}, cursors)
	assert.EqualValues(t, 2, attempts.Load())
}

func TestEventStateMachineRecoverableDisconnectRetriesWithCursor(t *testing.T) {
	var attempts atomic.Int32
	var seenCursors []string
	var mu sync.Mutex

	var events *eventClient
	events = &eventClient{
		subscriptions:  make(map[string]*eventSubscription),
		idleTimeout:    time.Second,
		retryBaseDelay: 5 * time.Millisecond,
		retryMaxDelay:  10 * time.Millisecond,
		streamCall: func(_ context.Context, _ *gomcp.ClientSession, p *eventParams) error {
			attempt := attempts.Add(1)
			mu.Lock()
			if p.Cursor != nil {
				seenCursors = append(seenCursors, *p.Cursor)
			} else {
				seenCursors = append(seenCursors, "")
			}
			mu.Unlock()
			bindWireID(events, p, "w2")
			now := time.Now().UTC().Format(time.RFC3339Nano)
			switch attempt {
			case 1:
				events.receive(mustEventNotification(t, "notifications/events/active", "w2", map[string]any{"cursor": "c1"}))
				events.receive(mustEventNotification(t, "notifications/events/event", "w2", map[string]any{
					"eventId": "e1", "name": "demo.message", "timestamp": now, "cursor": "c2", "data": map[string]any{"seq": 1},
				}))
				return errors.New("request terminated without response")
			case 2:
				events.receive(mustEventNotification(t, "notifications/events/active", "w2", map[string]any{"cursor": "c2"}))
				events.receive(mustEventNotification(t, "notifications/events/event", "w2", map[string]any{
					"eventId": "e2", "name": "demo.message", "timestamp": now, "cursor": "c3", "data": map[string]any{"seq": 2},
				}))
				return nil
			default:
				t.Errorf("unexpected extra attempt %d", attempt)
				return nil
			}
		},
	}

	rt := &eventRuntime{messages: make(chan string, 4)}
	_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
	require.NoError(t, err)
	waitNoSubscriptions(t, events)

	close(rt.messages)
	var received []string
	for msg := range rt.messages {
		received = append(received, msg)
	}
	require.Len(t, received, 2)
	assert.Contains(t, received[0], `"eventId":"e1"`)
	assert.Contains(t, received[1], `"eventId":"e2"`)
	assert.Equal(t, []string{"", "c2"}, seenCursors)
}

func TestEventStateMachineTerminalOutcomesDoNotRetry(t *testing.T) {
	t.Run("terminated notification", func(t *testing.T) {
		var attempts atomic.Int32
		var events *eventClient
		events = &eventClient{
			subscriptions:  make(map[string]*eventSubscription),
			idleTimeout:    time.Second,
			retryBaseDelay: 5 * time.Millisecond,
			streamCall: func(ctx context.Context, _ *gomcp.ClientSession, p *eventParams) error {
				attempts.Add(1)
				bindWireID(events, p, "w3")
				events.receive(mustEventNotification(t, "notifications/events/active", "w3", map[string]any{"cursor": "c1"}))
				events.receive(mustEventNotification(t, "notifications/events/terminated", "w3", map[string]any{
					"error": map[string]any{"code": -32000, "message": "gone"},
				}))
				<-ctx.Done()
				return ctx.Err()
			},
		}
		rt := &eventRuntime{messages: make(chan string, 4)}
		_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
		require.NoError(t, err)
		waitNoSubscriptions(t, events)
		assert.EqualValues(t, 1, attempts.Load())
	})

	t.Run("protocol jsonrpc error after active", func(t *testing.T) {
		var attempts atomic.Int32
		var events *eventClient
		events = &eventClient{
			subscriptions:  make(map[string]*eventSubscription),
			idleTimeout:    time.Second,
			retryBaseDelay: 5 * time.Millisecond,
			streamCall: func(_ context.Context, _ *gomcp.ClientSession, p *eventParams) error {
				attempts.Add(1)
				bindWireID(events, p, "w4")
				events.receive(mustEventNotification(t, "notifications/events/active", "w4", map[string]any{"cursor": "c1"}))
				return &jsonrpc.Error{Code: -32602, Message: "invalid cursor"}
			},
		}
		rt := &eventRuntime{messages: make(chan string, 4)}
		_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
		require.NoError(t, err)
		waitNoSubscriptions(t, events)
		assert.EqualValues(t, 1, attempts.Load())
	})
}

func TestEventStateMachineRecallRejectionStopsWithoutAdvancingCursor(t *testing.T) {
	t.Run("event delivery failure", func(t *testing.T) {
		params := &eventParams{Name: "demo.message"}
		var attempts atomic.Int32
		var events *eventClient
		events = &eventClient{
			subscriptions:  make(map[string]*eventSubscription),
			idleTimeout:    time.Second,
			retryBaseDelay: 5 * time.Millisecond,
			streamCall: func(ctx context.Context, _ *gomcp.ClientSession, p *eventParams) error {
				attempts.Add(1)
				bindWireID(events, p, "w5")
				now := time.Now().UTC().Format(time.RFC3339Nano)
				events.receive(mustEventNotification(t, "notifications/events/active", "w5", map[string]any{"cursor": "c1"}))
				events.receive(mustEventNotification(t, "notifications/events/event", "w5", map[string]any{
					"eventId": "e1", "name": "demo.message", "timestamp": now, "cursor": "c2", "data": map[string]any{"seq": 1},
				}))
				<-ctx.Done()
				return ctx.Err()
			},
		}
		rt := &eventRuntime{
			messages: make(chan string, 4),
			recallErr: func(string) error {
				return errors.New("steering queue full")
			},
		}
		_, err := events.subscribe(t.Context(), nil, params, "demo", rt)
		require.NoError(t, err)
		waitNoSubscriptions(t, events)
		require.NotNil(t, params.Cursor)
		assert.Equal(t, "c1", *params.Cursor)
		assert.EqualValues(t, 1, attempts.Load())
	})

	t.Run("gap notification delivery failure", func(t *testing.T) {
		initial := "c1"
		params := &eventParams{Name: "demo.message", Cursor: &initial}
		var attempts atomic.Int32
		var events *eventClient
		events = &eventClient{
			subscriptions:  make(map[string]*eventSubscription),
			idleTimeout:    time.Second,
			retryBaseDelay: 5 * time.Millisecond,
			streamCall: func(ctx context.Context, _ *gomcp.ClientSession, p *eventParams) error {
				attempts.Add(1)
				bindWireID(events, p, "w6")
				events.receive(mustEventNotification(t, "notifications/events/active", "w6", map[string]any{
					"cursor": "c9", "truncated": true,
				}))
				<-ctx.Done()
				return ctx.Err()
			},
		}
		rt := &eventRuntime{
			messages: make(chan string, 4),
			recallErr: func(string) error {
				return errors.New("steering queue full")
			},
		}
		_, err := events.subscribe(t.Context(), nil, params, "demo", rt)
		require.ErrorContains(t, err, "steering queue full")
		waitNoSubscriptions(t, events)
		require.NotNil(t, params.Cursor)
		assert.Equal(t, "c1", *params.Cursor)
		assert.EqualValues(t, 1, attempts.Load())
	})
}

func TestEventStateMachineReadyVsLifetimeRace(t *testing.T) {
	t.Run("immediate completion preserves confirmation", func(t *testing.T) {
		for range 50 {
			var events *eventClient
			events = &eventClient{
				subscriptions:  make(map[string]*eventSubscription),
				idleTimeout:    time.Second,
				retryBaseDelay: 5 * time.Millisecond,
				streamCall: func(_ context.Context, _ *gomcp.ClientSession, p *eventParams) error {
					bindWireID(events, p, "w7")
					events.receive(mustEventNotification(t, "notifications/events/active", "w7", map[string]any{"cursor": nil}))
					return nil
				},
			}
			rt := &eventRuntime{messages: make(chan string, 1)}
			id, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
			require.NoError(t, err)
			assert.NotEmpty(t, id)
			waitNoSubscriptions(t, events)
		}
	})

	t.Run("terminated error sanitized before confirmation", func(t *testing.T) {
		var events *eventClient
		events = &eventClient{
			subscriptions:  make(map[string]*eventSubscription),
			idleTimeout:    time.Second,
			retryBaseDelay: 5 * time.Millisecond,
			sanitizeError:  sessionRemoteError,
			streamCall: func(ctx context.Context, _ *gomcp.ClientSession, p *eventParams) error {
				bindWireID(events, p, "w-term-san")
				events.receive(mustEventNotification(t, "notifications/events/terminated", "w-term-san", map[string]any{
					"error": map[string]any{"code": -32000, "message": "https://mcp.example?token=leaked"},
				}))
				<-ctx.Done()
				return ctx.Err()
			},
		}
		rt := &eventRuntime{messages: make(chan string, 1)}
		_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "leaked")
		assert.Equal(t, "client remote MCP request failed", err.Error())
		waitNoSubscriptions(t, events)
	})

	t.Run("stale terminated notification from previous attempt ignored", func(t *testing.T) {
		var attempts atomic.Int32
		var events *eventClient
		events = &eventClient{
			subscriptions:  make(map[string]*eventSubscription),
			idleTimeout:    35 * time.Millisecond,
			retryBaseDelay: 5 * time.Millisecond,
			streamCall: func(ctx context.Context, _ *gomcp.ClientSession, p *eventParams) error {
				attempt := attempts.Add(1)
				now := time.Now().UTC().Format(time.RFC3339Nano)
				switch attempt {
				case 1:
					bindWireID(events, p, "w-old")
					events.receive(mustEventNotification(t, "notifications/events/active", "w-old", map[string]any{"cursor": "c1"}))
					<-ctx.Done()
					// Queue a late terminated notification on the old wireID after timeout cancellation.
					events.receive(mustEventNotification(t, "notifications/events/terminated", "w-old", map[string]any{
						"error": map[string]any{"code": -32000, "message": "stale"},
					}))
					return ctx.Err()
				case 2:
					bindWireID(events, p, "w-new")
					events.receive(mustEventNotification(t, "notifications/events/active", "w-new", map[string]any{"cursor": "c1"}))
					// Also send another late notification for w-old while w-new is active.
					events.receive(mustEventNotification(t, "notifications/events/terminated", "w-old", map[string]any{
						"error": map[string]any{"code": -32000, "message": "stale"},
					}))
					events.receive(mustEventNotification(t, "notifications/events/event", "w-new", map[string]any{
						"eventId": "e-new", "name": "demo.message", "timestamp": now, "cursor": "c2", "data": map[string]any{"ok": true},
					}))
					return nil
				default:
					return nil
				}
			},
		}
		rt := &eventRuntime{messages: make(chan string, 4)}
		_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
		require.NoError(t, err)
		waitNoSubscriptions(t, events)
		select {
		case msg := <-rt.messages:
			assert.Contains(t, msg, `"eventId":"e-new"`)
		default:
			t.Fatal("expected event from replacement stream")
		}
	})

	t.Run("immediate sanitized error preserved", func(t *testing.T) {
		for range 50 {
			events := &eventClient{
				subscriptions:  make(map[string]*eventSubscription),
				idleTimeout:    time.Second,
				retryBaseDelay: 5 * time.Millisecond,
				sanitizeError:  sessionRemoteError,
				streamCall: func(_ context.Context, _ *gomcp.ClientSession, _ *eventParams) error {
					return errors.New("Post https://mcp.example/events?token=super-secret: dial failed")
				},
			}
			rt := &eventRuntime{messages: make(chan string, 1)}
			_, err := events.subscribe(t.Context(), nil, &eventParams{Name: "demo.message"}, "demo", rt)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "super-secret")
			assert.Equal(t, "client remote MCP request failed", err.Error())
			waitNoSubscriptions(t, events)
		}
	})
}

var _ io.Closer = (*eventConnection)(nil)
