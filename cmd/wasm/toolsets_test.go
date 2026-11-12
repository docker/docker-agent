//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

func testRunConfig() *config.RuntimeConfig {
	return &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
}

func createTool(t *testing.T, registry teamloader.ToolsetRegistry, ts latest.Toolset) tools.ToolSet {
	t.Helper()
	created, err := registry.CreateTool(t.Context(), ts, "", testRunConfig(), "")
	require.NoError(t, err)
	return created
}

func toolNames(t *testing.T, ts tools.ToolSet) []string {
	t.Helper()
	list, err := ts.Tools(t.Context())
	require.NoError(t, err)
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Name)
	}
	return names
}

// callTool runs one of ts's tools directly and returns its output.
func callTool(t *testing.T, ts tools.ToolSet, name, args string) string {
	t.Helper()
	list, err := ts.Tools(t.Context())
	require.NoError(t, err)
	i := slices.IndexFunc(list, func(tool tools.Tool) bool { return tool.Name == name })
	require.NotEqual(t, -1, i, "tool %s", name)
	result, err := list[i].Handler(t.Context(), tools.ToolCall{ID: "call", Type: "function", Function: tools.FunctionCall{Name: name, Arguments: args}}, tools.NopRuntime{})
	require.NoError(t, err)
	return result.Output
}

func TestBrowserToolsetsServePortableBuiltins(t *testing.T) {
	registry := browserToolsets(nil)
	for _, supported := range []string{"datetime", "mcp", "calculator", "think", "random", "todo", "plan", "memory", "user_prompt", "session_context", "fetch", "api", "openapi", "model_picker"} {
		assert.True(t, registry.Has(supported), supported)
	}
	for _, unsupported := range []string{"shell", "script", "filesystem", "file", "git", "tasks", "environment", "background_jobs", "background_agents", "lsp", "mcp_catalog", "a2a", "webhook", "open_url", "scheduler"} {
		assert.False(t, registry.Has(unsupported), unsupported)
	}

	for name, tc := range map[string]struct {
		toolset latest.Toolset
		want    string
	}{
		"stdio mcp":    {latest.Toolset{Type: "mcp", Command: "npx"}, "stdio MCP servers"},
		"memory path":  {latest.Toolset{Type: "memory", Path: "memory.db"}, "memory path"},
		"local spec":   {latest.Toolset{Type: "openapi", URL: "./openapi.yaml"}, "only http(s)"},
		"no endpoint":  {latest.Toolset{Type: "api"}, "requires an endpoint"},
		"no models":    {latest.Toolset{Type: "model_picker"}, "at least one model"},
		"unknown type": {latest.Toolset{Type: "shell"}, "unknown toolset type"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := registry.CreateTool(t.Context(), tc.toolset, "/", testRunConfig(), "")
			require.ErrorContains(t, err, tc.want)
		})
	}

	assert.NotContains(t, toolNames(t, createTool(t, registry, latest.Toolset{Type: "plan"})), plan.ToolNameExportPlanToFile, "no files to export plans to")
}

func TestRepeatableToolsRunInASession(t *testing.T) {
	for _, codeMode := range []bool{false, true} {
		for _, tc := range []struct {
			toolset string
			tool    string
			args    string
		}{
			{toolset: "random", tool: "random_int", args: `{"min":1,"max":6}`},
			{toolset: "datetime", tool: "get_datetime", args: `{"format":"2006-01-02T15:04:05Z07:00","timezone":"UTC"}`},
		} {
			t.Run(fmt.Sprintf("%s/code_mode=%t", tc.toolset, codeMode), func(t *testing.T) {
				yaml := fmt.Sprintf(`
agents:
  root:
    model: mock/root
    code_mode_tools: %t
    toolsets:
      - type: %s
`, codeMode, tc.toolset)
				var turns []func(context.Context) (chat.MessageStream, error)
				for range 6 {
					turns = append(turns, toolTurn(tc.tool, tc.args))
				}
				turns = append(turns, textTurn("called six times"))
				model := newScriptedModel("mock/root", turns...)
				s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml})

				var c collectingEmitter
				result, err := s.send("call six times", c.emit)
				require.NoError(t, err)
				assert.Equal(t, "called six times", result["message"].(map[string]any)["content"])
				assert.Empty(t, c.find("tool_confirmation"), "repeatable tools have no external side effects")
				results := c.find("tool_result")
				require.Len(t, results, 6)
				for _, result := range results {
					assert.Equal(t, false, result["is_error"])
					output := result["output"].(string)
					if tc.toolset == "random" {
						value, err := strconv.ParseInt(output, 10, 64)
						require.NoError(t, err)
						assert.GreaterOrEqual(t, value, int64(1))
						assert.LessOrEqual(t, value, int64(6))
					} else {
						_, err := time.Parse(time.RFC3339, output)
						require.NoError(t, err)
					}
				}
			})
		}
	}
}

func TestDatetimeRunsInBrowser(t *testing.T) {
	ts := createTool(t, browserToolsets(nil), latest.Toolset{Type: "datetime"})
	assert.Equal(t, []string{"get_datetime"}, toolNames(t, ts))
	output := callTool(t, ts, "get_datetime", `{"format":"2006-01-02T15:04:05Z07:00","timezone":"Asia/Tokyo"}`)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+09:00$`, output)
}

func TestStatefulToolsetsAreSharedWithinASessionOnly(t *testing.T) {
	session, other := browserToolsets(nil), browserToolsets(nil)

	t.Run("todo", func(t *testing.T) {
		shared := latest.Toolset{Type: "todo", Shared: true}
		callTool(t, createTool(t, session, shared), "create_todo", `{"description":"ship it"}`)
		assert.Contains(t, callTool(t, createTool(t, session, shared), "list_todos", ""), "ship it", "shared lists are one per session")
		assert.NotContains(t, callTool(t, createTool(t, session, latest.Toolset{Type: "todo"}), "list_todos", ""), "ship it", "unshared lists are private")
		assert.NotContains(t, callTool(t, createTool(t, other, shared), "list_todos", ""), "ship it", "another session")
	})

	t.Run("plan", func(t *testing.T) {
		callTool(t, createTool(t, session, latest.Toolset{Type: "plan"}), "write_plan", `{"name":"release","content":"# Release"}`)
		assert.Contains(t, callTool(t, createTool(t, session, latest.Toolset{Type: "plan"}), "read_plan", `{"name":"release"}`), "# Release")
		assert.Contains(t, callTool(t, createTool(t, other, latest.Toolset{Type: "plan"}), "read_plan", `{"name":"release"}`), "not found")
	})

	t.Run("memory", func(t *testing.T) {
		callTool(t, createTool(t, session, latest.Toolset{Type: "memory"}), "add_memory", `{"memory":"likes tabs"}`)
		assert.Contains(t, callTool(t, createTool(t, session, latest.Toolset{Type: "memory"}), "get_memories", ""), "likes tabs")
		assert.NotContains(t, callTool(t, createTool(t, other, latest.Toolset{Type: "memory"}), "get_memories", ""), "likes tabs")
	})
}

func TestBrowserMemoryInstructionsMatchItsScope(t *testing.T) {
	ts := createTool(t, browserToolsets(nil), latest.Toolset{Type: "memory"})
	instructions := tools.GetInstructions(ts)
	assert.NotContains(t, instructions, "survives across sessions", "nothing outlives the session in the browser")
	assert.Contains(t, instructions, browserMemoryScope)
	assert.Contains(t, instructions, "call search_memories first", "the recall and store guidance is kept")
	assert.Equal(t, "memory", tools.DescribeToolSet(ts))
}

func TestMemoriesSurviveRestartOfTheSession(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    toolsets:
      - type: memory
`
	model := newScriptedModel("mock/root",
		toolTurn("add_memory", `{"memory":"likes tabs"}`), textTurn("noted"),
		toolTurn("get_memories", `{}`), textTurn("recalled"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, AutoApprove: true})

	_, err := s.send("remember", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	require.NoError(t, s.restart())

	var c collectingEmitter
	_, err = s.send("recall", c.emit)
	require.NoError(t, err)
	require.Len(t, c.find("tool_result"), 1)
	assert.Contains(t, c.find("tool_result")[0]["output"], "likes tabs")
}

func TestTodoToolRunsInASession(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    toolsets:
      - type: todo
`
	model := newScriptedModel("mock/root", toolTurn("create_todo", `{"description":"ship it"}`), textTurn("done"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml})

	var c collectingEmitter
	_, err := s.send("plan", c.emit)
	require.NoError(t, err)
	assert.Empty(t, c.find("tool_confirmation"), "todo tools are read-only hinted")
	require.Len(t, c.find("tool_result"), 1)
	assert.Contains(t, c.find("tool_result")[0]["output"], "ship it")
}

func TestSharedTodosReachTeammatesInTheSameSession(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    sub_agents: [helper]
    toolsets:
      - type: todo
        shared: true
  helper:
    model: mock/helper
    description: Helps.
    toolsets:
      - type: todo
        shared: true
`
	root := newScriptedModel("mock/root",
		toolTurn("create_todo", `{"description":"ship it"}`),
		toolTurn("transfer_task", `{"agent":"helper","task":"check the list","expected_output":"the list"}`),
		textTurn("done"))
	helper := newScriptedModel("mock/helper", toolTurn("list_todos", `{}`), textTurn("one todo"))
	models := map[string]provider.Provider{"root": root, "helper": helper}
	h := testHost(&echoToolSet{}, models)
	s := openTestSession(t, h, sessionOptions{YAML: yaml, AutoApprove: true})

	_, err := s.send("go", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	seen := helper.lastCall()
	require.Equal(t, chat.MessageRoleTool, seen[len(seen)-1].Role)
	assert.Contains(t, seen[len(seen)-1].Content, "ship it", "the helper reads the todo root created")

	fresh := newScriptedModel("mock/root", toolTurn("list_todos", `{}`), textTurn("empty"))
	models["root"] = fresh // the registry looks models up per session
	s2 := openTestSession(t, h, sessionOptions{YAML: yaml, AutoApprove: true})
	_, err = s2.send("list", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	seen = fresh.lastCall()
	assert.NotContains(t, seen[len(seen)-1].Content, "ship it", "a new session starts with an empty shared list")
}

func TestJavaScriptExpandsInstructionsFromTheSessionEnv(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    instruction: Hello ${env.WHO}, the answer is ${6 * 7}; secret is [${env.SECRET}].
`
	t.Setenv("SECRET", "from-process")
	model := newScriptedModel("mock/root", textTurn("ok"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, Env: map[string]string{"WHO": "browser"}})

	_, err := s.send("hi", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	require.Equal(t, chat.MessageRoleSystem, model.lastCall()[0].Role)
	assert.Contains(t, model.lastCall()[0].Content, "Hello browser, the answer is 42; secret is [].")
	assert.Equal(t, "from-process", os.Getenv("SECRET"), "the process env exists but is never consulted")
}

func TestCodeModeRunsToolsFromJavaScript(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    code_mode_tools: true
    toolsets:
      - type: echo
`
	echo := &echoToolSet{}
	model := newScriptedModel("mock/root", toolTurn("run_tools_with_javascript", `{"script":"return (await echo({text: \"ping\"})) + \"!\""}`), textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, AutoApprove: true})

	var c collectingEmitter
	_, err := s.send("run", c.emit)
	require.NoError(t, err)
	assert.Equal(t, 1, echo.callCount())
	calls := c.find("tool_call")
	require.Len(t, calls, 2)
	assert.Equal(t, "run_tools_with_javascript", calls[0]["name"])
	assert.Equal(t, "call-1", calls[0]["id"])
	assert.Equal(t, "echo", calls[1]["name"])
	assert.JSONEq(t, `{"text":"ping"}`, calls[1]["args"].(string))
	assert.NotEmpty(t, calls[1]["id"])
	assert.NotEqual(t, calls[0]["id"], calls[1]["id"])

	results := c.find("tool_result")
	require.Len(t, results, 2)
	assert.Equal(t, calls[1]["id"], results[0]["id"])
	assert.Equal(t, "echo", results[0]["name"])
	assert.Equal(t, "echo: ping", results[0]["output"])
	assert.Equal(t, false, results[0]["is_error"])
	assert.Equal(t, calls[0]["id"], results[1]["id"])
	assert.Equal(t, "run_tools_with_javascript", results[1]["name"])
	assert.Contains(t, results[1]["output"], `"value":"echo: ping!"`)
	assert.Equal(t, false, results[1]["is_error"])

	messages := model.lastCall()
	require.Equal(t, chat.MessageRoleTool, messages[len(messages)-1].Role)
	assert.Equal(t, "call-1", messages[len(messages)-1].ToolCallID)
}

func TestDeferredToolsActivateOnDemand(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    toolsets:
      - type: echo
        defer: true
`
	echo := &echoToolSet{readOnly: true}
	model := newScriptedModel("mock/root",
		toolTurn("search_tool", `{"query":"echo"}`),
		toolTurn("add_tool", `{"name":"echo"}`),
		toolTurn("echo", `{"text":"ping"}`),
		textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml})

	var c collectingEmitter
	_, err := s.send("find echo", c.emit)
	require.NoError(t, err)
	assert.Equal(t, 1, echo.callCount())
	results := c.find("tool_result")
	require.Len(t, results, 3)
	assert.Contains(t, results[0]["output"], "echo")
	assert.Contains(t, results[1]["output"], "activated")
	assert.Equal(t, "echo: ping", results[2]["output"])
}

func TestToonEncodesMatchingToolOutput(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    toolsets:
      - type: todo
        toon: list_todos
`
	model := newScriptedModel("mock/root", toolTurn("create_todo", `{"description":"ship it"}`), toolTurn("list_todos", `{}`), textTurn("done"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml})

	var c collectingEmitter
	_, err := s.send("list", c.emit)
	require.NoError(t, err)
	results := c.find("tool_result")
	require.Len(t, results, 2)
	assert.True(t, strings.HasPrefix(results[0]["output"].(string), "{"), "create_todo keeps JSON")
	listed := results[1]["output"].(string)
	assert.False(t, strings.HasPrefix(listed, "{"), "list_todos is TOON-encoded: %s", listed)
	assert.Contains(t, listed, "ship it")
}

func TestModelPickerSwitchesModelsFromTheSessionRegistry(t *testing.T) {
	const yaml = `
models:
  fast:
    provider: mock
    model: fast
  careful:
    provider: mock
    model: careful
agents:
  root:
    model: fast
    toolsets:
      - type: model_picker
        models: [fast, careful]
`
	fast := newScriptedModel("mock/fast", toolTurn("change_model", `{"model":"careful"}`))
	careful := newScriptedModel("mock/careful", textTurn("careful here"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"fast": fast, "careful": careful}), sessionOptions{YAML: yaml, AutoApprove: true})

	var c collectingEmitter
	result, err := s.send("think hard", c.emit)
	require.NoError(t, err)
	assert.Equal(t, "careful here", result["message"].(map[string]any)["content"])
	assert.Equal(t, 1, fast.callCount())
	assert.Equal(t, 1, careful.callCount(), "the turn continues on the picked model")
	require.Len(t, c.find("tool_result"), 1)
	assert.Equal(t, "Model changed to careful", c.find("tool_result")[0]["output"])
}

func TestPortableExampleLoadsWithTheDemoProviders(t *testing.T) {
	yaml, err := os.ReadFile("examples/portable-team.yaml")
	require.NoError(t, err)
	// Toolsets start lazily, so opening the session proves the config
	// passes strict loading and the browser audit without any network.
	s, err := browserHost.openSession(t.Context(), sessionOptions{YAML: string(yaml), Env: map[string]string{"OPENROUTER_API_KEY": "k", "GITHUB_TOKEN": "t"}})
	require.NoError(t, err)
	require.NoError(t, s.close())
}

// mockAPI serves a JSON endpoint and an OpenAPI spec describing it from the
// wasm process itself: Go's js/wasm net is an in-process loopback, so the
// tools' own HTTP client reaches it.
func mockAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pong":"` + r.URL.Query().Get("who") + `","auth":"` + r.Header.Get("Authorization") + `"}`))
	})
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openapi":"3.0.0","info":{"title":"mock","version":"1"},"paths":{"/ping":{"get":{"operationId":"ping","parameters":[{"name":"who","in":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}}}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestHTTPToolsReachRemoteMocks(t *testing.T) {
	server := mockAPI(t)
	yaml := `
agents:
  root:
    model: mock/root
    toolsets:
      - type: fetch
        allow_private_ips: true
      - type: api
        allow_private_ips: true
        api_config:
          name: ping
          method: GET
          endpoint: ` + server.URL + `/ping?who=${who}
          headers:
            Authorization: Bearer ${env.TOKEN}
          args:
            who:
              type: string
      - type: openapi
        allow_private_ips: true
        url: ` + server.URL + `/openapi.json
`
	model := newScriptedModel("mock/root",
		toolTurn("fetch", `{"urls":["`+server.URL+`/ping?who=fetch"]}`),
		toolTurn("ping", `{"who":"api"}`),
		toolTurn("ping", `{"who":"openapi"}`),
		textTurn("done"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, Env: map[string]string{"TOKEN": "t"}, AutoApprove: true})

	var c collectingEmitter
	_, err := s.send("ping everything", c.emit)
	require.NoError(t, err)
	results := c.find("tool_result")
	require.Len(t, results, 3)
	assert.Contains(t, results[0]["output"], `"pong":"fetch"`)
	assert.Contains(t, results[1]["output"], `"pong":"api","auth":"Bearer t"`)
	assert.Contains(t, results[2]["output"], `"pong":"openapi"`)
}

func TestHTTPToolsFailClosedWithoutOptIn(t *testing.T) {
	server := mockAPI(t)
	yaml := `
agents:
  root:
    model: mock/root
    toolsets:
      - type: fetch
`
	model := newScriptedModel("mock/root", toolTurn("fetch", `{"urls":["`+server.URL+`/ping"]}`), textTurn("done"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, AutoApprove: true})

	var c collectingEmitter
	_, err := s.send("fetch", c.emit)
	require.NoError(t, err)
	require.Len(t, c.find("tool_result"), 1)
	result := c.find("tool_result")[0]
	assert.Equal(t, true, result["is_error"])
	assert.NotContains(t, result["output"], "pong", "the guarded transport must not reach the loopback mock")
}

func TestCalculatorToolRunsInASession(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    toolsets:
      - type: calculator
`
	model := newScriptedModel("mock/root", toolTurn("calculate", `{"expression":"0.1 + 0.2"}`), textTurn("0.3"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml})

	var c collectingEmitter
	_, err := s.send("calculate", c.emit)
	require.NoError(t, err)
	assert.Empty(t, c.find("tool_confirmation"), "calculate has no side effects")
	results := c.find("tool_result")
	require.Len(t, results, 1)
	assert.JSONEq(t, `{"result":"0.3","decimal":"0.3","approximate":false}`, results[0]["output"].(string))
}
