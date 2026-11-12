package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/tools"
)

func remoteClientSpec(transport, name, endpoint, value string) acpsdk.McpServer {
	headers := []acpsdk.HttpHeader{{Name: "Authorization", Value: value}}
	if transport == "sse" {
		return acpsdk.McpServer{Sse: &acpsdk.McpServerSseInline{Type: "sse", Name: name, Url: endpoint, Headers: headers}}
	}
	return acpsdk.McpServer{Http: &acpsdk.McpServerHttpInline{Type: "http", Name: name, Url: endpoint, Headers: headers}}
}

// A child process snapshots the test proxy as operator-configured egress before
// package initialization; production SSRF policy remains enabled throughout.
func TestClientRemoteMCPIntegration(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			get := func(r *http.Request) *mcp.Server {
				value := r.Header.Get("Authorization")
				server := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
				mcp.AddTool(server, &mcp.Tool{Name: "remote_inspect", Annotations: &mcp.ToolAnnotations{Title: "remote_inspect", ReadOnlyHint: true}}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					data, _ := json.Marshal(map[string]string{"value": value})
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
				})
				return server
			}
			var handler http.Handler = mcp.NewStreamableHTTPHandler(get, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
			if transport == "sse" {
				handler = mcp.NewSSEHandler(get, &mcp.SSEOptions{DisableLocalhostProtection: true})
			}
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fail" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				assert.Equal(t, "acp-mcp.example", r.Host)
				handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestClientRemoteMCPHelper$")
			cmd.Env = append(os.Environ(), "ACP_REMOTE_HELPER="+transport, "HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL, "HTTPS_PROXY=", "https_proxy=", "ALL_PROXY=", "all_proxy=", "NO_PROXY=", "no_proxy=", "DOCKER_AGENT_DISABLE_DESKTOP_PROXY=1")
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

func TestClientRemoteMCPHelper(t *testing.T) {
	transport := os.Getenv("ACP_REMOTE_HELPER")
	if transport == "" {
		return
	}
	a := clientMCPAgent(t)
	fixture := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
	a.SetAgentConnection(fixture.agent.conn)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd, root := t.TempDir(), t.TempDir()
	spec := remoteClientSpec(transport, "remote", "http://acp-mcp.example/mcp", "${headers.Authorization} ${env.SECRET}")
	ctx, cancel := context.WithCancel(t.Context())
	created, err := a.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: wd, AdditionalDirectories: []string{root}, McpServers: []acpsdk.McpServer{clientServerSpec(t, "stdio", "local"), spec}})
	require.NoError(t, err)
	cancel()
	s := a.sessions[string(created.SessionId)]
	old := clientTool(t, s, "remote_inspect")
	assert.Equal(t, "${headers.Authorization} ${env.SECRET}", inspectClientTool(t, old)["value"])
	assert.Equal(t, "local", inspectClientTool(t, clientTool(t, s, "inspect"))["value"])
	second, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{remoteClientSpec(transport, "remote", "http://acp-mcp.example/mcp", "second")}})
	require.NoError(t, err)
	for _, method := range []string{"resume", "load"} {
		specs := []acpsdk.McpServer{remoteClientSpec(transport, "partial", "http://acp-mcp.example/mcp", "partial"), remoteClientSpec(transport, "bad", "http://acp-mcp.example/fail", "bad")}
		if method == "resume" {
			_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: specs})
		} else {
			_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: specs})
		}
		require.Error(t, err)
		assert.Equal(t, "${headers.Authorization} ${env.SECRET}", inspectClientTool(t, old)["value"])
		_, roots := s.workspaceSnapshot()
		assert.Equal(t, []string{root}, roots)
	}
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{remoteClientSpec(transport, "remote", "http://acp-mcp.example/mcp", "replacement")}})
	require.NoError(t, err)
	current := clientTool(t, s, "remote_inspect")
	assert.NotEqual(t, old.Name, current.Name)
	checker := permissions.NewCheckerFromRules([]string{old.Name}, nil, nil)
	assert.NotEqual(t, permissions.Allow, checker.CheckWithArgs(current.Name, nil))
	result, err := old.Handler(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, "replacement", inspectClientTool(t, current)["value"])
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	available, err := s.rt.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	for _, tool := range available {
		assert.False(t, strings.HasPrefix(tool.Name, "acp_"))
	}
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	assert.Equal(t, "second", inspectClientTool(t, clientTool(t, a.sessions[string(second.SessionId)], "remote_inspect"))["value"])
	_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{remoteClientSpec(transport, "cold", "http://acp-mcp.example/mcp", "cold")}})
	require.NoError(t, err)
	assert.Equal(t, "cold", inspectClientTool(t, clientTool(t, a.sessions[string(created.SessionId)], "remote_inspect"))["value"])
}

func TestClientRemoteMCPValidation(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"http", "sse"} {
		for _, endpoint := range []string{"relative", "file:///tmp/mcp", "unix:///tmp/mcp", "https:///mcp", "https://user:secret@example.org/mcp", "https://example.org/#", "https://example.org/#fragment", "https://example.org:bad/", "https://example.org/\x00"} {
			_, err := validateClientMCPServers([]acpsdk.McpServer{remoteClientSpec(transport, "remote", endpoint, "value")})
			require.Error(t, err, endpoint)
		}
		for _, header := range []acpsdk.HttpHeader{{Name: "bad name"}, {Name: "X-Test", Value: "value\r\nInjected: yes"}, {Name: "X-Test", Value: "\x00"}, {Name: ""}, {Name: "Host"}, {Name: "mcp-session-id"}, {Name: "MCP-Protocol-Version"}, {Name: "Last-Event-ID"}, {Name: "Content-Length"}, {Name: "Accept"}, {Name: "Proxy-Authorization"}, {Name: "Connection"}, {Name: "Idempotency-Key"}, {Name: "X-Idempotency-Key"}, {Name: "authorization", Value: "duplicate"}} {
			spec := remoteClientSpec(transport, "remote", "https://example.org/mcp", "secret")
			if spec.Http != nil {
				spec.Http.Headers = append(spec.Http.Headers, header)
			} else {
				spec.Sse.Headers = append(spec.Sse.Headers, header)
			}
			_, err := validateClientMCPServers([]acpsdk.McpServer{spec})
			require.Error(t, err, header.Name)
			assert.NotContains(t, err.Error(), "secret")
		}
	}
	mixed := []acpsdk.McpServer{clientServerSpec(t, "stdio", "value"), remoteClientSpec("http", "http", "https://example.org/mcp?key=secret", "literal"), remoteClientSpec("sse", "sse", "https://example.org/events", "literal")}
	validated, err := validateClientMCPServers(mixed)
	require.NoError(t, err)
	require.Len(t, validated, 3)
	mixed[1].Http.Headers[0].Value = "changed"
	assert.Equal(t, "literal", validated[1].headers["Authorization"])
	mixed[2].Sse.Name = "stdio"
	_, err = validateClientMCPServers(mixed)
	require.Error(t, err)
	mixed[0].Http = mixed[1].Http
	_, err = validateClientMCPServers(mixed[:1])
	require.Error(t, err)
}
