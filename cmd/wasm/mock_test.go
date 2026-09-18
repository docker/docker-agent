//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

// scriptedModel replays one prepared stream per model call, in order, and
// records the messages it was called with.
type scriptedModel struct {
	id string

	mu    sync.Mutex
	turns []func(ctx context.Context) (chat.MessageStream, error)
	calls [][]chat.Message
}

func newScriptedModel(id string, turns ...func(ctx context.Context) (chat.MessageStream, error)) *scriptedModel {
	return &scriptedModel{id: id, turns: turns}
}

func (m *scriptedModel) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero(m.id) }
func (m *scriptedModel) BaseConfig() base.Config { return base.Config{} }

func (m *scriptedModel) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	m.mu.Lock()
	m.calls = append(m.calls, messages)
	if len(m.turns) == 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: no scripted turn left", m.id)
	}
	turn := m.turns[0]
	m.turns = m.turns[1:]
	m.mu.Unlock()
	return turn(ctx)
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *scriptedModel) lastCall() []chat.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[len(m.calls)-1]
}

type replayStream struct {
	responses []chat.MessageStreamResponse
}

func (s *replayStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.responses) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	return next, nil
}

func (s *replayStream) Close() {}

func stream(responses ...chat.MessageStreamResponse) func(context.Context) (chat.MessageStream, error) {
	return func(context.Context) (chat.MessageStream, error) {
		return &replayStream{responses: responses}, nil
	}
}

func failing(err error) func(context.Context) (chat.MessageStream, error) {
	return func(context.Context) (chat.MessageStream, error) { return nil, err }
}

// blocking waits for ctx to be cancelled, like a model call that never
// answers, then fails with the context error.
func blocking(started chan<- struct{}) func(context.Context) (chat.MessageStream, error) {
	return blockingUntil(started, nil)
}

// blockingUntil is blocking, but once cancelled it also waits for release
// before returning, holding the run open past its abort.
func blockingUntil(started chan<- struct{}, release <-chan struct{}) func(context.Context) (chat.MessageStream, error) {
	return func(ctx context.Context) (chat.MessageStream, error) {
		close(started)
		<-ctx.Done()
		if release != nil {
			<-release
		}
		return nil, ctx.Err()
	}
}

func textTurn(text string, reasoning ...string) func(context.Context) (chat.MessageStream, error) {
	var responses []chat.MessageStreamResponse
	for _, r := range reasoning {
		responses = append(responses, choice(chat.MessageDelta{ReasoningContent: r}))
	}
	responses = append(responses,
		choice(chat.MessageDelta{Content: text}),
		stop(chat.FinishReasonStop, 10, 5),
	)
	return stream(responses...)
}

func toolTurn(name, args string) func(context.Context) (chat.MessageStream, error) {
	const id = "call-1"
	return stream(
		choice(chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: id, Type: "function", Function: tools.FunctionCall{Name: name}}}}),
		choice(chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: id, Type: "function", Function: tools.FunctionCall{Arguments: args}}}}),
		stop(chat.FinishReasonToolCalls, 10, 5),
	)
}

func choice(delta chat.MessageDelta) chat.MessageStreamResponse {
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: delta}}}
}

func stop(finish chat.FinishReason, input, output int64) chat.MessageStreamResponse {
	return chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{FinishReason: finish}},
		Usage:   &chat.Usage{InputTokens: input, OutputTokens: output},
	}
}

// keywordEmbedder is an offline embedding model: vectors count the words
// "vacation" and "expense" so related texts land close together.
type keywordEmbedder struct {
	calls atomic.Int64
}

func (e *keywordEmbedder) ID() modelsdev.ID        { return modelsdev.NewID("mock", "embed") }
func (e *keywordEmbedder) BaseConfig() base.Config { return base.Config{} }

func (e *keywordEmbedder) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("embedding model only")
}

func (e *keywordEmbedder) CreateEmbedding(_ context.Context, text string) (*base.EmbeddingResult, error) { //nolint:unparam // provider.EmbeddingProvider signature
	e.calls.Add(1)
	lower := strings.ToLower(text)
	return &base.EmbeddingResult{
		Embedding:   []float64{float64(strings.Count(lower, "vacation")), float64(strings.Count(lower, "expense")), 0.1},
		TotalTokens: 1,
	}, nil
}

// mockProviders serves `mock/<name>` model references from models.
func mockProviders(models map[string]provider.Provider) *provider.Registry {
	return provider.NewRegistry(map[string]provider.Factory{
		"mock": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			p, ok := models[cfg.Model]
			if !ok {
				return nil, fmt.Errorf("no scripted model %q", cfg.Model)
			}
			return p, nil
		},
	})
}

// echoToolSet is a `type: echo` toolset with one tool that returns its
// `text` argument. readOnly controls whether the runtime asks for approval.
type echoToolSet struct {
	readOnly bool
	mu       sync.Mutex
	calls    []string
}

func (e *echoToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{
		Name:        "echo",
		Description: "Echo text",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		Annotations: tools.ToolAnnotations{ReadOnlyHint: e.readOnly},
		Handler: func(ctx context.Context, call tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			var args struct {
				Text string `json:"text"`
			}
			if err := tools.UnmarshalToolArguments(ctx, call, &args); err != nil {
				return nil, err
			}
			e.mu.Lock()
			e.calls = append(e.calls, args.Text)
			e.mu.Unlock()
			return tools.ResultSuccess("echo: " + args.Text), nil
		},
	}}, nil
}

func (e *echoToolSet) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// testHost is a host serving mock models plus the browser toolsets and an
// echo toolset. Like the browser, it builds a registry per session.
func testHost(echo *echoToolSet, models map[string]provider.Provider) host {
	return host{
		providers: mockProviders(models),
		newToolsets: func(documents rag.Documents) teamloader.ToolsetRegistry {
			creators := browserToolsetCreators(documents)
			creators["echo"] = func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
				return echo, nil
			}
			return teamloader.NewToolsetRegistry(creators)
		},
	}
}

// collectingEmitter records projected events and answers tool
// confirmations and elicitations through the session with the configured
// decision, mirroring what a host would do from onEvent.
type collectingEmitter struct {
	session  *chatSession
	decision string

	mu     sync.Mutex
	events []map[string]any
}

func (c *collectingEmitter) emit(_ context.Context, event map[string]any) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	if c.session != nil && event["type"] == "tool_confirmation" {
		go func() { _ = c.session.confirm(c.decision, "", "no thanks") }()
	}
}

func (c *collectingEmitter) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		out = append(out, e["type"].(string))
	}
	return out
}

func (c *collectingEmitter) find(kind string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, e := range c.events {
		if e["type"] == kind {
			out = append(out, e)
		}
	}
	return out
}

func openTestSession(t *testing.T, h host, opts sessionOptions) *chatSession {
	t.Helper()
	s, err := h.openSession(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.close()) })
	return s
}

const plainAgentYAML = `
agents:
  root:
    model: mock/root
    instruction: Be helpful.
`

const echoAgentYAML = `
agents:
  root:
    model: mock/root
    instruction: Use the echo tool.
    toolsets:
      - type: echo
`
