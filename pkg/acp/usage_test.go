package acp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

func usageEvent(sid, agentName string, used, limit int64, cost float64) *runtime.TokenUsageEvent {
	return runtime.NewTokenUsageEvent(sid, agentName, &runtime.Usage{ContextLength: used, ContextLimit: limit, Cost: cost}).(*runtime.TokenUsageEvent)
}

func usageUpdates(t *testing.T, out *captureWriter) []*acpsdk.SessionUsageUpdate {
	t.Helper()
	var result []*acpsdk.SessionUsageUpdate
	for _, line := range out.lines() {
		var msg struct {
			Params acpsdk.SessionNotification `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &msg))
		if msg.Params.Update.UsageUpdate != nil {
			result = append(result, msg.Params.Update.UsageUpdate)
		}
	}
	return result
}

func TestUsageAggregatesSessionsWithoutReplacingRootContext(t *testing.T) {
	t.Parallel()
	events := []runtime.Event{
		usageEvent("child-first", "worker", 800, 900, 0.1),
		usageEvent(testSessionID, "root", 100, 1000, 0.2),
		usageEvent("child-first", "worker", 810, 900, 0.15),
		usageEvent("child-first", "worker", 810, 900, 0.15),
		usageEvent("child-two", "worker", 30, 50, 0.3),
		usageEvent(testSessionID, "handoff", 200, 2000, 0.4),
		usageEvent("", "indexer", 999, 999, 100),
		runtime.NewTokenUsageEvent("child", "worker", nil),
	}
	f := newRunAgentFixture(t, &fakeRuntime{events: events}, &captureWriter{})
	require.NoError(t, f.runAgent(t.Context(), f.sess))
	updates := usageUpdates(t, f.out)
	require.Len(t, updates, 5)
	for i, want := range []float64{0.3, 0.35, 0.35, 0.65, 0.85} {
		require.NotNil(t, updates[i].Cost)
		assert.InDelta(t, want, updates[i].Cost.Amount, 1e-9)
		if i < 4 {
			assert.Equal(t, 100, updates[i].Used)
			assert.Equal(t, 1000, updates[i].Size)
		}
	}
	assert.Equal(t, 200, updates[4].Used)
	assert.Equal(t, 2000, updates[4].Size)
	assert.Equal(t, int64(800), events[0].(*runtime.TokenUsageEvent).Usage.ContextLength)
	assert.InDelta(t, 0.2, events[1].(*runtime.TokenUsageEvent).Usage.Cost, 1e-9, "shared runtime events must not be mutated")
	f.rt.events = []runtime.Event{usageEvent(testSessionID, "handoff", 250, 2000, 0.5)}
	require.NoError(t, f.runAgent(t.Context(), f.sess))
	all := usageUpdates(t, f.out)
	assert.InDelta(t, 0.95, all[len(all)-1].Cost.Amount, 1e-9, "child costs survive foreground turns")
}

func TestUsageUnknownAndInvalidSnapshots(t *testing.T) {
	t.Parallel()
	f := newRunAgentFixture(t, &fakeRuntime{events: []runtime.Event{
		usageEvent("child", "worker", 99, 100, 0.2),
		usageEvent(testSessionID, "root", 20, 0, 0.1),
		usageEvent("child", "worker", 100, 100, 0.3),
		usageEvent(testSessionID, "root", 20, 1000, math.NaN()),
		usageEvent("child", "worker", 0, 0, math.Inf(1)),
		usageEvent("child", "worker", 0, 0, -1),
	}}, &captureWriter{})
	require.NoError(t, f.runAgent(t.Context(), f.sess))
	updates := usageUpdates(t, f.out)
	require.Len(t, updates, 3)
	for _, update := range updates {
		assert.Equal(t, 20, update.Used)
		assert.Equal(t, 1000, update.Size)
		assert.InDelta(t, 0.4, update.Cost.Amount, 1e-9)
	}
	assert.Nil(t, f.sess.recordUsage(nil))
	assert.Nil(t, f.sess.recordUsage(usageEvent(testSessionID, "root", -1, 1000, 0.1)))
	s := &Session{sess: session.New()}
	s.recordUsage(usageEvent(s.sess.ID, "root", 1, 2, math.MaxFloat64))
	got := s.recordUsage(usageEvent("child", "worker", 1, 2, math.MaxFloat64))
	assert.InDelta(t, math.MaxFloat64, got.Cost, 0)
}

func addUsageCost(s *session.Session, cost float64) {
	s.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Cost: cost}})
}

func TestUsageRestoredLiveChildrenAndCompaction(t *testing.T) {
	t.Parallel()
	root, old, child, nested := session.New(), session.New(), session.New(), session.New()
	addUsageCost(root, 0.1)
	addUsageCost(old, 0.2)
	root.AddSubSession(old)
	root.SetUsage(120, 30)
	s := &Session{sess: root}
	assert.InDelta(t, 0.3, s.currentUsage("root").Cost, 1e-9)
	addUsageCost(child, 0.05)
	addUsageCost(nested, 0.02)
	child.AddLiveSubSession(nested)
	root.AddLiveSubSession(child)
	s.recordUsage(usageEvent(nested.ID, "worker", 999, 9999, 0.02))
	s.recordUsage(usageEvent(child.ID, "worker", 555, 5555, 0.05))
	s.recordUsage(runtime.NewTokenUsageEvent(root.ID, "root", runtime.SessionUsage(root, 1000)).(*runtime.TokenUsageEvent))
	for range 2 {
		root.ApplyCompaction(10, 2, session.Item{Summary: "root summary", Cost: 0.01})
		child.ApplyCompaction(5, 1, session.Item{Summary: "child summary", Cost: 0.01})
		s.recordUsage(runtime.NewTokenUsageEvent(child.ID, "worker", runtime.SessionUsage(child, 500)).(*runtime.TokenUsageEvent))
		got := s.recordUsage(runtime.NewTokenUsageEvent(root.ID, "root", runtime.SessionUsage(root, 1000)).(*runtime.TokenUsageEvent))
		assert.InDelta(t, root.TotalCost(), got.Cost, 1e-9)
		assert.Equal(t, int64(12), got.ContextLength)
		assert.InDelta(t, root.TotalCost(), s.currentUsage("root").Cost, 1e-9)
	}
	// A cold restore embeds all descendants; seeding each again would double count.
	loaded := session.New()
	addUsageCost(loaded, root.OwnCost())
	loaded.AddSubSession(old)
	loaded.AddSubSession(child)
	fresh := &Session{sess: loaded}
	assert.InDelta(t, root.TotalCost(), fresh.currentUsage("root").Cost, 1e-9)
	assert.Len(t, fresh.usageCosts, 1)
}

func TestUsageCommandUsesAggregateForTextAndUpdate(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
	a, s, peer := newPromptTestAgent(t, rt)
	addUsageCost(s.sess, 0.1)
	s.sess.SetUsage(12, 3)
	s.recordUsage(usageEvent(s.sess.ID, "root", 15, 1000, 0.1))
	s.recordUsage(usageEvent("child", "worker", 900, 999, 0.2))
	history := len(s.sess.OwnMessages())
	_, err := a.Prompt(t.Context(), promptRequest("/usage"))
	require.NoError(t, err)
	out := peer.out.(*captureWriter)
	updates := usageUpdates(t, out)
	require.Len(t, updates, 1)
	assert.InDelta(t, 0.3, updates[0].Cost.Amount, 1e-9)
	assert.Equal(t, 15, updates[0].Used)
	assert.Contains(t, strings.Join(out.lines(), "\n"), "$0.300000 USD")
	assert.Len(t, s.sess.OwnMessages(), history)
	assert.Zero(t, rt.runs)
	rt.current = "other"
	_, err = a.Prompt(t.Context(), promptRequest("/usage"))
	require.NoError(t, err)
	assert.Len(t, usageUpdates(t, out), 1, "unknown window should not emit a made-up zero-size gauge")
	assert.Contains(t, strings.Join(out.lines(), "\n"), "context limit unknown")
}

func TestUsageAccountingSurvivesCancellationAndSendFailure(t *testing.T) {
	t.Parallel()
	for _, cancelTurn := range []bool{false, true} {
		rt := &fakeRuntime{events: []runtime.Event{usageEvent(testSessionID, "root", 12, 1000, 0.1), usageEvent("child", "worker", 999, 9999, 0.2)}}
		out := &captureWriter{}
		f := newRunAgentFixture(t, rt, out)
		ctx, cancel := context.WithCancel(t.Context())
		if cancelTurn {
			rt.onRunStream = cancel
		} else {
			out.failOn = func(n int) error {
				if n == 2 {
					return errors.New("send failed")
				}
				return nil
			}
		}
		err := f.runAgent(ctx, f.sess)
		cancel()
		require.Error(t, err)
		if cancelTurn {
			require.ErrorIs(t, err, context.Canceled)
		} else {
			assert.Contains(t, err.Error(), "send failed")
		}
		func() {
			f.sess.mu.Lock()
			defer f.sess.mu.Unlock()
			assert.InDelta(t, 0.3, f.sess.totalUsageCost(), 1e-9)
			assert.Equal(t, int64(12), f.sess.rootUsage.ContextLength)
		}()
	}
}

type usageModelStore struct{ runtime.ModelStore }

func (usageModelStore) GetModel(_ context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	limit := 1000
	if id.String() == "test/worker" {
		limit = 9000
	}
	return &modelsdev.Model{Limit: modelsdev.Limit{Context: limit}, Cost: &modelsdev.Cost{Input: 10, Output: 20}}, nil
}

func usageStream(call *tools.ToolCall, input, output int64) *mockStream {
	delta := chat.MessageDelta{Content: "done"}
	finish := chat.FinishReasonStop
	if call != nil {
		delta.ToolCalls = []tools.ToolCall{*call}
		finish = chat.FinishReasonToolCalls
	}
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: delta}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: finish}}, Usage: &chat.Usage{InputTokens: input, OutputTokens: output}},
	}}
}

func TestUsageRealRuntimeDelegation(t *testing.T) {
	t.Parallel()
	worker := agent.New("worker", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "worker"), stream: usageStream(nil, 600, 30)}))
	call := &tools.ToolCall{ID: "transfer", Type: "function", Function: tools.FunctionCall{Name: "transfer_task", Arguments: `{"agent":"worker","task":"work"}`}}
	root := agent.New("root", "test", agent.WithSubAgents(worker), agent.WithToolSets(transfertask.New()), agent.WithModel(&outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "root")}, streams: []chat.MessageStream{usageStream(call, 100, 10), usageStream(nil, 200, 20)}}))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root, worker)), runtime.WithSessionCompaction(false), runtime.WithModelStore(usageModelStore{}))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a, s, peer := newPromptTestAgent(t, rt)
	s.sess.SetToolsApproved(true)
	resp, err := a.Prompt(t.Context(), promptRequest("delegate"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, resp.StopReason)
	updates := usageUpdates(t, peer.out.(*captureWriter))
	require.GreaterOrEqual(t, len(updates), 3)
	var previous float64
	for _, update := range updates {
		assert.Equal(t, 1000, update.Size)
		assert.NotEqual(t, 630, update.Used)
		require.NotNil(t, update.Cost)
		assert.GreaterOrEqual(t, update.Cost.Amount, previous)
		previous = update.Cost.Amount
	}
	assert.InDelta(t, s.sess.TotalCost(), previous, 1e-9)
	assert.Greater(t, previous, s.sess.OwnCost())
}

func TestUsageBackgroundRuntimeRecordsWithoutClientIO(t *testing.T) {
	t.Parallel()
	a := clientMCPAgent(t)
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a.sessionStore = store
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		worker := agent.New("worker", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("openai", "gpt-4o"), stream: usageStream(nil, 100, 50)}))
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("openai", "gpt-4o")}), agent.WithSubAgents(worker))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root, worker))}, nil
	}
	f := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
	a.SetAgentConnection(f.agent.conn)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	before := len(f.out.lines())
	runner, ok := s.rt.(agenttool.Runner)
	require.True(t, ok)
	result := runner.RunAgent(t.Context(), agenttool.RunParams{AgentName: "worker", Task: "background", ParentSession: s.sess})
	require.Empty(t, result.ErrMsg)
	assert.Len(t, f.out.lines(), before, "background callback must not write to ACP")
	s.recordUsage(usageEvent(s.sess.ID, "root", 10, 1000, 0))
	got := s.currentUsage("root")
	assert.Positive(t, got.Cost)
	assert.InDelta(t, s.sess.TotalCost(), got.Cost, 1e-9)
	_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("/usage")}})
	require.NoError(t, err)
	updates := usageUpdates(t, f.out)
	require.Len(t, updates, 1)
	assert.InDelta(t, got.Cost, updates[0].Cost.Amount, 1e-9)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: s.workingDir})
	require.NoError(t, err)
	assert.InDelta(t, got.Cost, s.currentUsage("root").Cost, 1e-9, "active resume preserves live accounting")
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: s.workingDir})
	require.NoError(t, err)
	resumed := a.sessions[string(created.SessionId)]
	require.NotSame(t, s, resumed)
	assert.InDelta(t, got.Cost, resumed.currentUsage("root").Cost, 1e-9, "cold resume embeds previously live costs exactly once")
	assert.Len(t, resumed.usageCosts, 1)
}

func TestUsageConcurrentSnapshots(t *testing.T) {
	t.Parallel()
	s := &Session{sess: session.New()}
	s.recordUsage(usageEvent(s.sess.ID, "root", 10, 1000, 0))
	var wg sync.WaitGroup
	for _, sid := range []string{"a", "b", "c"} {
		wg.Go(func() {
			for i := range 100 {
				s.recordUsage(usageEvent(sid, "worker", int64(i), 5000, float64(i)))
			}
		})
	}
	wg.Go(func() {
		for range 100 {
			s.currentUsage("root")
		}
	})
	wg.Wait()
	assert.InDelta(t, 297, s.currentUsage("root").Cost, 0)
}

func TestUsageCompactKeepsAccountingAfterError(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
	a, s, _ := newPromptTestAgent(t, rt)
	s.recordUsage(usageEvent(s.sess.ID, "root", 100, 1000, 0.1))
	rt.summary = func(_ context.Context, _ *session.Session, _ string, sink runtime.EventSink) {
		sink.Emit(runtime.ErrorForSession(s.sess.ID, "compaction failed"))
		sink.Emit(usageEvent("child", "worker", 900, 9999, 0.2))
		sink.Emit(usageEvent(s.sess.ID, "root", 20, 1000, 0.15))
	}
	_, err := a.Prompt(t.Context(), promptRequest("/compact"))
	require.ErrorContains(t, err, "compaction failed")
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.InDelta(t, 0.35, s.totalUsageCost(), 1e-9)
	assert.Equal(t, int64(20), s.rootUsage.ContextLength)
}

func TestUsageInvalidRootHistoryKeepsValidContributions(t *testing.T) {
	t.Parallel()
	for _, invalid := range []float64{math.NaN(), -1} {
		s := &Session{sess: session.New()}
		addUsageCost(s.sess, invalid)
		s.recordUsage(usageEvent("child", "worker", 1, 100, 0.2))
		assert.InDelta(t, 0.2, s.currentUsage("root").Cost, 1e-9)

		valid := &Session{sess: session.New()}
		addUsageCost(valid.sess, 0.1)
		valid.recordUsage(usageEvent(valid.sess.ID, "root", 10, 1000, 0.1))
		valid.recordUsage(usageEvent("child", "worker", 1, 100, 0.2))
		addUsageCost(valid.sess, invalid)
		assert.InDelta(t, 0.3, valid.currentUsage("root").Cost, 1e-9)
	}
}

func TestUsageEvaluatorSnapshotReplacesRatherThanAdds(t *testing.T) {
	t.Parallel()
	s := &Session{sess: session.New()}
	addUsageCost(s.sess, 0.1)
	s.recordUsage(usageEvent(s.sess.ID, "root", 100, 1000, 0.1))
	s.recordUsage(usageEvent("child", "worker", 500, 5000, 0.2))
	s.sess.AddEvaluation(&session.Evaluation{Cost: new(0.03)})
	usage := runtime.SessionUsage(s.sess, 1000)
	usage.SnapshotOnly = true
	event := runtime.NewTokenUsageEvent(s.sess.ID, "root", usage).(*runtime.TokenUsageEvent)
	for range 2 {
		assert.InDelta(t, 0.33, s.recordUsage(event).Cost, 1e-9)
	}
}
