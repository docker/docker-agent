package runtime

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// recordingTelemetry captures every call made to it so tests can assert that
// the runtime emitted the expected lifecycle events. It is intentionally
// thread-safe because RunStream invokes telemetry from a goroutine.
type recordingTelemetry struct {
	mu            sync.Mutex
	sessionStarts []sessionStart
	sessionEnds   int
	errors        []string
	toolCalls     []toolCallRecord
	tokenUsages   []tokenUsageRecord
}

type sessionStart struct {
	AgentName string
	SessionID string
}

type toolCallRecord struct {
	ToolName  string
	SessionID string
	AgentName string
	Duration  time.Duration
	Err       error
}

type tokenUsageRecord struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	Cost         float64
}

func (r *recordingTelemetry) RecordSessionStart(_ context.Context, agentName, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionStarts = append(r.sessionStarts, sessionStart{AgentName: agentName, SessionID: sessionID})
}

func (r *recordingTelemetry) RecordSessionEnd(_ context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionEnds++
}

func (r *recordingTelemetry) RecordError(_ context.Context, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, msg)
}

func (r *recordingTelemetry) RecordToolCall(_ context.Context, toolName, sessionID, agentName string, duration time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCalls = append(r.toolCalls, toolCallRecord{
		ToolName:  toolName,
		SessionID: sessionID,
		AgentName: agentName,
		Duration:  duration,
		Err:       err,
	})
}

func (r *recordingTelemetry) RecordTokenUsage(_ context.Context, model string, in, out int64, cost float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokenUsages = append(r.tokenUsages, tokenUsageRecord{
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		Cost:         cost,
	})
}

func (r *recordingTelemetry) snapshot() recordingTelemetry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recordingTelemetry{
		sessionStarts: slices.Clone(r.sessionStarts),
		sessionEnds:   r.sessionEnds,
		errors:        slices.Clone(r.errors),
		toolCalls:     slices.Clone(r.toolCalls),
		tokenUsages:   slices.Clone(r.tokenUsages),
	}
}

func TestWithTelemetry_AppliedToRuntime(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rec := &recordingTelemetry{}
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithTelemetry(rec),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	assert.Same(t, rec, rt.telemetry, "WithTelemetry not wired into runtime")
}

func TestWithTelemetry_NilLeavesDefault(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithTelemetry(nil),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	_, ok := rt.telemetry.(defaultTelemetry)
	assert.True(t, ok, "WithTelemetry(nil) should leave defaultTelemetry, got %T", rt.telemetry)
}

func TestRuntime_RecordsPerCallTelemetryCost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		finish   chat.FinishReason
		noUsage  bool
		unpriced bool
		free     bool
		empty    bool
	}{
		{name: "stop", finish: chat.FinishReasonStop},
		{name: "bare EOF"},
		{name: "reasoning only", finish: chat.FinishReasonLength, empty: true},
		{name: "unpriced", finish: chat.FinishReasonStop, unpriced: true},
		{name: "free", finish: chat.FinishReasonStop, free: true},
		{name: "no usage", finish: chat.FinishReasonStop, noUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rates := &modelsdev.Cost{Input: 2, Output: 4, CacheRead: 0.2, CacheWrite: 2.5}
			wantCost := 0.00107 // 100 fresh + 200 cache-read + 300 cache-write + 20 output.
			if tc.unpriced {
				rates = nil
			}
			if tc.free {
				rates = &modelsdev.Cost{}
			}
			if tc.unpriced || tc.free || tc.noUsage {
				wantCost = 0
			}
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"test": {Models: map[string]modelsdev.Model{"model": {Name: "Test Model", Cost: rates}}},
			}})
			prov := &mockProvider{id: "test/model"}
			root := agent.New("root", "test", agent.WithModel(prov))
			rec := &recordingTelemetry{}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithTelemetry(rec), WithSessionCompaction(false), WithModelStore(store))
			require.NoError(t, err)

			sess := session.New(session.WithTitle("Telemetry test"), session.WithMessages([]session.Item{
				session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{
					Role: chat.MessageRoleAssistant, Content: "Earlier reply", Cost: 7,
				}}),
			}))
			var want []tokenUsageRecord
			for range 4 {
				builder := newStreamBuilder()
				if tc.empty {
					builder.AddReasoning("Thinking")
				} else {
					builder.AddContent("hello")
				}
				response := chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: tc.finish}}}
				if !tc.noUsage {
					response.Usage = &chat.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 200, CacheWriteTokens: 300, ReasoningTokens: 10}
					want = append(want, tokenUsageRecord{Model: "Test Model", InputTokens: 600, OutputTokens: 20, Cost: wantCost})
				}
				builder.responses = append(builder.responses, response)
				prov.stream = builder.Build()
				sess.AddMessage(session.UserMessage("hi"))
				for ev := range rt.RunStream(t.Context(), sess) {
					if ev, ok := ev.(*ErrorEvent); ok {
						t.Errorf("unexpected runtime error: %s", ev.Error)
					}
				}
				assert.Equal(t, want, rec.snapshot().tokenUsages)
			}
		})
	}
}

func TestRuntime_RecordsSessionStartAndEnd(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("hello").
		AddStopWithUsage(10, 20).
		Build()

	rec := &recordingTelemetry{}
	prov := &mockProvider{id: "test/mock-model", stream: stream}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithTelemetry(rec),
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	got := rec.snapshot()

	require.Len(t, got.sessionStarts, 1, "expected one session_start record")
	assert.Equal(t, "root", got.sessionStarts[0].AgentName)
	assert.Equal(t, sess.ID, got.sessionStarts[0].SessionID)

	assert.Equal(t, 1, got.sessionEnds, "expected one session_end record")

	// The stream reported usage so token usage must have been recorded.
	require.NotEmpty(t, got.tokenUsages, "expected at least one token-usage record")
	assert.Equal(t, int64(10), got.tokenUsages[0].InputTokens)
	assert.Equal(t, int64(20), got.tokenUsages[0].OutputTokens)
}
