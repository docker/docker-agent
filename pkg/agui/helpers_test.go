package agui

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

const testWorkspaceRoot = "/srv/agui-workspace"

// mockStream replays a fixed sequence of completion chunks.
type mockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
}

func (m *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if m.idx >= len(m.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := m.responses[m.idx]
	m.idx++
	return resp, nil
}

func (m *mockStream) Close() {}

// scriptedProvider plays one scripted stream per model call and records the
// messages each call received.
type scriptedProvider struct {
	mu      sync.Mutex
	streams [][]chat.MessageStreamResponse
	err     error
	calls   [][]chat.Message
}

func script(streams ...[]chat.MessageStreamResponse) *scriptedProvider {
	return &scriptedProvider{streams: streams}
}

func (p *scriptedProvider) ID() modelsdev.ID { return modelsdev.NewID("test", "mock-model") }

func (p *scriptedProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, messages)
	if p.err != nil {
		return nil, p.err
	}
	if len(p.streams) == 0 {
		return &mockStream{responses: stop()}, nil
	}
	next := p.streams[0]
	p.streams = p.streams[1:]
	return &mockStream{responses: next}, nil
}

func (p *scriptedProvider) BaseConfig() base.Config { return base.Config{} }
func (p *scriptedProvider) MaxTokens() int          { return 0 }

func (p *scriptedProvider) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// conversation returns the user and assistant messages of the n-th model call.
func (p *scriptedProvider) conversation(n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var out []string
	for _, m := range p.calls[n] {
		if m.Role == chat.MessageRoleUser || m.Role == chat.MessageRoleAssistant {
			out = append(out, string(m.Role)+": "+m.Content)
		}
	}
	return out
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func delta(d chat.MessageDelta) chat.MessageStreamResponse {
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: d}}}
}

func stop() []chat.MessageStreamResponse {
	return []chat.MessageStreamResponse{{
		Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}},
		Usage:   &chat.Usage{InputTokens: 3, OutputTokens: 7},
	}}
}

// text streams the chunks as assistant content, then stops.
func text(chunks ...string) []chat.MessageStreamResponse {
	var out []chat.MessageStreamResponse
	for _, chunk := range chunks {
		out = append(out, delta(chat.MessageDelta{Content: chunk}))
	}
	return append(out, stop()...)
}

// toolCall streams the tool call "call-1", whose arguments arrive in several
// deltas.
func toolCall(name string, argChunks ...string) []chat.MessageStreamResponse {
	const id = "call-1"
	out := []chat.MessageStreamResponse{delta(chat.MessageDelta{ToolCalls: []tools.ToolCall{{
		ID: id, Type: "function", Function: tools.FunctionCall{Name: name},
	}}})}
	for _, chunk := range argChunks {
		out = append(out, delta(chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID: id, Function: tools.FunctionCall{Arguments: chunk},
		}}}))
	}
	return append(out, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonToolCalls}},
	})
}

func singleAgentTeam(prov *scriptedProvider, opts ...dagent.Opt) *team.Team {
	root := dagent.New("root", "You are a test agent", append([]dagent.Opt{dagent.WithModel(prov)}, opts...)...)
	return team.New(team.WithAgents(root))
}

var autonomous = servesafety.Resolved{Policy: session.SafetyPolicyAutonomous}

func listen(t *testing.T) net.Listener {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}

// sqliteStore opens the store production uses, on a database of its own.
func sqliteStore(t *testing.T) session.Store {
	t.Helper()

	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type testServer struct {
	url   string
	srv   *server
	store session.Store
}

// startServer serves tm over AG-UI on a loopback port until the test ends.
func startServer(t *testing.T, tm *team.Team, store session.Store, options RunOptions) *testServer {
	t.Helper()

	srv := newServer(tm, "root", store, autonomous, testWorkspaceRoot)
	handler, err := srv.handler(options)
	require.NoError(t, err)

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}, ln) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return &testServer{url: "http://" + ln.Addr().String() + Path, srv: srv, store: store}
}

// userInput builds a RunAgentInput body, as an AG-UI client sends it, whose
// history is the given user messages ("id:text").
func userInput(threadID, runID string, messages ...string) string {
	history := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		id, content, _ := strings.Cut(m, ":")
		history = append(history, map[string]any{"id": id, "role": "user", "content": content})
	}
	body, err := json.Marshal(map[string]any{
		"threadId":       threadID,
		"runId":          runID,
		"state":          map[string]any{},
		"messages":       history,
		"tools":          []any{},
		"context":        []any{},
		"forwardedProps": map[string]any{},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// transcript returns the user and assistant messages stored for thread-1.
func (s *testServer) transcript(t *testing.T) []string {
	t.Helper()

	sess, err := s.store.GetSession(t.Context(), "thread-1")
	require.NoError(t, err)
	var out []string
	for _, m := range sess.GetAllMessages() {
		if m.Message.Role == chat.MessageRoleUser || m.Message.Role == chat.MessageRoleAssistant {
			out = append(out, string(m.Message.Role)+": "+m.Message.Content)
		}
	}
	return out
}

type wireEvent map[string]any

func (e wireEvent) kind() string { return fmt.Sprint(e["type"]) }

// reply is a fully read HTTP response.
type reply struct {
	status int
	header http.Header
	// events are the decoded SSE events of a 200 response.
	events []wireEvent
	// body is the raw body of any other response.
	body string
}

// request describes one HTTP exchange with the server.
type request struct {
	method  string
	body    string
	headers map[string]string
	// onEvent, when set, is called for each event as it arrives.
	onEvent func(wireEvent)
}

// exchange sends req and reads the whole response. It makes no assertion, so
// it may run outside the test goroutine.
func (s *testServer) exchange(ctx context.Context, req request) (*reply, error) {
	httpReq, err := http.NewRequestWithContext(ctx, cmp.Or(req.method, http.MethodPost), s.url, strings.NewReader(req.body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for key, value := range req.headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &reply{status: resp.StatusCode, header: resp.Header}
	if resp.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(resp.Body)
		out.body = string(raw)
		return out, err
	}
	out.events, err = readEvents(resp.Body, req.onEvent)
	return out, err
}

// post sends a raw body and returns the HTTP status with the decoded SSE
// events (or the error body when the status is not 200).
func (s *testServer) post(t *testing.T, body string) (int, []wireEvent, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got, err := s.exchange(ctx, request{body: body})
	require.NoError(t, err)
	if got.status == http.StatusOK {
		require.Equal(t, "text/event-stream", got.header.Get("Content-Type"))
	}
	return got.status, got.events, got.body
}

// run posts a valid input and returns the stream without CUSTOM events, whose
// position depends on runtime timing.
func (s *testServer) run(t *testing.T, body string) []string {
	t.Helper()

	status, events, errBody := s.post(t, body)
	require.Equal(t, http.StatusOK, status, errBody)
	return render(events)
}

// readEvents decodes SSE frames until the stream ends. onEvent, when set, is
// called for each event as it arrives.
func readEvents(r io.Reader, onEvent func(wireEvent)) ([]wireEvent, error) {
	var events []wireEvent
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		data, ok := bytes.CutPrefix(scanner.Bytes(), []byte("data: "))
		if !ok {
			if line := scanner.Text(); line != "" && !strings.HasPrefix(line, "id: ") {
				return events, fmt.Errorf("unexpected SSE line %q", line)
			}
			continue
		}
		var event wireEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return events, err
		}
		events = append(events, event)
		if onEvent != nil {
			onEvent(event)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return events, err
	}
	return events, nil
}

// render turns wire events into comparable lines such as
// "TEXT_MESSAGE_CONTENT delta=Hi messageId=m1". CUSTOM events and timestamps
// are dropped, and runtime-generated message IDs are replaced by m1, m2, ...
// in order of appearance.
func render(events []wireEvent) []string {
	aliases := map[string]string{}
	alias := func(id string) string {
		if strings.HasSuffix(id, "-result") {
			return id
		}
		suffix := ""
		if base, ok := strings.CutSuffix(id, "-reasoning"); ok {
			id, suffix = base, "-reasoning"
		}
		if _, ok := aliases[id]; !ok {
			aliases[id] = fmt.Sprintf("m%d", len(aliases)+1)
		}
		return aliases[id] + suffix
	}

	var out []string
	for _, event := range events {
		if event.kind() == "CUSTOM" {
			continue
		}
		keys := make([]string, 0, len(event))
		for key := range event {
			if key != "type" && key != "timestamp" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		var line strings.Builder
		line.WriteString(event.kind())
		for _, key := range keys {
			value := fmt.Sprint(event[key])
			if key == "messageId" {
				value = alias(value)
			}
			line.WriteString(" " + key + "=" + value)
		}
		out = append(out, line.String())
	}
	return out
}
