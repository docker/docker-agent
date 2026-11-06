package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

const testTraceParent = "00-11111111111111111111111111111111-2222222222222222-01"

func installACPTraceRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		require.NoError(t, tp.Shutdown(context.WithoutCancel(t.Context())))
	})
	return recorder
}

func TestACPTraceMetadata(t *testing.T) {
	recorder := installACPTraceRecorder(t)
	member, err := baggage.NewMember("private", "host-secret")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	parent, cancel := context.WithCancel(baggage.ContextWithBaggage(t.Context(), bag))
	defer cancel()
	meta := map[string]any{"traceparent": testTraceParent, "tracestate": "vendor=value", "baggage": "private=client-secret", "arbitrary": "untouched"}
	ctx, span := startACPRequest(parent, "session/prompt", meta)
	sc := trace.SpanContextFromContext(ctx)
	assert.Equal(t, "11111111111111111111111111111111", sc.TraceID().String())
	assert.Equal(t, "vendor=value", sc.TraceState().String())
	output := traceMeta(ctx, meta)
	assert.NotContains(t, output, "baggage")
	assert.Equal(t, "untouched", output["arbitrary"])
	assert.Equal(t, testTraceParent, meta["traceparent"])
	assert.Contains(t, meta, "baggage")
	assert.NotEqual(t, testTraceParent, output["traceparent"])
	trace.SpanFromContext(ctx).RecordError(errors.New("private downstream error"))
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("private.path", "private path"))
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	span.End()
	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.True(t, ended[0].Parent().IsRemote())
	assert.Equal(t, trace.SpanKindServer, ended[0].SpanKind())
	assert.Equal(t, "acp.session/prompt", ended[0].Name())
	require.Len(t, ended[0].Attributes(), 1)
	assert.Empty(t, ended[0].Events())
	clean := traceMeta(t.Context(), meta)
	assert.NotContains(t, clean, "traceparent")
	assert.NotContains(t, clean, "tracestate")
	assert.NotContains(t, clean, "baggage")
	assert.Equal(t, "untouched", clean["arbitrary"])
	assert.Nil(t, traceMeta(t.Context(), nil))
	for _, bad := range []any{123, nil, []string{testTraceParent}, "invalid", strings.Repeat("a", 513)} {
		child, s := startACPRequest(t.Context(), "initialize", map[string]any{"traceparent": bad, "tracestate": "vendor=value"})
		assert.NotEqual(t, sc.TraceID(), trace.SpanContextFromContext(child).TraceID())
		s.End()
	}
	child, s := startACPRequest(t.Context(), "initialize", map[string]any{"traceparent": testTraceParent, "tracestate": strings.Repeat("x", 513)})
	assert.Equal(t, sc.TraceID(), trace.SpanContextFromContext(child).TraceID())
	assert.Empty(t, trace.SpanContextFromContext(child).TraceState().String())
	s.End()
}

type tracedACPProvider struct {
	mockProvider

	contexts chan trace.SpanContext
	release  <-chan struct{}
}

func (p *tracedACPProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.contexts <- trace.SpanContextFromContext(ctx)
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "answer"}}}}, {Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}}}}, nil
}

func TestACPTracePromptIsolationWire(t *testing.T) {
	recorder := installACPTraceRecorder(t)
	release := make(chan struct{})
	provider := &tracedACPProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "traces")}, contexts: make(chan trace.SpanContext, 8), release: release}
	a := NewAgent(config.NewBytesSource("test", nil), &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(provider))))}, nil
	}
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := a.NewConnection(output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	sendCall := func(id int, method string, params any) {
		t.Helper()
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
	}
	type response struct {
		ID     *int                       `json:"id"`
		Result json.RawMessage            `json:"result"`
		Error  *acpsdk.RequestError       `json:"error"`
		Params acpsdk.SessionNotification `json:"params"`
	}
	readResponse := func() response {
		t.Helper()
		for {
			var r response
			require.NoError(t, decoder.Decode(&r))
			if r.ID != nil {
				require.Nil(t, r.Error)
				return r
			}
		}
	}
	sendCall(1, "initialize", acpsdk.InitializeRequest{ProtocolVersion: 1, Meta: map[string]any{"traceparent": testTraceParent}})
	readResponse()
	var ids []acpsdk.SessionId
	for i := range 2 {
		sendCall(2+i, "session/new", acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{}, Meta: map[string]any{"traceparent": testTraceParent}})
		r := readResponse()
		var created acpsdk.NewSessionResponse
		require.NoError(t, json.Unmarshal(r.Result, &created))
		ids = append(ids, created.SessionId)
	}
	parents := []string{"00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01", "00-cccccccccccccccccccccccccccccccc-dddddddddddddddd-01"}
	for i, id := range ids {
		sendCall(10+i, "session/prompt", acpsdk.PromptRequest{SessionId: id, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("private prompt")}, Meta: map[string]any{"traceparent": parents[i], "baggage": "secret=private"}})
	}
	// Read notifications concurrently so SDK writes cannot hold up provider entry.
	var mu sync.Mutex
	notifications := map[acpsdk.SessionId][]map[string]any{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		completed := 0
		for completed < 2 {
			var r response
			if err := decoder.Decode(&r); err != nil {
				t.Error(err)
				return
			}
			if r.ID != nil {
				assert.Nil(t, r.Error)
				completed++
			} else {
				mu.Lock()
				notifications[r.Params.SessionId] = append(notifications[r.Params.SessionId], r.Params.Meta)
				mu.Unlock()
			}
		}
	}()
	first, second := <-provider.contexts, <-provider.contexts
	assert.NotEqual(t, first.TraceID(), second.TraceID())
	assert.ElementsMatch(t, []string{strings.Split(parents[0], "-")[1], strings.Split(parents[1], "-")[1]}, []string{first.TraceID().String(), second.TraceID().String()})
	close(release)
	<-done
	mu.Lock()
	for i, id := range ids {
		require.NotEmpty(t, notifications[id])
		for _, meta := range notifications[id] {
			assert.Contains(t, meta["traceparent"], strings.Split(parents[i], "-")[1])
			assert.NotContains(t, meta, "baggage")
		}
	}
	mu.Unlock()
	sendCall(20, "session/prompt", acpsdk.PromptRequest{SessionId: ids[0], Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("untraced")}})
	readResponse()
	third := <-provider.contexts
	assert.NotEqual(t, first.TraceID(), third.TraceID())
	assert.NotEqual(t, second.TraceID(), third.TraceID())
	assert.NotEqual(t, "11111111111111111111111111111111", third.TraceID().String())
	var prompts int
	for _, s := range recorder.Ended() {
		if s.Name() == "acp.session/prompt" {
			prompts++
			assert.Equal(t, trace.SpanKindServer, s.SpanKind())
			for _, attr := range s.Attributes() {
				assert.NotContains(t, attr.Value.AsString(), "private")
			}
		}
	}
	assert.Equal(t, 3, prompts)
}

func TestACPTraceOutboundBoundaries(t *testing.T) {
	t.Parallel()
	ctx := (propagation.TraceContext{}).Extract(t.Context(), propagation.MapCarrier{"traceparent": testTraceParent, "tracestate": "vendor=value"})
	fs, _, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
	ctx = withSessionID(ctx, "policy-session")
	for _, call := range []struct{ name, args string }{{"read_file", `{"path":"file.txt"}`}, {"read_multiple_files", `{"paths":["file.txt"]}`}, {"write_file", `{"path":"file.txt","content":"new"}`}, {"edit_file", `{"path":"file.txt","edits":[{"oldText":"original","newText":"new"}]}`}} {
		available, err := fs.Tools(ctx)
		require.NoError(t, err)
		for _, tool := range available {
			if tool.Name == call.name {
				result, err := tool.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: call.name, Arguments: call.args}}, tools.NopRuntime{})
				require.NoError(t, err)
				require.False(t, result.IsError, result.Output)
			}
		}
	}
	fs.agent.buildUserMessage(ctx, "policy-session", []acpsdk.ContentBlock{acpsdk.ResourceLinkBlock("file", fileURI(filepath.Join(fs.workingDir, "file.txt")))})
	peer.mu.Lock()
	for _, req := range peer.reads {
		assert.Equal(t, testTraceParent, req.Meta["traceparent"])
	}
	for _, req := range peer.writes {
		assert.Equal(t, testTraceParent, req.Meta["traceparent"])
	}
	peer.mu.Unlock()
	ts, owner, _, terminal := newTerminalFixture(t, latest.Toolset{})
	terminalCtx := context.WithValue(ctx, terminalOwnerKey{}, owner.terminals)
	result := callTerminalTool(t, terminalCtx, ts, `{"cmd":"echo test"}`)
	require.False(t, result.IsError)
	opCtx, op, err := owner.terminals.acquire(terminalCtx)
	require.NoError(t, err)
	require.NoError(t, op.create(opCtx, acpsdk.CreateTerminalRequest{SessionId: owner.terminals.sid, Command: "unused", Meta: map[string]any{"traceparent": "stale", "baggage": "private"}}))
	require.NoError(t, op.kill(opCtx))
	require.NoError(t, op.release(opCtx))
	op.finish()
	for _, req := range terminal.snapshot() {
		var p struct {
			Meta map[string]any `json:"_meta"`
		}
		require.NoError(t, json.Unmarshal(req.params, &p))
		assert.Equal(t, testTraceParent, p.Meta["traceparent"])
		assert.NotContains(t, p.Meta, "baggage")
	}
	a := &Agent{}
	elicitation := newElicitationPeer(t, a, &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}, Url: &acpsdk.ElicitationUrlCapabilities{}}, func(map[string]any) any { return map[string]any{"action": "decline"} })
	for _, mode := range []string{"form", "url"} {
		req := formRequest()
		req.Mode = mode
		req.Meta = map[string]any{"traceparent": "hostile-server", "baggage": "private", "other": "retained"}
		if mode == "url" {
			req.URL = "https://example.invalid/auth"
			req.ElicitationID = "id"
		}
		_, err := a.elicitationHandler("session")(ctx, req)
		require.NoError(t, err)
	}
	for _, req := range elicitation.snapshot() {
		meta := req["_meta"].(map[string]any)
		assert.Equal(t, testTraceParent, meta["traceparent"])
		assert.NotContains(t, meta, "baggage")
		assert.Equal(t, "retained", meta["other"])
		assert.NotContains(t, meta, elicitationSessionKey)
	}
}

func TestACPTracePermissionsAndHostileReplies(t *testing.T) {
	t.Parallel()
	ctx := (propagation.TraceContext{}).Extract(t.Context(), propagation.MapCarrier{"traceparent": testTraceParent})
	f := newRunAgentFixtureWithPermissions(t, &fakeRuntime{}, &captureWriter{}, func(req acpsdk.RequestPermissionRequest) any {
		assert.Equal(t, testTraceParent, req.Meta["traceparent"])
		option := "allow"
		if req.ToolCall.ToolCallId == "max_iterations" {
			option = "continue"
		}
		response := permissionSelected(option)
		response.Meta = map[string]any{"traceparent": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"}
		return response
	})
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "test", Arguments: "{}"}}
	_, err := f.agent.handleToolCallConfirmation(ctx, f.sess, runtime.ToolCallConfirmation(call, tools.Tool{Name: "test"}, "root", nil).(*runtime.ToolCallConfirmationEvent), "opaque")
	require.NoError(t, err)
	require.NoError(t, f.agent.handleMaxIterationsReached(ctx, f.sess, runtime.MaxIterationsReached(2).(*runtime.MaxIterationsReachedEvent)))
	require.Len(t, f.peer.recordedRequests(), 2)
	require.NoError(t, f.agent.sendUpdate(ctx, f.sess.id, acpsdk.UpdateAgentMessageText("after hostile reply")))
	for _, line := range f.out.lines() {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Meta map[string]any `json:"_meta"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &msg))
		if msg.Method == "session/update" {
			assert.Equal(t, testTraceParent, msg.Params.Meta["traceparent"])
		}
	}
}

func TestACPTraceEveryAgentBoundary(t *testing.T) {
	recorder := installACPTraceRecorder(t)
	a := clientMCPAgent(t)
	a.team = nil
	a.agentSource = config.NewBytesSource("test", nil)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	meta := map[string]any{"traceparent": testTraceParent}
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{Meta: meta})
	require.NoError(t, err)
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod, Meta: meta})
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), Meta: meta})
	require.NoError(t, err)
	_, err = a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Meta: meta})
	require.NoError(t, err)
	_, _ = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: "missing", Meta: meta})
	_, _ = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: "missing", Meta: meta})
	_, _ = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: "missing", Meta: meta})
	_, _ = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "bad", Meta: meta})
	_, _ = a.SetSessionConfigOption(t.Context(), acpsdk.SetSessionConfigOptionRequest{Boolean: &acpsdk.SetSessionConfigOptionBoolean{SessionId: created.SessionId, ConfigId: "mode", Meta: meta}})
	require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: created.SessionId, Meta: meta}))
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId, Meta: meta})
	require.NoError(t, err)
	_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: created.SessionId, Meta: meta})
	require.NoError(t, err)
	_, err = a.Logout(t.Context(), acpsdk.LogoutRequest{Meta: meta})
	require.NoError(t, err)
	var methods []string
	for _, s := range recorder.Ended() {
		if !strings.HasPrefix(s.Name(), "acp.") {
			continue
		}
		assert.Equal(t, "11111111111111111111111111111111", s.SpanContext().TraceID().String())
		assert.Equal(t, "2222222222222222", s.Parent().SpanID().String())
		methods = append(methods, strings.TrimPrefix(s.Name(), "acp."))
	}
	assert.ElementsMatch(t, []string{"initialize", "authenticate", "logout", "session/new", "session/list", "session/resume", "session/load", "session/prompt", "session/set_mode", "session/set_config_option", "session/cancel", "session/close", "session/delete"}, methods)
}

func TestACPTraceMCPSetupDoesNotAnnotateRequestSpan(t *testing.T) {
	recorder := installACPTraceRecorder(t)
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	spec := clientServerSpec(t, "private server label", "private-env-value")
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{spec}, Meta: map[string]any{"traceparent": testTraceParent}})
	require.NoError(t, err)
	spec.Stdio.Command = filepath.Join(t.TempDir(), "private-executable-path")
	_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{spec}, Meta: map[string]any{"traceparent": testTraceParent}})
	require.Error(t, err)
	var count int
	for _, span := range recorder.Ended() {
		if span.Name() != "acp.session/new" {
			continue
		}
		count++
		require.Len(t, span.Attributes(), 1)
		assert.Equal(t, "rpc.method", string(span.Attributes()[0].Key))
		assert.Equal(t, "session/new", span.Attributes()[0].Value.AsString())
		assert.Empty(t, span.Events())
	}
	assert.Equal(t, 2, count)
}

func TestACPTraceNativeCompactDoesNotAnnotateBoundary(t *testing.T) {
	recorder := installACPTraceRecorder(t)
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	p := &nativeCommandCompactor{mockProvider: mockProvider{id: modelsdev.NewID("test", "compact")}}
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		root := agent.New("root", "test", agent.WithModel(p), agent.WithTools(tools.Tool{Name: "__structured_output__"}), agent.WithStructuredOutput(&latest.StructuredOutput{Name: "result", Mode: "tool", Schema: map[string]any{"type": "object"}}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
	}
	captureReplay(t, a, &captureWriter{})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	a.sessions[string(created.SessionId)].sess.AddMessage(session.UserMessage("history"))
	_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("/compact")}, Meta: map[string]any{"traceparent": testTraceParent}})
	require.ErrorContains(t, err, "__structured_output__")
	var found bool
	for _, span := range recorder.Ended() {
		if span.Name() != "acp.session/prompt" {
			continue
		}
		found = true
		assert.Equal(t, "11111111111111111111111111111111", span.SpanContext().TraceID().String())
		require.Len(t, span.Attributes(), 1)
		assert.Empty(t, span.Events())
	}
	assert.True(t, found)
}
