package mcp

import (
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"os/exec"
	"sync"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// failingInitClient is a mock mcpClient whose Initialize method returns a
// configurable error for the first N calls, then succeeds.
type failingInitClient struct {
	mu          sync.Mutex
	initErr     error // error to return from Initialize
	failsLeft   int   // how many more times Initialize should fail
	initCalls   int   // total Initialize calls
	waitCh      chan struct{}
	toolsToList []*gomcp.Tool
}

func (m *failingInitClient) Initialize(_ context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initCalls++
	if m.failsLeft > 0 {
		m.failsLeft--
		return nil, m.initErr
	}
	if m.waitCh != nil {
		m.waitCh = make(chan struct{})
	}
	return &gomcp.InitializeResult{}, nil
}

func (m *failingInitClient) ListTools(_ context.Context, _ *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error] {
	m.mu.Lock()
	t := m.toolsToList
	m.mu.Unlock()
	return func(yield func(*gomcp.Tool, error) bool) {
		for _, tool := range t {
			if !yield(tool, nil) {
				return
			}
		}
	}
}

func (m *failingInitClient) CallTool(context.Context, *gomcp.CallToolParams) (*gomcp.CallToolResult, error) {
	return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "ok"}}}, nil
}

func (m *failingInitClient) ListPrompts(context.Context, *gomcp.ListPromptsParams) iter.Seq2[*gomcp.Prompt, error] {
	return func(func(*gomcp.Prompt, error) bool) {}
}

func (m *failingInitClient) GetPrompt(context.Context, *gomcp.GetPromptParams) (*gomcp.GetPromptResult, error) {
	return &gomcp.GetPromptResult{}, nil
}

func (m *failingInitClient) SetElicitationHandler(tools.ElicitationHandler) {}
func (m *failingInitClient) SetOAuthSuccessHandler(func())                  {}
func (m *failingInitClient) SetManagedOAuth(bool)                           {}
func (m *failingInitClient) SetToolListChangedHandler(func())               {}
func (m *failingInitClient) SetPromptListChangedHandler(func())             {}

func (m *failingInitClient) Wait() error {
	m.mu.Lock()
	ch := m.waitCh
	m.mu.Unlock()
	if ch == nil {
		select {}
	}
	<-ch
	return nil
}

func (m *failingInitClient) Close(context.Context) error {
	m.mu.Lock()
	if m.waitCh != nil {
		select {
		case <-m.waitCh:
		default:
			close(m.waitCh)
		}
	}
	m.mu.Unlock()
	return nil
}

// TestStdioStartReturnsErrorWhenServerUnavailable verifies that a stdio toolset
// propagates errServerUnavailable when Initialize returns io.EOF, and that
// started remains false so the runtime can retry.
func TestStdioStartReturnsErrorWhenServerUnavailable(t *testing.T) {
	t.Parallel()

	mock := &failingInitClient{
		initErr:   io.EOF,
		failsLeft: 1,
	}

	ts := newTestToolset("test-stdio", "test-cmd", mock)

	err := ts.Start(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errServerUnavailable)

	assert.False(t, ts.IsStarted(), "stdio toolset must not be marked as started when server is unavailable")
}

// TestStdioStartReturnsErrorWhenBinaryNotFound verifies that exec.ErrNotFound
// from Initialize is treated the same as io.EOF for stdio toolsets.
func TestStdioStartReturnsErrorWhenBinaryNotFound(t *testing.T) {
	t.Parallel()

	mock := &failingInitClient{
		initErr:   fmt.Errorf("start command: %w", exec.ErrNotFound),
		failsLeft: 1,
	}

	ts := newTestToolset("test-stdio", "missing-binary", mock)

	err := ts.Start(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errServerUnavailable)

	assert.False(t, ts.IsStarted(), "stdio toolset must not be marked as started when binary is not found")
}

// TestStdioLazyRetrySucceedsWhenBinaryAppears verifies the end-to-end retry
// scenario: turn 1 fails with EOF (binary not yet available), turn 2 succeeds
// once the binary "appears" (mock stops failing).
func TestStdioLazyRetrySucceedsWhenBinaryAppears(t *testing.T) {
	t.Parallel()

	pingTool := &gomcp.Tool{Name: "ping"}
	mock := &failingInitClient{
		initErr:     io.EOF,
		failsLeft:   1,
		toolsToList: []*gomcp.Tool{pingTool},
		waitCh:      make(chan struct{}),
	}

	ts := newTestToolset("test-stdio", "lazy-binary", mock)

	// Turn 1: Start fails — binary not available yet.
	err := ts.Start(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errServerUnavailable)

	// Turn 2: Binary has "appeared" (mock will succeed).
	err = ts.Start(t.Context())
	require.NoError(t, err)

	assert.True(t, ts.IsStarted(), "stdio toolset must be started after successful retry")

	toolList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 1)
	assert.Equal(t, "test-stdio_ping", toolList[0].Name)

	_ = ts.Stop(t.Context())
}

// TestRemoteStartRetriesWhenUnavailable verifies that a remote toolset also
// returns an error and stays un-started when the server is unavailable (EOF),
// confirming retry-on-next-turn applies to all toolset types.
func TestRemoteStartRetriesWhenUnavailable(t *testing.T) {
	t.Parallel()

	mock := &failingInitClient{
		initErr:   io.EOF,
		failsLeft: 1,
	}

	ts := newTestToolset("test-remote", "remote-server", mock)

	err := ts.Start(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errServerUnavailable)

	assert.False(t, ts.IsStarted(), "remote toolset must not be marked as started when server is unavailable")
}

// TestStartableToolSetRetryAcrossTurns is a full integration test using
// tools.NewStartable to wrap an MCP Toolset. It verifies that when a stdio
// toolset fails N turns, the StartableToolSet keeps retrying and succeeds
// on turn N+1.
func TestStartableToolSetRetryAcrossTurns(t *testing.T) {
	t.Parallel()

	const failTurns = 3

	pingTool := &gomcp.Tool{Name: "ping"}
	mock := &failingInitClient{
		initErr:     fmt.Errorf("command not found: %w", os.ErrNotExist),
		failsLeft:   failTurns,
		toolsToList: []*gomcp.Tool{pingTool},
		waitCh:      make(chan struct{}),
	}

	mcpToolset := newTestToolset("retry-test", "retry-binary", mock)

	startable := tools.NewStartable(mcpToolset)

	// Turns 1..N: Start fails, IsStarted stays false.
	for turn := 1; turn <= failTurns; turn++ {
		err := startable.Start(t.Context())
		require.Error(t, err, "turn %d should fail", turn)
		assert.False(t, startable.IsStarted(), "turn %d: should not be started", turn)
	}

	// Turn N+1: binary is now available, Start succeeds.
	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.True(t, startable.IsStarted())

	toolList, err := mcpToolset.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolList, 1)
	assert.Equal(t, "retry-test_ping", toolList[0].Name)

	_ = startable.Stop(t.Context())
}
