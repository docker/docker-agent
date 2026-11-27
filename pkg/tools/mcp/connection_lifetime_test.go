package mcp

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lifetimeClient struct {
	mockMCPClient

	mu                           sync.Mutex
	toolsHandler, promptsHandler func()
	toolsStarted, promptsStarted chan context.Context
	closed                       chan struct{}
}

func (c *lifetimeClient) SetToolListChangedHandler(h func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolsHandler = h
}

func (c *lifetimeClient) SetPromptListChangedHandler(h func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.promptsHandler = h
}

func (c *lifetimeClient) ListTools(ctx context.Context, _ *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error] {
	return func(yield func(*gomcp.Tool, error) bool) { c.toolsStarted <- ctx; <-ctx.Done(); yield(nil, ctx.Err()) }
}

func (c *lifetimeClient) ListPrompts(ctx context.Context, _ *gomcp.ListPromptsParams) iter.Seq2[*gomcp.Prompt, error] {
	return func(yield func(*gomcp.Prompt, error) bool) {
		c.promptsStarted <- ctx
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}
func (c *lifetimeClient) Close(context.Context) error { c.closed <- struct{}{}; return nil }

func TestConnectionCloseCancelsRefreshesAndNotNewerConnection(t *testing.T) {
	t.Parallel()
	c := &lifetimeClient{toolsStarted: make(chan context.Context, 2), promptsStarted: make(chan context.Context, 2), closed: make(chan struct{}, 2)}
	ts := newTestToolset("test", "test", c)
	ts.markStartedForTesting()
	connector := &clientConnector{ts: ts}
	first, err := connector.Connect(t.Context())
	require.NoError(t, err)
	c.mu.Lock()
	oldTools, oldPrompts := c.toolsHandler, c.promptsHandler
	c.mu.Unlock()
	go oldTools()
	go oldPrompts()
	var toolCtx, promptCtx context.Context
	select {
	case toolCtx = <-c.toolsStarted:
	case <-time.After(time.Second):
		t.Fatal("tools refresh not started")
	}
	select {
	case promptCtx = <-c.promptsStarted:
	case <-time.After(time.Second):
		t.Fatal("prompt refresh not started")
	}
	second, err := connector.Connect(t.Context())
	require.NoError(t, err)
	ts.mu.Lock()
	newerCtx := ts.refreshCtx
	ts.mu.Unlock()
	require.NoError(t, first.Close(t.Context()))
	require.ErrorIs(t, toolCtx.Err(), context.Canceled)
	require.ErrorIs(t, promptCtx.Err(), context.Canceled)
	require.NoError(t, newerCtx.Err())
	ts.mu.Lock()
	generation := ts.cacheGen
	ts.mu.Unlock()
	oldTools()
	oldPrompts()
	ts.mu.Lock()
	assert.Equal(t, generation, ts.cacheGen)
	ts.mu.Unlock()
	require.NoError(t, second.Close(t.Context()))
	require.ErrorIs(t, newerCtx.Err(), context.Canceled)
}
