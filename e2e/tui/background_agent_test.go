package tui_test

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

// This file covers the background-agent journey end to end through the real
// TUI: a root agent dispatches a run_background_agent task, the worker's
// sub-session runs on a detached goroutine (its events are dropped from the
// live stream), and only the runtime's out-of-band forwarding can bring its
// token usage back to the sidebar. Unlike the VCR-based scenarios in this
// package, the model responses are scripted in-process: driving a
// deterministic multi-agent tool-call flow through cassettes would couple the
// test to HTTP wire details it does not care about.

// scriptedStream replays a fixed sequence of streaming chunks.
type scriptedStream struct {
	responses []chat.MessageStreamResponse
	idx       int
}

func (s *scriptedStream) Recv() (chat.MessageStreamResponse, error) {
	if s.idx >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := s.responses[s.idx]
	s.idx++
	return resp, nil
}

func (s *scriptedStream) Close() {}

// scriptedProvider returns one scripted stream per model call, repeating the
// last script if called more often. contextWindow is exposed through
// provider_opts so the runtime resolves a context limit without the
// models.dev catalogue, which is what makes percentages computable.
type scriptedProvider struct {
	id            string
	contextWindow int64
	scripts       [][]chat.MessageStreamResponse

	mu   sync.Mutex
	call int
}

func (p *scriptedProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *scriptedProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := min(p.call, len(p.scripts)-1)
	p.call++
	return &scriptedStream{responses: p.scripts[i]}, nil
}

func (p *scriptedProvider) BaseConfig() base.Config {
	window := p.contextWindow
	return base.Config{ModelConfig: latest.ModelConfig{ContextWindow: &window}}
}

// toolCallScript scripts one turn that calls a single tool and stops with the
// given usage.
func toolCallScript(callID, toolName, args string, in, out int64) []chat.MessageStreamResponse {
	return []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID: callID, Type: "function", Function: tools.FunctionCall{Name: toolName},
		}}}}}},
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID: callID, Type: "function", Function: tools.FunctionCall{Arguments: args},
		}}}}}},
		{
			Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}},
			Usage:   &chat.Usage{InputTokens: in, OutputTokens: out},
		},
	}
}

// contentScript scripts one plain-content turn that stops with the given usage.
func contentScript(content string, in, out int64) []chat.MessageStreamResponse {
	return []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: content}}}},
		{
			Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}},
			Usage:   &chat.Usage{InputTokens: in, OutputTokens: out},
		},
	}
}

// stubModelStore keeps the runtime off the network: no models.dev lookups, no
// pricing. Context limits come from provider_opts instead.
type stubModelStore struct{}

func (stubModelStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	return nil, nil
}
func (stubModelStore) GetDatabase(context.Context) (*modelsdev.Database, error) { return nil, nil }

// newBackgroundAgentTUI builds the real TUI over a real LocalRuntime whose
// team is assembled programmatically: a root orchestrator with the
// background_agents toolset and a worker sub-agent, both backed by scripted
// providers. Tools are pre-approved so the dispatch needs no confirmation
// dialog.
func newBackgroundAgentTUI(t *testing.T, width, height int) *tuitest.Driver {
	t.Helper()

	isolateState(t)

	rootProv := &scriptedProvider{
		id:            "test/fake-root",
		contextWindow: 1000,
		scripts: [][]chat.MessageStreamResponse{
			toolCallScript("call-1", agenttool.ToolNameRunBackgroundAgent,
				`{"agent":"worker","task":"count the files"}`, 40, 10),
			// Session context tracks the LAST call's usage (its input already
			// spans the whole prompt), so the root settles at 100/1000 = 10%.
			contentScript("Dispatched the worker.", 80, 20),
		},
	}
	workerProv := &scriptedProvider{
		id:            "test/fake-worker",
		contextWindow: 1000,
		scripts: [][]chat.MessageStreamResponse{
			contentScript("worker finished the count", 300, 150),
		},
	}

	worker := agent.New("worker", "Count things when asked.",
		agent.WithModel(workerProv),
		agent.WithDescription("Background worker"),
	)
	root := agent.New("root", "Dispatch work to the worker in the background.",
		agent.WithModel(rootProv),
		agent.WithDescription("Orchestrator"),
		agent.WithSubAgents(worker),
		agent.WithToolSets(agenttool.New()),
	)

	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root, worker)),
		runtime.WithCurrentAgent("root"),
		runtime.WithSessionCompaction(false),
		runtime.WithModelStore(stubModelStore{}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	application := app.New(t.Context(), rt, session.New(session.WithToolsApproved(true)))

	wd, _ := os.Getwd()
	model := tui.New(t.Context(), nil /* no spawner: single tab */, application, wd, func() {})
	return tuitest.New(t, model, width, height)
}

// TestBackgroundAgent_PerAgentContextInSidebar drives the full journey: the
// worker's usage (450 of 1000 tokens) can only reach the TUI through the
// runtime's out-of-band background-event forwarding, so the 45% appearing on
// the worker's roster line proves the whole chain — tool call, detached
// sub-session, OnBackgroundEvent, app event stream, sidebar accounting.
// The root's own line independently settles at 10% (its final turn consumed
// 100 of 1000 tokens).
func TestBackgroundAgent_PerAgentContextInSidebar(t *testing.T) {
	d := newBackgroundAgentTUI(t, 120, 40)

	// Both agents are listed before anything runs, with no context percent.
	d.WaitFor(tuitest.ContainsAll("root", "worker")).
		Assert(tuitest.Absent("%"))

	d.Type("Please dispatch the worker.").
		Enter().
		WaitFor(tuitest.Contains("Dispatched the worker.")).
		WaitFor(tuitest.Contains("45%")).
		WaitFor(tuitest.Contains("10%"))
}

// TestBackgroundAgent_InspectorShowsWorkerContext opens the worker's Agent
// Inspector (right-click on its roster line) after the background task
// completed and checks the exact per-agent context line.
func TestBackgroundAgent_InspectorShowsWorkerContext(t *testing.T) {
	d := newBackgroundAgentTUI(t, 120, 40)

	d.Type("Please dispatch the worker.").
		Enter().
		WaitFor(tuitest.Contains("Dispatched the worker.")).
		WaitFor(tuitest.Contains("45%"))

	// "45%" renders only on the worker's roster line, so its coordinates are
	// a reliable right-click target for that agent (either of the entry's two
	// lines opens the inspector).
	x, y := d.MustFindText("45%")
	d.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight}).
		Send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseRight}).
		WaitFor(tuitest.Contains("Context: 450 of 1.0K tokens (45%)"))
}
