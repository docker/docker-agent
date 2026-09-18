package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/deferred"
)

// TestRunStream_NativeToolSearch_DispatchesUnactivatedCatalogTool drives
// provider_opts.native_tool_search end to end: a real *openai.Client with an
// empty base_url (so the first-party gate holds), rerouted to a fake
// Responses server through the HTTP transport wrapper, behind a real agent,
// deferred toolset and LocalRuntime.
//
// Turn 1 the server finds write_file through server-side tool_search and
// calls it directly, without add_tool. The runtime must run the catalog
// handler exactly once, then replay the whole hosted response (encrypted
// reasoning, commentary-phase message, tool_search_call/output pair, the
// function_call) verbatim ahead of the tool result on turn 2, with a
// byte-identical tool declaration. The session-excluded catalog tool never
// reaches the provider, and no client-side search surface is involved.
func TestRunStream_NativeToolSearch_DispatchesUnactivatedCatalogTool(t *testing.T) {
	t.Parallel()

	server := newFakeResponsesServer(t)

	var writeFileCalls atomic.Int64
	toolsets := newDeferredAgentToolSets(
		tools.Tool{
			Name:        "write_file",
			Description: "Writes a file",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}},
			Handler: func(_ context.Context, call tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
				writeFileCalls.Add(1)
				assert.JSONEq(t, `{"path":"notes.txt"}`, call.Function.Arguments)
				return tools.ResultSuccess("written"), nil
			},
		},
		tools.Tool{
			Name:       "delete_everything",
			Parameters: map[string]any{"type": "object"},
			Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				t.Error("excluded catalog tool must never run")
				return tools.ResultSuccess(""), nil
			},
		},
	)

	cfg := nativeSearchConfig()
	client, err := openai.NewClient(t.Context(), &cfg,
		environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}),
		options.WithHTTPTransportWrapper(server.reroute))
	require.NoError(t, err)
	require.True(t, client.NativeToolSearchEnabled(), "gate must hold with an empty base_url")

	root := agent.New("root", "test", agent.WithModel(client), agent.WithToolSets(toolsets...))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("write my notes"), session.WithToolsApproved(true))
	sess.NonInteractive = true
	sess.ExcludedTools = []string{"delete_everything"}

	for ev := range rt.RunStream(t.Context(), sess) {
		if e, ok := ev.(*ErrorEvent); ok {
			t.Fatalf("unexpected error event: %s", e.Error)
		}
	}

	requests := server.requests()
	require.Len(t, requests, 2, "one turn to call the tool, one to consume its result")
	assert.Equal(t, int64(1), writeFileCalls.Load(), "the unactivated catalog tool runs through its own handler")

	// Declarations: search_tool/add_tool stay regular, write_file is only a
	// deferred hosted declaration, the server searches, the excluded tool is
	// absent, and activation state cannot change any of it between turns.
	first, second := requests[0], requests[1]
	assert.Equal(t, []string{"reasoning.encrypted_content"}, first.Include)
	assert.Equal(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "write_file", "tool_search"}, first.toolNames(t))
	assert.Equal(t, []string{"write_file"}, first.deferLoadingNames(t))
	assert.Equal(t, []string{"server"}, first.toolSearchExecutions(t))
	assert.JSONEq(t, string(first.Tools), string(second.Tools), "hosted declarations must be byte-identical across turns")

	// Turn 2 replays turn 1's hosted output verbatim after the prompt
	// messages (untyped input items), then appends the tool result.
	replay := second.Input[len(second.Input)-6:]
	assert.Equal(t, []string{"reasoning", "message", "tool_search_call", "tool_search_output", "function_call", "function_call_output"}, inputTypes(replay))
	assert.NotContains(t, inputTypes(second.Input[:len(second.Input)-6]), "reasoning", "hosted output must be replayed exactly once")
	assert.Equal(t, "enc_1", replay[0]["encrypted_content"])
	assert.Equal(t, "commentary", replay[1]["phase"])
	assert.Equal(t, "server", replay[2]["execution"])
	assert.Equal(t, "server", replay[3]["execution"])
	assert.Equal(t, "call_1", replay[4]["call_id"])
	assert.Equal(t, "call_1", replay[5]["call_id"])
	assert.Equal(t, "written", replay[5]["output"])

	// Session: the hosted state of the tool-calling turn is scoped to the
	// resolved source, and the only tool the model ever called is write_file.
	var toolCallIDs []string
	var state *chat.OpenAIResponse
	for _, item := range sess.Messages {
		if item.Message == nil {
			continue
		}
		switch msg := item.Message.Message; msg.Role {
		case chat.MessageRoleAssistant:
			for _, call := range msg.ToolCalls {
				assert.Equal(t, "write_file", call.Function.Name)
				state = msg.OpenAIResponse
			}
		case chat.MessageRoleTool:
			toolCallIDs = append(toolCallIDs, msg.ToolCallID)
		}
	}
	assert.Equal(t, []string{"call_1"}, toolCallIDs)
	require.NotNil(t, state)
	assert.Equal(t, "resp_1", state.ID)
	assert.Equal(t, "openai/gpt-5.4", state.Source)
	assert.Equal(t, sess.ID, state.SessionID)
}

// TestRunStream_NativeToolSearch_CatalogToolRequiresApproval proves that a
// catalog tool the server found and called directly cannot skip the user
// prompt: the confirmation fires before the handler, a rejection keeps the
// handler from running, and an approval runs it exactly once.
func TestRunStream_NativeToolSearch_CatalogToolRequiresApproval(t *testing.T) {
	t.Parallel()

	t.Run("reject", func(t *testing.T) {
		t.Parallel()
		run := runUnapprovedNativeSearch(t, ResumeReject("wrong path"))
		assert.Equal(t, int64(0), run.handlerCalls.Load(), "a rejected catalog tool must never run")
		require.True(t, run.response.Result.IsError)
		assert.Contains(t, run.response.Response, "The user rejected the tool call.")
		assert.Contains(t, run.response.Response, "Reason: wrong path")
		assert.Equal(t, run.response.Response, run.replayedOutput(t), "the model must see the rejection, not a fabricated result")
	})

	t.Run("approve", func(t *testing.T) {
		t.Parallel()
		run := runUnapprovedNativeSearch(t, ResumeApprove())
		assert.Equal(t, int64(1), run.handlerCalls.Load(), "an approved catalog tool runs exactly once")
		require.False(t, run.response.Result.IsError)
		assert.Equal(t, "written", run.response.Response)
		assert.Equal(t, "written", run.replayedOutput(t))
	})
}

type unapprovedNativeSearchRun struct {
	server       *fakeResponsesServer
	handlerCalls atomic.Int64
	response     *ToolCallResponseEvent
}

// replayedOutput is the function_call_output the model receives on turn 2.
func (r *unapprovedNativeSearchRun) replayedOutput(t *testing.T) string {
	t.Helper()
	requests := r.server.requests()
	require.Len(t, requests, 2, "turn 2 must carry the decision back to the model")
	last := requests[1].Input[len(requests[1].Input)-1]
	require.Equal(t, "function_call_output", last["type"])
	return last["output"].(string)
}

// runUnapprovedNativeSearch drives the hosted-search turn from the dispatch
// test without ToolsApproved and answers the single prompt with decision.
// NonInteractive stays false so the runtime asks instead of auto-denying.
func runUnapprovedNativeSearch(t *testing.T, decision ResumeRequest) *unapprovedNativeSearchRun {
	t.Helper()
	run := &unapprovedNativeSearchRun{server: newFakeResponsesServer(t)}

	toolsets := newDeferredAgentToolSets(tools.Tool{
		Name:        "write_file",
		Description: "Writes a file",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			run.handlerCalls.Add(1)
			return tools.ResultSuccess("written"), nil
		},
	})

	cfg := nativeSearchConfig()
	client, err := openai.NewClient(t.Context(), &cfg,
		environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}),
		options.WithHTTPTransportWrapper(run.server.reroute))
	require.NoError(t, err)

	root := agent.New("root", "test", agent.WithModel(client), agent.WithToolSets(toolsets...))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("write my notes"))
	require.False(t, sess.ToolsApproved)

	var prompts []*ToolCallConfirmationEvent
	for ev := range rt.RunStream(t.Context(), sess) {
		switch e := ev.(type) {
		case *ErrorEvent:
			t.Fatalf("unexpected error event: %s", e.Error)
		case *ToolCallConfirmationEvent:
			assert.Equal(t, int64(0), run.handlerCalls.Load(), "the prompt must precede the handler")
			prompts = append(prompts, e)
			rt.resumeChan <- decision
		case *ToolCallResponseEvent:
			run.response = e
		}
	}

	require.Len(t, prompts, 1, "exactly one confirmation for the single hosted call")
	assert.Equal(t, "call_1", prompts[0].ToolCall.ID)
	assert.Equal(t, "write_file", prompts[0].ToolCall.Function.Name)
	assert.True(t, prompts[0].ToolDefinition.SearchOnly, "the prompt is for the catalog tool, not an activated copy")
	require.NotNil(t, run.response)
	assert.Equal(t, "call_1", run.response.ToolCallID)
	return run
}

// fakeResponsesServer serves two scripted Responses SSE streams and records
// every request body it receives.
type fakeResponsesServer struct {
	url *url.URL

	mu   sync.Mutex
	seen []responsesRequest
}

type responsesRequest struct {
	Include []string         `json:"include"`
	Tools   json.RawMessage  `json:"tools"`
	Input   []map[string]any `json:"input"`
}

func (r responsesRequest) toolDecls(t *testing.T) []map[string]any {
	t.Helper()
	var decls []map[string]any
	require.NoError(t, json.Unmarshal(r.Tools, &decls))
	return decls
}

// toolNames lists function names, falling back to the type for hosted tools.
func (r responsesRequest) toolNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, decl := range r.toolDecls(t) {
		name, _ := decl["name"].(string)
		if name == "" {
			name, _ = decl["type"].(string)
		}
		names = append(names, name)
	}
	return names
}

func (r responsesRequest) deferLoadingNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, decl := range r.toolDecls(t) {
		if decl["defer_loading"] == true {
			names = append(names, decl["name"].(string))
		}
	}
	return names
}

func (r responsesRequest) toolSearchExecutions(t *testing.T) []string {
	t.Helper()
	var executions []string
	for _, decl := range r.toolDecls(t) {
		if decl["type"] == "tool_search" {
			executions = append(executions, decl["execution"].(string))
		}
	}
	return executions
}

func inputTypes(items []map[string]any) []string {
	types := make([]string, 0, len(items))
	for _, item := range items {
		typ, _ := item["type"].(string)
		types = append(types, typ)
	}
	return types
}

func newFakeResponsesServer(t *testing.T) *fakeResponsesServer {
	t.Helper()
	s := &fakeResponsesServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var req responsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, req)
		turn := len(s.seen)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		events := finalTurnEvents()
		if turn == 1 {
			events = hostedSearchTurnEvents()
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		}
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	s.url = u
	return s
}

func (s *fakeResponsesServer) requests() []responsesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]responsesRequest(nil), s.seen...)
}

// reroute keeps the client configured for api.openai.com while sending its
// requests to the fake server.
func (s *fakeResponsesServer) reroute(next http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req = req.Clone(req.Context())
		req.URL.Scheme = s.url.Scheme
		req.URL.Host = s.url.Host
		req.Host = s.url.Host
		return next.RoundTrip(req)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// hostedSearchTurnEvents is what the Responses API streams when the model
// reasons, comments, searches the deferred catalog server-side and calls the
// tool it found, all in one response.
func hostedSearchTurnEvents() []map[string]any {
	reasoning := map[string]any{
		"type": "reasoning", "id": "rs_1", "encrypted_content": "enc_1",
		"summary": []map[string]any{{"type": "summary_text", "text": "Need write_file."}},
	}
	message := map[string]any{
		"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "phase": "commentary",
		"content": []map[string]any{{"type": "output_text", "text": "Saving your notes.", "annotations": []any{}}},
	}
	searchCall := map[string]any{
		"type": "tool_search_call", "id": "ts_1", "execution": "server", "call_id": nil, "status": "completed",
		"arguments": map[string]any{"query": "write"},
	}
	searchOutput := map[string]any{
		"type": "tool_search_output", "id": "tso_1", "execution": "server", "call_id": nil, "status": "completed",
		"tools": []map[string]any{{"type": "function", "name": "write_file", "parameters": map[string]any{"type": "object"}, "strict": true}},
	}
	callAdded := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "write_file", "arguments": "", "status": "in_progress"}
	callDone := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "write_file", "arguments": `{"path":"notes.txt"}`, "status": "completed"}

	return []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_1"}},
		{"type": "response.output_item.added", "output_index": 0, "item": reasoning},
		{"type": "response.reasoning_summary_text.delta", "item_id": "rs_1", "output_index": 0, "delta": "Need write_file."},
		{"type": "response.output_item.done", "output_index": 0, "item": reasoning},
		{"type": "response.output_item.added", "output_index": 1, "item": message},
		{"type": "response.output_item.done", "output_index": 1, "item": message},
		{"type": "response.output_item.added", "output_index": 2, "item": searchCall},
		{"type": "response.output_item.done", "output_index": 2, "item": searchCall},
		{"type": "response.output_item.added", "output_index": 3, "item": searchOutput},
		{"type": "response.output_item.done", "output_index": 3, "item": searchOutput},
		{"type": "response.output_item.added", "output_index": 4, "item": callAdded},
		{"type": "response.output_item.done", "output_index": 4, "item": callDone},
		{"type": "response.completed", "response": map[string]any{
			"id":     "resp_1",
			"output": []map[string]any{reasoning, message, searchCall, searchOutput, callDone},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		}},
	}
}

func finalTurnEvents() []map[string]any {
	message := map[string]any{
		"type": "message", "id": "msg_2", "role": "assistant", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": "Done.", "annotations": []any{}}},
	}
	return []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_2"}},
		{"type": "response.output_item.added", "output_index": 0, "item": message},
		{"type": "response.output_item.done", "output_index": 0, "item": message},
		{"type": "response.completed", "response": map[string]any{
			"id":     "resp_2",
			"output": []map[string]any{message},
			"usage":  map[string]any{"input_tokens": 20, "output_tokens": 2, "total_tokens": 22},
		}},
	}
}
