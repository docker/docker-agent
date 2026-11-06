package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestSessionStdioHelper(t *testing.T) {
	if os.Getenv("MCP_SESSION_HELPER") != "1" {
		return
	}
	marker := os.Getenv("MCP_MARKER")
	_ = os.WriteFile(marker+".started", []byte("started"), 0o600)
	if os.Getenv("MCP_MODE") == "hang-init" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	server := gomcp.NewServer(&gomcp.Implementation{Name: "test", Version: "1"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "ping"}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, any, error) {
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "pong"}}}, nil, nil
	})
	gomcp.AddTool(server, &gomcp.Tool{Name: "exit"}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, any, error) {
		os.Exit(0)
		return nil, nil, nil
	})
	if os.Getenv("MCP_MODE") == "block-pipe" {
		decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := decoder.Decode(&req); err != nil {
				os.Exit(2)
			}
			if len(req.ID) == 0 {
				continue
			}
			response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			switch req.Method {
			case "server/discover":
				response["error"] = map[string]any{"code": -32601, "message": "not found"}
			case "initialize":
				response["result"] = map[string]any{"protocolVersion": "2025-11-25", "serverInfo": map[string]any{"name": "blocked", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
			case "tools/list":
				response["result"] = map[string]any{"tools": []any{map[string]any{"name": "ping", "inputSchema": map[string]any{"type": "object"}}}}
			default:
				response["result"] = map[string]any{}
			}
			_ = encoder.Encode(response)
			if req.Method == "tools/list" {
				_ = os.WriteFile(marker+".blocked", []byte("blocked"), 0o600)
				// Keep the helper alive without reading stdin until the parent kills it.
				<-time.After(time.Hour)
			}
		}
	}
	if os.Getenv("MCP_MODE") == "discovery" {
		name, err := os.ReadFile(marker + ".capabilities")
		if err != nil {
			os.Exit(2)
		}
		server.RemoveTools("ping", "exit")
		gomcp.AddTool(server, &gomcp.Tool{Name: string(name)}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{}, nil, nil
		})
		server.AddPrompt(&gomcp.Prompt{Name: string(name)}, func(context.Context, *gomcp.GetPromptRequest) (*gomcp.GetPromptResult, error) {
			return &gomcp.GetPromptResult{}, nil
		})
	}
	_ = server.Run(t.Context(), &gomcp.StdioTransport{})
	_ = os.WriteFile(marker+".stopped", []byte("stopped"), 0o600)
	os.Exit(0)
}

func sessionTestToolset(t *testing.T, mode string) (*Toolset, string) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	wd := t.TempDir()
	marker := filepath.Join(wd, "server")
	env := append(os.Environ(), "MCP_SESSION_HELPER=1", "MCP_MODE="+mode, "MCP_MARKER="+marker)
	ts := NewSessionToolsetCommand("session", executable, []string{"-test.run=^TestSessionStdioHelper$"}, env, wd)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		_ = ts.Stop(ctx)
	})
	return ts, marker
}

func TestSessionStdioStartAfterStop(t *testing.T) {
	t.Parallel()
	ts, _ := sessionTestToolset(t, "normal")
	for range 3 {
		require.NoError(t, ts.Start(t.Context()))
		available, err := ts.Tools(t.Context())
		require.NoError(t, err)
		found := false
		for _, tool := range available {
			if tool.Name != "session_ping" {
				continue
			}
			result, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, "pong", result.Output)
			found = true
		}
		require.True(t, found)
		require.NoError(t, ts.Stop(t.Context()))
		require.False(t, ts.IsStarted())
	}
}

func TestSessionStdioSetupCancellation(t *testing.T) {
	t.Parallel()
	ts, marker := sessionTestToolset(t, "hang-init")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ts.Start(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(marker + ".started"); return err == nil }, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup cancellation did not reap the child")
	}
	require.NoError(t, ts.Stop(t.Context()))
}

func TestSessionStdioSurvivesSetupAndDoesNotReconnect(t *testing.T) {
	t.Parallel()
	ts, marker := sessionTestToolset(t, "normal")
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, ts.Start(ctx))
	cancel()
	available, err := ts.Tools(t.Context())
	require.NoError(t, err)
	for _, name := range []string{"session_ping", "session_exit"} {
		for _, tool := range available {
			if tool.Name != name {
				continue
			}
			result, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: "{}"}}, tools.NopRuntime{})
			if name == "session_ping" {
				require.NoError(t, err)
				assert.Equal(t, "pong", result.Output)
			} else {
				require.Error(t, err)
			}
		}
	}
	require.Eventually(t, func() bool { return !ts.IsStarted() }, 5*time.Second, time.Millisecond)
	_, err = ts.Tools(t.Context())
	require.Error(t, err)
	assert.FileExists(t, marker+".started")
	require.NoError(t, ts.Stop(t.Context()))
}

func TestSessionStdioStopUnblocksPipeWrite(t *testing.T) {
	t.Parallel()
	ts, marker := sessionTestToolset(t, "block-pipe")
	require.NoError(t, ts.Start(t.Context()))
	available, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, available, 1)
	tool := available[0]
	require.Eventually(t, func() bool { _, err := os.Stat(marker + ".blocked"); return err == nil }, 5*time.Second, time.Millisecond)
	args, err := json.Marshal(map[string]any{"payload": strings.Repeat("x", 2*1024*1024)})
	require.NoError(t, err)
	called := make(chan error, 1)
	go func() {
		_, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: tool.Name, Arguments: string(args)}}, tools.NopRuntime{})
		called <- err
	}()
	stopped := make(chan error, 1)
	go func() { stopped <- ts.Stop(t.Context()) }()
	require.Eventually(t, func() bool { return ts.State().State == lifecycle.StateStopped }, 5*time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, ts.Stop(ctx), context.DeadlineExceeded)
	select {
	case err := <-stopped:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not kill the blocked subprocess")
	}
	select {
	case err := <-called:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("tool call remained blocked after stop")
	}
}

func TestSessionStdioResumeRefreshesDiscovery(t *testing.T) {
	t.Parallel()
	ts, marker := sessionTestToolset(t, "discovery")
	for _, name := range []string{"before", "after"} {
		require.NoError(t, os.WriteFile(marker+".capabilities", []byte(name), 0o600))
		require.NoError(t, ts.Start(t.Context()))
		available, err := ts.Tools(t.Context())
		require.NoError(t, err)
		require.Len(t, available, 1)
		assert.Equal(t, "session_"+name, available[0].Name)
		prompts, err := ts.ListPrompts(t.Context())
		require.NoError(t, err)
		require.Len(t, prompts, 1)
		assert.Equal(t, name, prompts[0].Name)
		require.NoError(t, ts.Stop(t.Context()))
	}
}

func TestSessionStdioResumeWaitsForCleanup(t *testing.T) {
	t.Parallel()
	ts, _ := sessionTestToolset(t, "normal")
	require.NoError(t, ts.Start(t.Context()))
	cleaning, release := make(chan struct{}), make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	client := ts.mcpClient.(*stdioMCPClient)
	client.processMu.Lock()
	cancelProcess := client.processCancel
	var cleanups atomic.Int32
	client.processCancel = func() {
		if cleanups.Add(1) == 1 {
			close(cleaning)
			<-release
		}
		cancelProcess()
	}
	client.processMu.Unlock()
	stopped := make(chan error, 1)
	go func() { stopped <- ts.Stop(t.Context()) }()
	<-cleaning
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, ts.Start(ctx), context.DeadlineExceeded)
	close(release)
	require.NoError(t, <-stopped)
	release = nil
	require.NoError(t, ts.Start(t.Context()))
	available, err := ts.Tools(t.Context())
	require.NoError(t, err)
	for _, tool := range available {
		if tool.Name == "session_ping" {
			result, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: tool.Name, Arguments: "{}"}}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, "pong", result.Output)
			return
		}
	}
	t.Fatal("ping tool missing after resume")
}
