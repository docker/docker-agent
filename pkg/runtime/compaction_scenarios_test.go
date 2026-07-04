package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// collectCompactionEvents runs compactWithReason and returns the ordered
// event kinds relevant to compaction UX.
func collectCompactionEvents(t *testing.T, rt *LocalRuntime, sess *session.Session) (kinds []string) {
	t.Helper()
	events := make(chan Event, 256) // ample headroom: a blocked sink would deadlock the test
	rt.compactWithReason(t.Context(), sess, "", compactionReasonManual, NewChannelSink(events))
	close(events)
	for ev := range events {
		switch e := ev.(type) {
		case *SessionCompactionEvent:
			kinds = append(kinds, "compaction:"+e.Status)
		case *SessionSummaryEvent:
			kinds = append(kinds, "summary")
		case *ErrorEvent:
			kinds = append(kinds, "error")
		}
	}
	return kinds
}

func twoMessageSession() *session.Session {
	return session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "hi"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hello"}}),
	}))
}

// Scenario: when the model definition can't be resolved (unknown model, no
// provider_opts), doCompact emits started -> error -> completed; the
// started/completed pairing is what UIs use for spinner logic.
func TestScenario_CompactionFailure_EmitsErrorAndCompleted(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}), // context limit unresolvable -> failure
	)
	require.NoError(t, err)

	sess := twoMessageSession()
	kinds := collectCompactionEvents(t, rt, sess)

	// The started/completed pairing is preserved for spinner logic.
	assert.Equal(t, []string{"compaction:started", "error", "compaction:completed"}, kinds)
	assert.Len(t, sess.Messages, 2, "failed compaction must not modify the session")
}

// Scenario: when the summary model returns an empty response, the
// compaction must be a no-op: the session is left untouched (an empty
// model response must NOT fall back to the conversation's own last
// assistant message — that would wipe real history).
func TestScenario_EmptySummary_IsNoOp(t *testing.T) {
	t.Parallel()

	emptySummaryStream := newStreamBuilder().AddStopWithUsage(1, 0).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{emptySummaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	sess := twoMessageSession()
	kinds := collectCompactionEvents(t, rt, sess)

	assert.Equal(t, []string{"compaction:started", "compaction:completed"}, kinds)
	assert.Len(t, sess.Messages, 2, "an empty model response must leave the session unmodified")
}

// Scenario: session token bookkeeping right after a successful compaction.
// InputTokens is reset to the summary size only; the kept-tail messages
// still enter the next prompt but are not accounted for until the next
// model call reports real usage. Verifies the value is at least sane
// (non-negative, much smaller than before).
func TestScenario_PostCompactionTokenAccounting(t *testing.T) {
	t.Parallel()

	summaryStream := newStreamBuilder().AddContent("the summary").AddStopWithUsage(50, 7).Build()
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{summaryStream}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStoreWithLimit{limit: 100_000}),
	)
	require.NoError(t, err)

	// A session with a big kept tail: two large recent messages.
	big := strings.Repeat("x", 40_000) // ~11.4k tokens each
	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: big}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: big}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "recent question"}}),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "recent answer"}}),
	}))
	sess.SetUsage(90_000, 500)

	kinds := collectCompactionEvents(t, rt, sess)
	require.Contains(t, kinds, "summary")

	in, out := sess.Usage()
	assert.Equal(t, int64(7), in, "InputTokens after compaction = summary output tokens only (kept tail unaccounted)")
	assert.Zero(t, out)
}
