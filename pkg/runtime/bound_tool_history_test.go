package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func oversizedHistory(t *testing.T) *session.Session {
	t.Helper()
	sess := session.New()
	sess.AddMessage(session.UserMessage("inspect the job"))
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role:            chat.MessageRoleAssistant,
		ToolCalls:       []tools.ToolCall{{ID: "old-call", Function: tools.FunctionCall{Name: "view_background_job", Arguments: `{"job_id":"old-job"}`}}},
		ToolDefinitions: []tools.Tool{{Name: "view_background_job", Category: "background_jobs"}},
	}})
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "old-call", Content: strings.Repeat("x", 10*1024*1024) + "diagnostic tail"}})
	sess.AddMessage(session.UserMessage("continue"))
	// Exercise the persisted representation, not only an in-memory tool object.
	data, err := json.Marshal(sess)
	require.NoError(t, err)
	loaded := session.New()
	require.NoError(t, json.Unmarshal(data, loaded))
	return loaded
}

func assertBoundedHistoricalResult(t *testing.T, messages []chat.Message) {
	t.Helper()
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleTool || msg.ToolCallID != "old-call" {
			continue
		}
		assert.LessOrEqual(t, len(msg.Content), 50*1024)
		assert.Contains(t, msg.Content, "diagnostic tail")
		return
	}
	t.Fatal("historical tool result was dropped")
}

func TestRunStreamRecoversOversizedHistoricalResult(t *testing.T) {
	t.Parallel()
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "fallback"}[fallback], func(t *testing.T) {
			t.Parallel()
			prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("recovered").AddStopWithUsage(1, 1).Build()}}
			a := agent.New("root", "instructions", agent.WithModel(prov))
			if fallback {
				a = agent.New("root", "instructions", agent.WithModel(&failingProvider{id: "test/primary", err: assert.AnError}), agent.WithFallbackModel(prov), agent.WithFallbackRetries(0))
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			sess := oversizedHistory(t)
			original := sess.Messages[2].Message.Message.Content
			for event := range rt.RunStream(t.Context(), sess) {
				if failure, ok := event.(*ErrorEvent); ok {
					t.Fatalf("unexpected runtime error: %s", failure.Error)
				}
			}
			require.NotEmpty(t, prov.got)
			for _, messages := range prov.got {
				assertBoundedHistoricalResult(t, messages)
			}
			assert.Equal(t, original, sess.Messages[2].Message.Message.Content)
			assert.Equal(t, "recovered", sess.GetLastAssistantMessageContent())
		})
	}
}

func TestNativeCompactionBoundsHistoricalResult(t *testing.T) {
	t.Parallel()
	prov := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
	rt := newNativeRuntime(t, agent.New("root", "instructions", agent.WithModel(prov)))
	sess := oversizedHistory(t)
	original := sess.Messages[2].Message.Message.Content
	result := runCompaction(t, rt, sess, "")
	assert.Empty(t, result.errors)
	assert.Equal(t, CompactionOutcomeApplied, result.outcome)
	assertBoundedHistoricalResult(t, prov.messages)
	assert.Equal(t, original, sess.Messages[2].Message.Message.Content)
}

func TestLLMCompactionBoundsHistoricalResult(t *testing.T) {
	t.Parallel()
	prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("summary").AddStopWithUsage(1, 1).Build()}}
	rt := newNativeRuntime(t, agent.New("root", "instructions", agent.WithModel(prov)))
	sess := oversizedHistory(t)
	original := sess.Messages[2].Message.Message.Content
	result := runCompaction(t, rt, sess, "")
	assert.Empty(t, result.errors)
	assert.Equal(t, CompactionOutcomeApplied, result.outcome)
	require.NotEmpty(t, prov.got)
	assertBoundedHistoricalResult(t, prov.got[0])
	assert.Equal(t, original, sess.Messages[2].Message.Message.Content)
}
