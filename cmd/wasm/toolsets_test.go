//go:build js && wasm

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/rag"
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
	for _, supported := range []string{"mcp", "think", "todo", "plan", "memory", "user_prompt", "session_context", "fetch", "api", "openapi", "model_picker", "rag"} {
		assert.True(t, registry.Has(supported), supported)
	}
	for _, unsupported := range []string{"shell", "script", "filesystem", "file", "git", "tasks", "environment", "background_jobs", "background_agents", "lsp", "mcp_catalog", "a2a", "webhook", "open_url", "scheduler"} {
		assert.False(t, registry.Has(unsupported), unsupported)
	}

	for name, tc := range map[string]struct {
		toolset latest.Toolset
		want    string
	}{
		"stdio mcp":             {latest.Toolset{Type: "mcp", Command: "npx"}, "stdio MCP servers"},
		"memory path":           {latest.Toolset{Type: "memory", Path: "memory.db"}, "memory path"},
		"local spec":            {latest.Toolset{Type: "openapi", URL: "./openapi.yaml"}, "only http(s)"},
		"no endpoint":           {latest.Toolset{Type: "api"}, "requires an endpoint"},
		"no models":             {latest.Toolset{Type: "model_picker"}, "at least one model"},
		"unknown type":          {latest.Toolset{Type: "shell"}, "unknown toolset type"},
		"rag without config":    {latest.Toolset{Type: "rag"}, "requires a rag_config"},
		"rag docs not supplied": {latest.Toolset{Type: "rag", RAGConfig: &latest.RAGConfig{Docs: []string{"guide.md"}, Strategies: []latest.RAGStrategyConfig{{Type: "bm25"}}}}, `"/guide.md" selects none of the supplied documents []`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := registry.CreateTool(t.Context(), tc.toolset, "/", testRunConfig(), "")
			require.ErrorContains(t, err, tc.want)
		})
	}

	assert.NotContains(t, toolNames(t, createTool(t, registry, latest.Toolset{Type: "plan"})), plan.ToolNameExportPlanToFile, "no files to export plans to")
}

func TestRAGRejectsWhatDocumentsCannotHonour(t *testing.T) {
	for name, tc := range map[string]struct {
		strategy string
		want     string
	}{
		"database":    {"database: rag.db", "strategies[0].database: nothing is persisted"},
		"code_aware":  {"chunking:\n      code_aware: true", "strategies[0].chunking.code_aware: tree-sitter needs cgo"},
		"respect_vcs": {"respect_vcs: true", "strategies[0].respect_vcs: there is no VCS"},
	} {
		t.Run(name, func(t *testing.T) {
			yaml := `
agents:
  root:
    model: mock/root
    toolsets:
      - type: rag
        ref: guide
rag:
  guide:
    docs: [guide.md]
    strategies:
      - type: bm25
        ` + strings.ReplaceAll(tc.strategy, "\n", "\n        ") + `
`
			h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})
			_, err := h.openSession(t.Context(), sessionOptions{YAML: yaml, Documents: rag.Documents{"guide.md": []byte("hi")}})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

const ragAgentYAML = `
models:
  embed:
    provider: mock
    model: embed
agents:
  root:
    model: mock/root
    instruction: Answer from the handbook.
    toolsets:
      - type: rag
        ref: handbook
rag:
  handbook:
    docs: [handbook]
    strategies:
      - type: bm25
      - type: chunked-embeddings
        embedding_model: embed
        vector_dimensions: 3
        max_indexing_concurrency: 1
        threshold: 0
    results:
      limit: 2
`

var handbook = rag.Documents{
	"handbook/leave.md":    []byte("Vacation: everyone gets 25 days of vacation per year. Vacation requests go to your manager."),
	"handbook/expenses.md": []byte("Expenses: file expense reports within 30 days with receipts attached."),
	"private/salaries.md":  []byte("Salaries are confidential."),
}

func TestRAGIndexesTheSessionDocuments(t *testing.T) {
	models := map[string]provider.Provider{
		"root":  newScriptedModel("mock/root", toolTurn("handbook", `{"query":"vacation days"}`), textTurn("25 days")),
		"embed": &keywordEmbedder{},
	}
	h := testHost(&echoToolSet{}, models)
	s := openTestSession(t, h, sessionOptions{YAML: ragAgentYAML, Documents: handbook})

	var c collectingEmitter
	_, err := s.send("how much vacation do I get?", c.emit)
	require.NoError(t, err)
	assert.Empty(t, c.find("tool_confirmation"), "the rag tool is read-only")
	results := c.find("tool_result")
	require.Len(t, results, 1)
	output := results[0]["output"].(string)
	assert.Contains(t, output, `"source_path":"handbook/leave.md"`, "results carry the host's logical paths")
	assert.Contains(t, output, "25 days of vacation")
	assert.NotContains(t, output, "salaries", "docs: [handbook] leaves the other documents out")
	assert.Positive(t, models["embed"].(*keywordEmbedder).calls.Load(), "the embedding strategy went through the mock model")

	models["root"] = newScriptedModel("mock/root", toolTurn("handbook", `{"query":"vacation"}`), textTurn("unlimited"))
	other := openTestSession(t, h, sessionOptions{YAML: strings.ReplaceAll(ragAgentYAML, "docs: [handbook]", "docs: [notes.md]"), Documents: rag.Documents{"notes.md": []byte("vacation is unlimited")}})
	var c2 collectingEmitter
	_, err = other.send("vacation?", c2.emit)
	require.NoError(t, err)
	require.Len(t, c2.find("tool_result"), 1)
	assert.Contains(t, c2.find("tool_result")[0]["output"], "unlimited")
	assert.NotContains(t, c2.find("tool_result")[0]["output"], "25 days", "documents are scoped to their session")
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
	model := newScriptedModel("mock/root", toolTurn("run_tools_with_javascript", `{"script":"return echo({text: \"ping\"}) + \"!\""}`), textTurn("done"))
	s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, AutoApprove: true})

	var c collectingEmitter
	_, err := s.send("run", c.emit)
	require.NoError(t, err)
	assert.Equal(t, 1, echo.callCount())
	require.Len(t, c.find("tool_result"), 1)
	assert.Contains(t, c.find("tool_result")[0]["output"], `"value":"echo: ping!"`)
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

func TestRAGExampleLoadsOverTheSessionDocuments(t *testing.T) {
	yaml, err := os.ReadFile("examples/handbook-rag.yaml")
	require.NoError(t, err)
	opts := sessionOptions{YAML: string(yaml), Env: map[string]string{"OPENROUTER_API_KEY": "k"}}
	_, err = browserHost.openSession(t.Context(), opts)
	require.ErrorContains(t, err, `"/handbook" selects none of the supplied documents []`, "without documents there is nothing to index")

	opts.Documents = handbook
	s, err := browserHost.openSession(t.Context(), opts)
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
