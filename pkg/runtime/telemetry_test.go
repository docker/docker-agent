package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
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
		sessionStarts: append([]sessionStart(nil), r.sessionStarts...),
		sessionEnds:   r.sessionEnds,
		errors:        append([]string(nil), r.errors...),
		toolCalls:     append([]toolCallRecord(nil), r.toolCalls...),
		tokenUsages:   append([]tokenUsageRecord(nil), r.tokenUsages...),
	}
}

func TestWithTelemetry_AppliedToRuntime(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rec := &recordingTelemetry{}
	rt, err := NewLocalRuntime(tm,
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

	rt, err := NewLocalRuntime(tm,
		WithTelemetry(nil),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	_, ok := rt.telemetry.(defaultTelemetry)
	assert.True(t, ok, "WithTelemetry(nil) should leave defaultTelemetry, got %T", rt.telemetry)
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

	rt, err := NewLocalRuntime(tm,
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
