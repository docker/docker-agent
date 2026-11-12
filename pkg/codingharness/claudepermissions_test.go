package codingharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/tools"
)

// fakeClaudeDir, when set, makes the test binary act as the claude CLI and
// write what it saw into that directory.
const fakeClaudeDir = "DOCKER_AGENT_TEST_FAKE_CLAUDE_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeClaudeDir); dir != "" {
		os.Exit(runFakeClaude(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestWithClaudeFlags(t *testing.T) {
	t.Parallel()
	flags := []string{"--mcp-config", "/tmp/mcp.json", "--permission-prompt-tool", "mcp__x__y"}
	assert.Equal(t,
		[]string{"claude", "--mcp-config", "/tmp/mcp.json", "--permission-prompt-tool", "mcp__x__y", "--print", "-p", "prompt"},
		withClaudeFlags([]string{"claude", "--print", "-p", "prompt"}, flags))
	assert.Empty(t, withClaudeFlags(nil, flags))
}

func TestClaudePermissionServer(t *testing.T) {
	t.Parallel()
	elicit, requests := recordingElicitation(tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"q1": "Blue"}}, nil)
	server, err := startClaudePermissionServer(t.Context(), elicit)
	require.NoError(t, err)
	t.Cleanup(server.Close)

	require.Len(t, server.flags, 4)
	assert.Equal(t, "--mcp-config", server.flags[0])
	assert.Equal(t, []string{"--permission-prompt-tool", "mcp__docker-agent-harness__permission_prompt"}, server.flags[2:])
	configPath := server.flags[1]
	if runtime.GOOS != "windows" { // file modes are POSIX-only
		assertMode(t, configPath, 0o600)
		assertMode(t, filepath.Dir(configPath), 0o700)
	}

	endpoint, headers := readClaudeMCPConfig(t, configPath)
	assert.Equal(t, http.StatusUnauthorized, postWithoutToken(t, endpoint))

	session := connectClaudeMCP(t, endpoint, headers)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	assert.Equal(t, claudePermissionToolName, listed.Tools[0].Name)

	decision := callPermissionPrompt(t, session, claudeAskUserQuestionTool, map[string]any{"questions": []any{
		map[string]any{"question": "Which color do you prefer?", "options": []any{map[string]any{"label": "Red"}, map[string]any{"label": "Blue"}}},
	}})
	assert.Equal(t, "allow", decision.Behavior)
	assert.Equal(t, map[string]any{"Which color do you prefer?": "Blue"}, decision.UpdatedInput["answers"])
	assert.Len(t, *requests, 1)

	assert.Equal(t, "allow", callPermissionPrompt(t, session, "Bash", map[string]any{"command": "ls"}).Behavior)
	assert.Len(t, *requests, 1, "only AskUserQuestion and ExitPlanMode ask the user")

	server.Close()
	_, err = os.Stat(filepath.Dir(configPath))
	assert.ErrorIs(t, err, os.ErrNotExist, "Close must remove the config and its token")
}

func TestClaudeCodeTurnAsksThroughThePermissionTool(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run("resume="+strconv.FormatBool(resume), func(t *testing.T) {
			dir := useFakeClaude(t)
			elicit, requests := recordingElicitation(tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"q1": "Blue"}}, nil)
			p := newElicitingClaudeCode(t, elicit)

			var result harness.Event
			handle := func(ev harness.Event) {
				if ev.Type == harness.EventToolResult {
					result = ev
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if resume {
				require.NoError(t, p.Resume(ctx, "claude-session", "pick a color", handle))
			} else {
				require.NoError(t, p.Run(ctx, "pick a color", handle))
			}

			require.Empty(t, fakeClaudeReport(t, dir, "error"))
			args := strings.Split(fakeClaudeReport(t, dir, "args"), "\n")
			assert.Equal(t, []string{"--mcp-config"}, args[:1])
			assert.Equal(t, []string{"--permission-prompt-tool", "mcp__docker-agent-harness__permission_prompt", "--print"}, args[2:5])
			assert.Equal(t, []string{"-p", "pick a color"}, args[len(args)-2:])
			assert.Equal(t, resume, slices.Contains(args, "--resume"))
			assert.Equal(t, "600 700", fakeClaudeReport(t, dir, "modes"))
			assert.Equal(t, "401", fakeClaudeReport(t, dir, "unauthorized"))
			_, err := os.Stat(args[1])
			require.ErrorIs(t, err, os.ErrNotExist, "the config must not outlive the turn")

			require.Len(t, *requests, 1)
			assert.Equal(t, claudeAskUserQuestionTool, result.ToolName)
			var decision claudePermissionDecision
			require.NoError(t, json.Unmarshal([]byte(result.ToolOutput), &decision))
			assert.Equal(t, map[string]any{"Which color do you prefer?": "Blue"}, decision.UpdatedInput["answers"])
		})
	}
}

func TestClaudeCodeTurnWithoutElicitationKeepsItsArguments(t *testing.T) {
	dir := useFakeClaude(t)
	p, err := Factory(&latest.HarnessConfig{Type: TypeClaudeCode})
	require.NoError(t, err)

	require.NoError(t, p.Run(t.Context(), "pick a color", func(harness.Event) {}))

	args := fakeClaudeReport(t, dir, "args")
	assert.NotContains(t, args, "--mcp-config")
	assert.NotContains(t, args, "--permission-prompt-tool")
}

func TestClaudeCodeQuestionEndsWithTheTurn(t *testing.T) {
	dir := useFakeClaude(t)
	type turnKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), turnKey{}, "turn"))
	defer cancel()
	asked := make(chan string, 1)
	answered := make(chan error, 1)
	p := newElicitingClaudeCode(t, func(ctx context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
		asked <- ctx.Value(turnKey{}).(string)
		<-ctx.Done()
		answered <- ctx.Err()
		return tools.ElicitationResult{}, ctx.Err()
	})

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, "pick a color", func(harness.Event) {}) }()
	select {
	case value := <-asked:
		assert.Equal(t, "turn", value, "the question must see the turn's context values")
	case <-time.After(30 * time.Second):
		t.Fatalf("the fake CLI never asked: %s", fakeClaudeReport(t, dir, "error"))
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("the turn did not stop with its context")
	}
	select {
	case err := <-answered:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("the question stayed open after the turn")
	}
}

// A CLI that exits with a question still open must not leave it waiting.
func TestClaudePermissionServerCloseEndsOpenQuestions(t *testing.T) {
	t.Parallel()
	asked := make(chan struct{})
	answered := make(chan error, 1)
	server, err := startClaudePermissionServer(t.Context(), func(ctx context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
		close(asked)
		<-ctx.Done()
		answered <- ctx.Err()
		return tools.ElicitationResult{}, ctx.Err()
	})
	require.NoError(t, err)
	endpoint, headers := readClaudeMCPConfig(t, server.flags[1])
	session := connectClaudeMCP(t, endpoint, headers)

	go func() {
		_, _ = session.CallTool(context.WithoutCancel(t.Context()), &mcp.CallToolParams{
			Name:      claudePermissionToolName,
			Arguments: map[string]any{"tool_name": claudeExitPlanModeTool, "input": map[string]any{"plan": "x"}},
		})
	}()
	select {
	case <-asked:
	case <-time.After(30 * time.Second):
		t.Fatal("the question never reached the handler")
	}
	server.Close()

	select {
	case err := <-answered:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("Close left the question open")
	}
}

func newElicitingClaudeCode(t *testing.T, elicit tools.ElicitationHandler) harness.Provider {
	t.Helper()
	p, err := Factory(&latest.HarnessConfig{Type: TypeClaudeCode})
	require.NoError(t, err)
	eliciter, ok := p.(harness.Eliciter)
	require.True(t, ok, "the Claude Code driver must accept an elicitation handler")
	return eliciter.WithElicitation(elicit)
}

// useFakeClaude puts the test binary on PATH as `claude` and returns the
// directory it reports into.
func useFakeClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinked fake CLI")
	}
	bin := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(self, filepath.Join(bin, "claude")))
	report := t.TempDir()
	t.Setenv(fakeClaudeDir, report)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return report
}

func fakeClaudeReport(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

// runFakeClaude behaves like `claude --print` with a permission prompt tool:
// it asks AskUserQuestion through the tool and streams the tool's answer.
func runFakeClaude(dir string, args []string) int {
	report := func(name, value string) { _ = os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600) }
	report("args", strings.Join(args, "\n"))
	fail := func(err error) int {
		report("error", err.Error())
		return 1
	}
	emit := func(lines ...map[string]any) {
		for _, line := range lines {
			data, _ := json.Marshal(line)
			_, _ = os.Stdout.Write(append(data, '\n'))
		}
	}
	result := map[string]any{"type": "result", "subtype": "success", "result": "done"}

	configPath := flagValue(args, "--mcp-config")
	if configPath == "" {
		emit(result)
		return 0
	}
	config, err := os.Stat(configPath)
	if err != nil {
		return fail(err)
	}
	configDir, err := os.Stat(filepath.Dir(configPath))
	if err != nil {
		return fail(err)
	}
	report("modes", fmt.Sprintf("%o %o", config.Mode().Perm(), configDir.Mode().Perm()))

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fail(err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout int64             `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fail(err)
	}
	server, ok := parsed.MCPServers[claudePermissionServerName]
	if !ok || server.Timeout != claudePermissionTimeout.Milliseconds() {
		return fail(fmt.Errorf("unexpected MCP config: %s", data))
	}
	tool, ok := strings.CutPrefix(flagValue(args, "--permission-prompt-tool"), "mcp__"+claudePermissionServerName+"__")
	if !ok {
		return fail(errors.New("unexpected --permission-prompt-tool"))
	}

	//nolint:forbidigo // Runs as the fake CLI process, outside any test.
	ctx := context.Background()
	unauthorized, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("{}"))
	if err != nil {
		return fail(err)
	}
	resp, err := http.DefaultClient.Do(unauthorized)
	if err != nil {
		return fail(err)
	}
	_ = resp.Body.Close()
	report("unauthorized", strconv.Itoa(resp.StatusCode))

	client := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   server.URL,
		HTTPClient: &http.Client{Transport: headerTransport{headers: server.Headers}},
	}, nil)
	if err != nil {
		return fail(err)
	}
	defer session.Close()

	input := map[string]any{"questions": []any{map[string]any{
		"question": "Which color do you prefer?",
		"options":  []any{map[string]any{"label": "Red"}, map[string]any{"label": "Blue"}},
	}}}
	emit(
		map[string]any{"type": "system", "subtype": "init", "session_id": "claude-session"},
		map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": claudeAskUserQuestionTool, "input": input},
		}}},
	)
	answer, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{
		"tool_name": claudeAskUserQuestionTool, "input": input, "tool_use_id": "toolu_1",
	}})
	if err != nil {
		return fail(err)
	}
	text, ok := answer.Content[0].(*mcp.TextContent)
	if !ok {
		return fail(errors.New("the permission prompt tool did not answer with text"))
	}
	emit(
		map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": text.Text},
		}}},
		result,
	)
	return 0
}

func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

type headerTransport struct{ headers map[string]string }

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, want, info.Mode().Perm(), path)
}

func readClaudeMCPConfig(t *testing.T, path string) (endpoint string, headers map[string]string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var config struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout int64             `json:"timeout"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &config))
	server, ok := config.MCPServers[claudePermissionServerName]
	require.True(t, ok)
	assert.Equal(t, "http", server.Type)
	assert.True(t, strings.HasPrefix(server.URL, "http://127.0.0.1:"), server.URL)
	assert.Equal(t, int64(24*60*60*1000), server.Timeout)
	return server.URL, server.Headers
}

func postWithoutToken(t *testing.T, endpoint string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func connectClaudeMCP(t *testing.T, endpoint string, headers map[string]string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: headerTransport{headers: headers}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callPermissionPrompt(t *testing.T, session *mcp.ClientSession, toolName string, input map[string]any) claudePermissionDecision {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      claudePermissionToolName,
		Arguments: map[string]any{"tool_name": toolName, "input": input, "tool_use_id": "toolu_1"},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	var decision claudePermissionDecision
	require.NoError(t, json.Unmarshal([]byte(text.Text), &decision))
	return decision
}
