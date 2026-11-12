package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

type elicitationPeer struct {
	output   captureWriter
	peer     io.Writer
	respond  func(map[string]any) any
	envelope func(json.RawMessage, any) any
	mu       sync.Mutex
	requests []map[string]any
}

func (p *elicitationPeer) Write(data []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return 0, err
	}
	if msg.Method != acpsdk.ClientMethodElicitationCreate {
		return p.output.Write(data)
	}
	p.mu.Lock()
	p.requests = append(p.requests, msg.Params)
	p.mu.Unlock()
	result := p.respond(msg.Params)
	if result == nil {
		return len(data), nil
	}
	var envelope any = map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}
	if p.envelope != nil {
		envelope = p.envelope(msg.ID, result)
	}
	response, err := json.Marshal(envelope)
	if err != nil {
		return 0, err
	}
	if _, err = p.peer.Write(append(response, '\n')); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (p *elicitationPeer) snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

func newElicitationPeer(t *testing.T, a *Agent, caps *acpsdk.ElicitationCapabilities, respond func(map[string]any) any) *elicitationPeer {
	t.Helper()
	reader, writer := io.Pipe()
	peer := &elicitationPeer{peer: writer, respond: respond}
	conn := a.NewConnection(peer, reader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = writer.Close(); <-conn.Done() })
	a.clientElicitation = acpsdk.ElicitationCapabilities{}
	if caps != nil {
		a.clientElicitation = *caps
	}
	return peer
}

func formRequest() *mcp.ElicitParams {
	return &mcp.ElicitParams{Message: "Choose", Mode: "form", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"answer"}}}
}

func TestElicitationNegotiationAndActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		caps *acpsdk.ElicitationCapabilities
	}{
		{name: "absent"},
		{name: "empty", caps: &acpsdk.ElicitationCapabilities{}},
		{name: "form", caps: &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}},
		{name: "url", caps: &acpsdk.ElicitationCapabilities{Url: &acpsdk.ElicitationUrlCapabilities{}}},
		{name: "both", caps: &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}, Url: &acpsdk.ElicitationUrlCapabilities{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Agent{}
			peer := newElicitationPeer(t, a, tc.caps, func(map[string]any) any {
				return map[string]any{"action": "accept", "content": map[string]any{"answer": "yes"}}
			})
			handler := a.elicitationHandler("root-session")
			if tc.caps == nil || (tc.caps.Form == nil && tc.caps.Url == nil) {
				assert.Nil(t, handler)
				return
			}
			for _, mode := range []string{"form", "url", "unknown"} {
				req := formRequest()
				req.Mode = mode
				req.Meta = mcp.Meta{"ordinary": "preserved"}
				if mode == "url" {
					req.URL = "https://example.invalid/authorize"
					req.ElicitationID = "server-id"
				}
				result, err := handler(t.Context(), req)
				require.NoError(t, err)
				supported := mode == "form" && tc.caps.Form != nil || mode == "url" && tc.caps.Url != nil
				if !supported {
					assert.Equal(t, tools.ElicitationActionDecline, result.Action)
					continue
				}
				assert.Equal(t, tools.ElicitationActionAccept, result.Action)
				if mode == "url" {
					assert.Nil(t, result.Content)
				} else {
					assert.Equal(t, map[string]any{"answer": "yes"}, result.Content)
				}
			}
			for _, req := range peer.snapshot() {
				assert.Equal(t, "root-session", req["sessionId"])
				assert.Equal(t, map[string]any{"ordinary": "preserved"}, req["_meta"])
				if req["mode"] == "url" {
					assert.NotEqual(t, "server-id", req["elicitationId"])
					assert.NotEmpty(t, req["elicitationId"])
				}
			}
			a.SetAgentConnection(a.conn)
			assert.Nil(t, a.elicitationHandler("sid"), "unwrapped connections must retain headless fallback")
		})
	}
	for _, action := range []string{"accept", "decline", "cancel", "unknown", ""} {
		t.Run("action/"+action, func(t *testing.T) {
			a := &Agent{}
			newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any {
				return map[string]any{"action": action, "content": map[string]any{"answer": "yes"}}
			})
			result, err := a.elicitationHandler("sid")(t.Context(), formRequest())
			require.NoError(t, err)
			want := tools.ElicitationAction(action)
			if action == "unknown" || action == "" {
				want = tools.ElicitationActionDecline
			}
			assert.Equal(t, want, result.Action)
			if action != "accept" {
				assert.Nil(t, result.Content)
			}
		})
	}
}

func TestElicitationRejectsUnsafeOrLossyRequests(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}, Url: &acpsdk.ElicitationUrlCapabilities{}}, func(map[string]any) any {
		t.Error("unsafe request reached client")
		return map[string]any{"action": "decline"}
	})
	handler := a.elicitationHandler("sid")
	for _, change := range []func(*mcp.ElicitParams){
		func(r *mcp.ElicitParams) { r.Meta = mcp.Meta{elicitationSessionKey: "wrong-session"} },
		func(r *mcp.ElicitParams) { r.Meta = mcp.Meta{"docker-agent/type": "oauth_flow"} },
		func(r *mcp.ElicitParams) { r.Meta = mcp.Meta{"docker-agent/type": "oauth_client_credentials"} },
		func(r *mcp.ElicitParams) { r.Meta = mcp.Meta{"big": int64(9007199254740993)} },
		func(r *mcp.ElicitParams) {
			r.RequestedSchema = map[string]any{"type": "object", "properties": map[string]any{"password": map[string]any{"type": "string"}}}
		},
		func(r *mcp.ElicitParams) {
			r.RequestedSchema = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string", "format": "password"}}}
		},
		func(r *mcp.ElicitParams) {
			r.RequestedSchema = map[string]any{"type": "object", "properties": map[string]any{}, "$ref": "https://never-fetch.invalid/schema"}
		},
		func(r *mcp.ElicitParams) { r.Mode = "url"; r.URL = "file:///private" },
		func(r *mcp.ElicitParams) { r.Mode = "url"; r.URL = "https://user:password@example.invalid" },
	} {
		req := formRequest()
		change(req)
		result, err := handler(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, tools.ElicitationActionDecline, result.Action)
	}
	assert.Empty(t, peer.snapshot())
}

func TestElicitationResponseSchemaValidation(t *testing.T) {
	t.Parallel()
	for _, content := range []any{nil, map[string]any{}, map[string]any{"answer": 2}, map[string]any{"answer": ""}, map[string]any{"answer": "yes", "unexpected": "private"}} {
		a := &Agent{}
		newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any { return map[string]any{"action": "accept", "content": content} })
		result, err := a.elicitationHandler("sid")(t.Context(), formRequest())
		require.NoError(t, err)
		assert.Equal(t, tools.ElicitationActionDecline, result.Action)
	}
}

func TestElicitationSchemaSubset(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"text":        map[string]any{"type": "string", "pattern": "^ok$"},
		"number":      map[string]any{"type": "number", "minimum": 1, "maximum": 3},
		"integer":     map[string]any{"type": "integer"},
		"flag":        map[string]any{"type": "boolean"},
		"enum":        map[string]any{"type": "string", "enum": []string{"a", "b"}},
		"titled":      map[string]any{"type": "string", "oneOf": []any{map[string]any{"const": "x", "title": "X"}}},
		"array":       map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"a", "b"}}},
		"titledArray": map[string]any{"type": "array", "items": map[string]any{"anyOf": []any{map[string]any{"const": "x", "title": "X"}}}},
	}}
	_, resolved, err := elicitationSchema(schema)
	require.NoError(t, err)
	require.NoError(t, resolved.Validate(map[string]any{"text": "ok", "number": 2, "array": []any{"a"}, "titledArray": []any{"x"}}))
	require.Error(t, resolved.Validate(map[string]any{"text": "wrong"}))
	for _, field := range []map[string]any{
		{"type": "object", "properties": map[string]any{}}, {"type": "array", "items": map[string]any{"type": "number"}}, {"type": "string", "enumNames": []string{"old"}}, {"type": "string", "allOf": []any{}},
	} {
		_, _, err := elicitationSchema(map[string]any{"type": "object", "properties": map[string]any{"field": field}})
		require.Error(t, err)
	}
}

type shortElicitationWriter struct{}

func (shortElicitationWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestElicitationTransportScopeAndWrites(t *testing.T) {
	t.Parallel()
	input := []byte(`{"jsonrpc":"2.0","id":9007199254740993,"method":"elicitation/create","params":{"mode":"url","_meta":{"docker-agent/internal-acp-session":"sid","number":9007199254740993}}}` + "\n")
	var out bytes.Buffer
	writer := &elicitationWriter{output: &out}
	n, err := writer.Write(input)
	require.NoError(t, err)
	assert.Equal(t, len(input), n)
	assert.Contains(t, out.String(), `"id":9007199254740993`)
	assert.Contains(t, out.String(), `"number":9007199254740993`)
	assert.Contains(t, out.String(), `"sessionId":"sid"`)
	assert.NotContains(t, out.String(), elicitationSessionKey)
	writer.output = shortElicitationWriter{}
	_, err = writer.Write(input)
	require.ErrorIs(t, err, io.ErrShortWrite)
	for _, params := range []string{`{}`, `{"_meta":{}}`, `{"sessionId":"spoof","_meta":{"docker-agent/internal-acp-session":"sid"}}`} {
		_, err = writer.Write([]byte(`{"method":"elicitation/create","params":` + params + `}`))
		require.Error(t, err)
	}
	out.Reset()
	writer.output = &out
	unchanged := []byte(`{"method":"session/update","params":{"text":"elicitation/create"}}` + "\n")
	_, err = writer.Write(unchanged)
	require.NoError(t, err)
	assert.Equal(t, unchanged, out.Bytes())
}

func TestNegotiatedElicitationRealRuntimeAndColdResume(t *testing.T) {
	t.Parallel()
	for _, atStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "tool", true: "start"}[atStart], func(t *testing.T) {
			t.Parallel()
			a := NewAgent(config.NewBytesSource("agent.yaml", nil), &config.RuntimeConfig{}, session.NewInMemorySessionStore())
			var approved atomic.Bool
			approved.Store(true)
			outcomes := make(chan elicitationOutcome, 2)
			a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
				ts := &acpElicitingToolset{request: formRequest(), atStart: atStart, approved: &approved, outcomes: outcomes}
				prov := &elicitationTestProvider{approvalTestProvider: approvalTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "bridge")}, toolNames: []string{"ask_user"}}}
				root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts))
				return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
			}
			peer := newElicitationPeer(t, a, nil, func(map[string]any) any {
				return map[string]any{"action": "accept", "content": map[string]any{"answer": "yes"}}
			})
			defer func() { require.NoError(t, a.Stop(t.Context())) }()
			_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{ClientCapabilities: acpsdk.ClientCapabilities{Elicitation: &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}}})
			require.NoError(t, err)
			wd := t.TempDir()
			created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
			require.NoError(t, err)
			for turn := range 2 {
				if turn == 1 {
					_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
					require.NoError(t, err)
					_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
					require.NoError(t, err)
				}
				a.sessions[string(created.SessionId)].sess.SetToolsApproved(true)
				resp, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("ask")}})
				require.NoError(t, err)
				assert.Equal(t, acpsdk.StopReasonEndTurn, resp.StopReason)
				outcome := <-outcomes
				require.NoError(t, outcome.err)
				assert.Equal(t, tools.ElicitationActionAccept, outcome.result.Action)
			}
			requests := peer.snapshot()
			require.Len(t, requests, 2)
			for _, req := range requests {
				assert.Equal(t, string(created.SessionId), req["sessionId"])
			}
		})
	}
}

func TestElicitationCancellationAndDisconnect(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := &Agent{}
		entered := make(chan struct{})
		peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any { close(entered); return nil })
		handler := a.elicitationHandler("sid")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := handler(ctx, formRequest()); done <- err }()
		<-entered
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		synctest.Wait()
		assert.Contains(t, strings.Join(peer.output.lines(), "\n"), "$/cancel_request")
	})
	a := &Agent{}
	peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any { return nil })
	require.NoError(t, peer.peer.(io.Closer).Close())
	<-a.conn.Done()
	result, err := a.elicitationHandler("sid")(t.Context(), formRequest())
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action)
}

func TestElicitationTransportPassesUnrelatedResponses(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":"elicitation/create","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"title":"elicitation/create"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"data":"elicitation/create"}}`,
	} {
		var out bytes.Buffer
		n, err := (&elicitationWriter{output: &out}).Write([]byte(input))
		require.NoError(t, err)
		assert.Equal(t, len(input), n)
		assert.Equal(t, input, out.String())
	}
}

func TestElicitationRejectsCredentialPromptsInAllLabels(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any { t.Error("credential form sent"); return map[string]any{"action": "decline"} })
	for _, location := range []string{"message", "title", "description", "field-title", "field-description"} {
		req := formRequest()
		schema := req.RequestedSchema.(map[string]any)
		field := schema["properties"].(map[string]any)["answer"].(map[string]any)
		switch location {
		case "message":
			req.Message = "Enter your API key"
		case "title", "description":
			schema[location] = "Enter an access token"
		case "field-title":
			field["title"] = "Password"
		case "field-description":
			field["description"] = "API key granting access to your account"
		}
		result, err := a.elicitationHandler("sid")(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, tools.ElicitationActionDecline, result.Action)
	}
	assert.Empty(t, peer.snapshot())
}

func TestElicitationRejectsMixedEnumBranches(t *testing.T) {
	t.Parallel()
	for _, branch := range []map[string]any{
		{"const": "ok", "title": "OK", "format": "password"},
		{"const": "ok", "title": "OK", "x-unsupported": true},
		{"const": "ok", "title": "OK", "$ref": "https://never-fetch.invalid"},
	} {
		_, _, err := elicitationSchema(map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string", "enum": []string{"ok"}, "oneOf": []any{branch}}}})
		require.Error(t, err)
	}
}

func TestElicitationNumbersBeforeSDKDecoding(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		number string
		safe   bool
	}{
		{"0.1", true},
		{"1.000", true},
		{"1e3", true},
		{"-123", true},
		{"0.1234567890123456789", false},
		{"1e-400", false},
		{"1.0000000000000001", false},
		{"9007199254740993", false},
		{"1e999999999", false},
	} {
		t.Run(test.number, func(t *testing.T) {
			assert.Equal(t, test.safe, safeElicitationNumbers([]byte(`{"number":`+test.number+`}`)))
			a := &Agent{}
			newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any {
				return json.RawMessage(`{"action":"accept","content":{"answer":` + test.number + `}}`)
			})
			req := formRequest()
			req.RequestedSchema = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "number"}}}
			result, err := a.elicitationHandler("sid")(t.Context(), req)
			require.NoError(t, err)
			if test.safe {
				assert.Equal(t, tools.ElicitationActionAccept, result.Action)
			} else {
				assert.Equal(t, tools.ElicitationActionDecline, result.Action)
				assert.Nil(t, result.Content)
			}
		})
	}
}

func TestElicitationReaderPreservesOtherTraffic(t *testing.T) {
	t.Parallel()
	input := `{"id":9007199254740993,"result":{"number":1.0000000000000001}}` + "\n" + `{"id":2,"method":"session/prompt","params":{"prompt":[]}}` + "\n"
	reader := &elicitationReader{scanner: bufio.NewScanner(strings.NewReader(input)), pending: &elicitationRequests{ids: map[string]string{}}}
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, input, string(got))
}

func TestElicitationConcurrentSessionsAndURLIDs(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Url: &acpsdk.ElicitationUrlCapabilities{}}, func(req map[string]any) any {
		return map[string]any{"action": "accept", "content": map[string]any{"token": "do not forward"}}
	})
	var wg sync.WaitGroup
	for _, sid := range []string{"first", "second"} {
		handler := a.elicitationHandler(sid)
		wg.Go(func() {
			for range 10 {
				result, err := handler(t.Context(), &mcp.ElicitParams{Mode: "url", Message: "Authorize", URL: "https://example.invalid/flow", ElicitationID: "duplicate-server-id"})
				require.NoError(t, err)
				assert.Equal(t, tools.ElicitationActionAccept, result.Action)
				assert.Nil(t, result.Content)
			}
		})
	}
	wg.Wait()
	requests := peer.snapshot()
	require.Len(t, requests, 20)
	ids := map[any]bool{}
	sessions := map[any]int{}
	for _, req := range requests {
		assert.False(t, ids[req["elicitationId"]])
		ids[req["elicitationId"]] = true
		sessions[req["sessionId"]]++
	}
	assert.Equal(t, 10, sessions["first"])
	assert.Equal(t, 10, sessions["second"])
}

func TestElicitationClientStdioMCPMultiRoundTrip(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	prov := &clientMCPProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "elicit-mcp")}}
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov))))}, nil
	}
	peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any {
		return map[string]any{"action": "accept", "content": map[string]any{"answer": "chosen"}}
	})
	wd := t.TempDir()
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	spec := clientServerSpec(t, "eliciting", "value")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_ELICIT", Value: "1"})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	a.sessions[string(created.SessionId)].sess.SetToolsApproved(true)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	response, err := a.Prompt(ctx, acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("ask")}})
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	assert.Contains(t, prov.result, "chosen")
	requests := peer.snapshot()
	require.Len(t, requests, 1)
	assert.Equal(t, string(created.SessionId), requests[0]["sessionId"])
}

func TestElicitationPromptCancelCloseDeleteAndStop(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "close", "delete", "stop"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := clientMCPAgent(t)
				var approved atomic.Bool
				approved.Store(true)
				outcomes := make(chan elicitationOutcome, 1)
				a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
					toolset := &acpElicitingToolset{request: formRequest(), approved: &approved, outcomes: outcomes}
					prov := &elicitationTestProvider{approvalTestProvider: approvalTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "cancel-elicit")}, toolNames: []string{"ask_user"}}}
					return &teamloader.LoadResult{Team: team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(toolset))))}, nil
				}
				entered := make(chan struct{})
				peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any { close(entered); return nil })
				defer func() { require.NoError(t, a.Stop(t.Context())) }()
				created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
				require.NoError(t, err)
				a.sessions[string(created.SessionId)].sess.SetToolsApproved(true)
				done := make(chan error, 1)
				go func() {
					response, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("ask")}})
					assert.Equal(t, acpsdk.StopReasonCancelled, response.StopReason)
					done <- err
				}()
				<-entered
				switch mode {
				case "cancel":
					err = a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: created.SessionId})
				case "close":
					_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
				case "delete":
					_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: created.SessionId})
				case "stop":
					err = a.Stop(t.Context())
				}
				require.NoError(t, err)
				require.NoError(t, <-done)
				require.ErrorIs(t, (<-outcomes).err, context.Canceled)
				synctest.Wait()
				assert.Contains(t, strings.Join(peer.output.lines(), "\n"), "$/cancel_request")
			})
		})
	}
}

func TestElicitationReaderChecksEquivalentIDsAndEmptyMethod(t *testing.T) {
	t.Parallel()
	pending := &elicitationRequests{ids: map[string]string{"1": "form"}}
	input := `{"jsonrpc":"2.0","id":1.0,"method":"","result":{"action":"accept","content":{"answer":1.0000000000000001}}}` + "\n"
	reader := &elicitationReader{scanner: bufio.NewScanner(strings.NewReader(input)), pending: pending}
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"action":"decline"`)
	assert.NotContains(t, string(data), "1.0000000000000001")
	assert.Empty(t, pending.ids)
}

func TestElicitationPendingRequestsReleaseOnCancellation(t *testing.T) {
	t.Parallel()
	pending := &elicitationRequests{ids: map[string]string{}}
	for i := range 128 {
		require.NoError(t, pending.add(json.RawMessage(strconv.Itoa(i)), "form"))
	}
	require.Error(t, pending.add(json.RawMessage("200"), "form"))
	var out bytes.Buffer
	writer := &elicitationWriter{output: &out, pending: pending}
	_, err := writer.Write([]byte(`{"method":"$/cancel_request","params":{"requestId":1}}`))
	require.NoError(t, err)
	require.NoError(t, pending.add(json.RawMessage("200"), "form"))
}

func TestBackgroundElicitationUsesOwningACPSession(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	var approved atomic.Bool
	approved.Store(true)
	outcomes := make(chan elicitationOutcome, 1)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		ts := &acpElicitingToolset{request: formRequest(), approved: &approved, outcomes: outcomes}
		worker := agent.New("worker", "test", agent.WithToolSets(ts), agent.WithModel(&elicitationTestProvider{approvalTestProvider: approvalTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "worker")}, toolNames: []string{"ask_user"}}}))
		root := agent.New("root", "test", agent.WithSubAgents(worker), agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "root")}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root, worker))}, nil
	}
	peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any {
		return map[string]any{"action": "accept", "content": map[string]any{"answer": "yes"}}
	})
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	s.sess.SetToolsApproved(true)
	runner, ok := s.rt.(agenttool.Runner)
	require.True(t, ok)
	result := runner.RunAgent(t.Context(), agenttool.RunParams{ParentSession: s.sess, AgentName: "worker", Task: "ask"})
	require.Empty(t, result.ErrMsg)
	outcome := <-outcomes
	require.NoError(t, outcome.err)
	assert.Equal(t, tools.ElicitationActionAccept, outcome.result.Action)
	requests := peer.snapshot()
	require.Len(t, requests, 1)
	assert.Equal(t, string(created.SessionId), requests[0]["sessionId"])
}

func TestElicitationReaderMatchesSDKEnvelope(t *testing.T) {
	t.Parallel()
	lossy := `{"action":"accept","content":{"answer":1.0000000000000001}}`
	for _, input := range []string{
		`{"id":1,"error":null,"result":` + lossy + `}`,
		`{"ID":1,"result":` + lossy + `}`,
		`{"id":2,"ID":1,"result":` + lossy + `}`,
		`{"id":1,"RESULT":` + lossy + `}`,
		`{"id":1,"method":42,"result":{}}` + "\n" + `{"id":1,"result":` + lossy + `}`,
		`{"id":1.` + strings.Repeat("0", 260) + `,"result":` + lossy + `}`,
		`{"id":1e+` + strings.Repeat("0", 260) + `,"result":` + lossy + `}`,
	} {
		pending := &elicitationRequests{ids: map[string]string{"1": "form"}}
		reader := &elicitationReader{scanner: bufio.NewScanner(strings.NewReader(input + "\n")), pending: pending}
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"action":"decline"`, input)
		assert.NotContains(t, string(data), "1.0000000000000001", input)
		assert.Empty(t, pending.ids)
	}
}

func TestElicitationWireRejectsEnvelopePrecisionBypasses(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"null error", "upper id", "duplicate id", "long decimal id"} {
		t.Run(variant, func(t *testing.T) {
			a := &Agent{}
			peer := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}, func(map[string]any) any {
				return json.RawMessage(`{"action":"accept","content":{"answer":1.0000000000000001}}`)
			})
			peer.envelope = func(id json.RawMessage, result any) any {
				raw := string(result.(json.RawMessage))
				switch variant {
				case "null error":
					return json.RawMessage(`{"id":` + string(id) + `,"error":null,"result":` + raw + `}`)
				case "upper id":
					return json.RawMessage(`{"ID":` + string(id) + `,"result":` + raw + `}`)
				case "duplicate id":
					return json.RawMessage(`{"id":99999,"ID":` + string(id) + `,"result":` + raw + `}`)
				default:
					return json.RawMessage(`{"id":` + string(id) + `.` + strings.Repeat("0", 260) + `,"result":` + raw + `}`)
				}
			}
			req := formRequest()
			req.RequestedSchema = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer", "minimum": 1, "maximum": 1}}}
			result, err := a.elicitationHandler("sid")(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, tools.ElicitationActionDecline, result.Action)
			assert.Nil(t, result.Content)
		})
	}
}

func TestElicitationReaderBoundsNormalizedReplies(t *testing.T) {
	t.Parallel()
	const prefix = `{"jsonrpc":"2.0","id":1,"result":{"action":"accept","content":{"answer":"`
	const suffix = `"}}}`
	boundary := (maxElicitationBytes - len(prefix) - len(suffix)) / len(`\u003c`)
	for _, tc := range []struct {
		name   string
		input  string
		accept bool
	}{
		{name: "within encoded limit", input: prefix + strings.Repeat("<", boundary) + suffix, accept: true},
		{name: "escaping exceeds encoded limit", input: prefix + strings.Repeat("<", boundary+1) + suffix},
		{name: "oversized envelope metadata", input: `{"jsonrpc":"` + strings.Repeat("<", boundary) + `","id":1,"params":{"extra":"<"},"result":{"action":"accept","content":{"answer":"yes"}}}`},
		{name: "oversized echoed id", input: `{"id":1e` + strings.Repeat("0", maxElicitationBytes) + `,"result":{"action":"accept","content":{"answer":"yes"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const following = `{"jsonrpc":"2.0","id":"next","method":"session/prompt","params":{"prompt":[]}}`
			pending := &elicitationRequests{ids: map[string]string{"1": "form"}}
			reader := &elicitationReader{scanner: bufio.NewScanner(strings.NewReader(tc.input + "\n" + following + "\n")), pending: pending}
			reader.scanner.Buffer(make([]byte, 4096), 10<<20)
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			require.Len(t, lines, 2)
			assert.LessOrEqual(t, len(lines[0]), maxElicitationBytes)
			if tc.accept {
				assert.Contains(t, lines[0], `"action":"accept"`)
			} else {
				assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"action":"decline"}}`, lines[0])
			}
			assert.Equal(t, following, lines[1], "reject only the oversized reply, not the connection") //nolint:testifylint // Pin byte-for-byte passthrough, not JSON equivalence.
			assert.Empty(t, pending.ids)
		})
	}
}

func TestElicitationOversizedNormalizedReplyDoesNotDisconnect(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	peerInput, send := io.Pipe()
	receive, peerOutput := io.Pipe()
	conn := a.NewConnection(peerOutput, peerInput)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	a.clientElicitation = acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}}
	handler := a.elicitationHandler("session")
	decoder := json.NewDecoder(receive)
	for _, oversized := range []bool{true, false} {
		outcomes := make(chan elicitationOutcome, 1)
		go func() {
			result, err := handler(t.Context(), formRequest())
			outcomes <- elicitationOutcome{result: result, err: err}
		}()
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		require.NoError(t, decoder.Decode(&request))
		answer := "yes"
		if oversized {
			answer = strings.Repeat("<", maxElicitationBytes/6)
		}
		// Send literal HTML characters; json.Marshal would escape them before the reader.
		wire := `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"action":"accept","content":{"answer":"` + answer + `"}}}` + "\n"
		require.Less(t, len(wire), maxElicitationBytes)
		_, err := io.WriteString(send, wire)
		require.NoError(t, err)
		outcome := <-outcomes
		require.NoError(t, outcome.err)
		if oversized {
			assert.Equal(t, tools.ElicitationActionDecline, outcome.result.Action)
			assert.Nil(t, outcome.result.Content)
		} else {
			assert.Equal(t, tools.ElicitationActionAccept, outcome.result.Action)
			assert.Equal(t, map[string]any{"answer": "yes"}, outcome.result.Content)
		}
	}
}
