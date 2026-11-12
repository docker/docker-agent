package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestEvaluationJSON(t *testing.T) {
	t.Parallel()

	e := &Evaluation{ID: "request", Evaluator: "judge", AgentName: "root", Model: "judge-model", CreatedAt: "2026-09-22T12:00:00Z"}
	payload, err := json.Marshal(Item{Evaluation: e})
	require.NoError(t, err)
	assert.JSONEq(t, `{"evaluation":{"id":"request","evaluator":"judge","agent_name":"root","model":"judge-model","created_at":"2026-09-22T12:00:00Z"}}`, string(payload))

	e.Cost = new(float64)
	e.Usage = &chat.Usage{}
	payload, err = json.Marshal(e)
	require.NoError(t, err)
	var decoded Evaluation
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.NotNil(t, decoded.Cost)
	assert.Zero(t, *decoded.Cost)
	require.NotNil(t, decoded.Usage)
}

func TestAddEvaluationCopiesAndDeduplicates(t *testing.T) {
	t.Parallel()

	sess := New()
	cost := 0.25
	e := &Evaluation{ID: "request", Evaluator: "judge", Cost: &cost, Usage: &chat.Usage{InputTokens: 100, OutputTokens: 10}}
	sess.AddEvaluation(nil)
	sess.AddEvaluation(e)
	sess.AddEvaluation(e)
	e.Evaluator = "changed"
	e.Usage.InputTokens = 999
	cost = 99
	sess.AddEvaluation(e)

	items := sess.MessagesSnapshot()
	require.Len(t, items, 1)
	assert.Equal(t, "judge", items[0].Evaluation.Evaluator)
	assert.Equal(t, int64(100), items[0].Evaluation.Usage.InputTokens)
	assert.InDelta(t, 0.25, *items[0].Evaluation.Cost, 1e-9)

	items[0].Evaluation.Evaluator = "snapshot changed"
	items[0].Evaluation.Usage.InputTokens = 123
	*items[0].Evaluation.Cost = 123
	assert.Equal(t, "judge", sess.MessagesSnapshot()[0].Evaluation.Evaluator)
	assert.Equal(t, int64(100), sess.MessagesSnapshot()[0].Evaluation.Usage.InputTokens)
	assert.InDelta(t, 0.25, sess.TotalCost(), 1e-9)
}

func TestEvaluationDoesNotChangeChatContext(t *testing.T) {
	t.Parallel()

	sess := New()
	sess.AddMessage(UserMessage("hello"))
	sess.AddMessage(&Message{AgentName: "root", Message: chat.Message{
		Role: chat.MessageRoleAssistant, Content: "answer", Cost: 0.5,
		Usage: &chat.Usage{InputTokens: 12, OutputTokens: 3},
	}})
	sess.SetUsage(12, 3)
	a := agent.New("root", "")
	before := sess.GetMessages(a)
	cost := 0.25
	sess.AddEvaluation(&Evaluation{ID: "request", Cost: &cost, Usage: &chat.Usage{InputTokens: 1000, OutputTokens: 100}})

	assert.Equal(t, before, sess.GetMessages(a))
	assert.Len(t, sess.GetAllMessages(), 2)
	assert.Len(t, sess.OwnMessages(), 2)
	input, output := sess.Usage()
	assert.Equal(t, int64(12), input)
	assert.Equal(t, int64(3), output)
	assert.InDelta(t, 0.75, sess.TotalCost(), 1e-9)
	assert.InDelta(t, 0.75, sess.OwnCost(), 1e-9)
}

func TestEvaluationTotalsAndCopiesIncludeSubSessions(t *testing.T) {
	t.Parallel()

	parent, child := New(), New()
	parentCost, childCost := 0.25, 0.5
	parent.AddEvaluation(&Evaluation{ID: "parent", Cost: &parentCost, Usage: &chat.Usage{InputTokens: 10}})
	parent.AddEvaluation(&Evaluation{ID: "unknown"})
	child.AddEvaluation(&Evaluation{ID: "child", Cost: &childCost, Usage: &chat.Usage{OutputTokens: 20}})
	parent.AddSubSession(child)
	assert.InDelta(t, 0.25, parent.OwnCost(), 1e-9)
	assert.InDelta(t, 0.75, parent.TotalCost(), 1e-9)
	assert.InDelta(t, 0.5, parent.EmbeddedSubSessionCost(), 1e-9)

	for _, kind := range []string{"clone", "branch", "fork"} {
		t.Run(kind, func(t *testing.T) {
			var copied *Session
			switch kind {
			case "clone":
				copied = parent.Clone()
			case "branch":
				var err error
				copied, err = BranchSession(parent, parent.ItemCount())
				require.NoError(t, err)
			case "fork":
				var err error
				copied, err = ForkSession(parent, parent.ItemCount())
				require.NoError(t, err)
			}
			require.Len(t, copied.Messages, 3)
			assert.InDelta(t, 0.75, copied.TotalCost(), 1e-9)
			assert.Nil(t, copied.Messages[1].Evaluation.Cost)
			assert.Nil(t, copied.Messages[1].Evaluation.Usage)
			assert.Equal(t, "parent", copied.Messages[0].Evaluation.ID)
			copied.Messages[0].Evaluation.ID = "changed"
			*copied.Messages[0].Evaluation.Cost = 12
			copied.Messages[0].Evaluation.Usage.InputTokens = 999
			copiedChild := copied.Messages[2].SubSession
			*copiedChild.Messages[0].Evaluation.Cost = 13
			copiedChild.Messages[0].Evaluation.Usage.OutputTokens = 888
			assert.InDelta(t, 0.75, parent.TotalCost(), 1e-9)
			assert.Equal(t, "parent", parent.MessagesSnapshot()[0].Evaluation.ID)
			assert.Equal(t, int64(10), parent.MessagesSnapshot()[0].Evaluation.Usage.InputTokens)
			assert.Equal(t, int64(20), child.MessagesSnapshot()[0].Evaluation.Usage.OutputTokens)
			input, output := copied.Usage()
			assert.Zero(t, input)
			assert.Zero(t, output)
		})
	}

	live := New()
	live.AddLiveSubSession(child)
	assert.InDelta(t, 0.5, live.TotalCost(), 1e-9)
	assert.Zero(t, live.OwnCost())
	assert.Zero(t, live.EmbeddedSubSessionCost())
}

func TestEvaluationConcurrentSnapshots(t *testing.T) {
	t.Parallel()

	sess := New()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			cost := 1.0
			e := &Evaluation{ID: fmt.Sprintf("request-%d", i), Cost: &cost, Usage: &chat.Usage{InputTokens: 1}}
			sess.AddEvaluation(e)
			sess.AddEvaluation(e)
		})
		wg.Go(func() {
			for _, item := range sess.MessagesSnapshot() {
				*item.Evaluation.Cost = 100
				item.Evaluation.Usage.InputTokens = 100
			}
			_ = sess.Clone()
			_ = sess.TotalCost()
		})
	}
	wg.Wait()
	assert.Equal(t, 50, sess.ItemCount())
	assert.InDelta(t, 50, sess.TotalCost(), 1e-9)
}

func TestEvaluationUsageHistoryCopiesAndDeduplicates(t *testing.T) {
	t.Parallel()

	sess := New()
	assert.Nil(t, sess.EvaluationUsageHistorySnapshot())
	sess.AddEvaluationUsageRecord(nil)
	assert.Zero(t, sess.EvaluationUsageHistoryCount())

	cost := 0.25
	e := &Evaluation{ID: "request", Evaluator: "judge", Cost: &cost, Usage: &chat.Usage{InputTokens: 100}}
	sess.AddEvaluationUsageRecord(e)
	e.Evaluator = "changed"
	e.Usage.InputTokens = 999
	cost = 999
	sess.AddEvaluationUsageRecord(e)
	sess.AddEvaluationUsageRecord(&Evaluation{ID: "unknown"})
	sess.AddEvaluationUsageRecord(&Evaluation{ID: "free", Cost: new(float64)})

	records := sess.EvaluationUsageHistorySnapshot()
	require.Len(t, records, 3)
	assert.Equal(t, "judge", records[0].Evaluator)
	assert.Equal(t, int64(100), records[0].Usage.InputTokens)
	assert.InDelta(t, 0.25, *records[0].Cost, 1e-9)
	assert.Nil(t, records[1].Cost)
	assert.Nil(t, records[1].Usage)
	require.NotNil(t, records[2].Cost)
	assert.Zero(t, *records[2].Cost)

	records[0].Evaluator = "snapshot changed"
	records[0].Usage.InputTokens = 123
	*records[0].Cost = 123
	records = sess.EvaluationUsageHistorySnapshot()
	assert.Equal(t, "judge", records[0].Evaluator)
	assert.Equal(t, int64(100), records[0].Usage.InputTokens)
	assert.InDelta(t, 0.25, *records[0].Cost, 1e-9)
	assert.Equal(t, 3, sess.EvaluationUsageHistoryCount())
	assert.Zero(t, sess.ItemCount(), "transient records must not enter the conversation")
	assert.Zero(t, sess.TotalCost(), "transient records must not change persisted totals")
	payload, err := json.Marshal(sess)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "judge")
	assert.NotContains(t, string(payload), "EvaluationUsageHistory")
}

func TestEvaluationUsageHistoryClone(t *testing.T) {
	t.Parallel()

	parent, child := New(), New()
	parent.AddSubSession(child)
	cost := 0.25
	parent.AddEvaluationUsageRecord(&Evaluation{ID: "root", Cost: &cost, Usage: &chat.Usage{InputTokens: 10}})
	child.AddEvaluationUsageRecord(&Evaluation{ID: "child"})
	cloned := parent.Clone()
	assert.Equal(t, parent.EvaluationUsageHistorySnapshot(), cloned.EvaluationUsageHistorySnapshot())
	assert.Equal(t, 2, parent.EvaluationUsageHistoryCount())
	cloned.EvaluationUsageHistory[0].ID = "changed"
	*cloned.EvaluationUsageHistory[0].Cost = 123
	cloned.EvaluationUsageHistory[0].Usage.InputTokens = 999
	cloned.Messages[0].SubSession.EvaluationUsageHistory[0].ID = "changed"

	record := parent.EvaluationUsageHistorySnapshot()[0]
	assert.Equal(t, "root", record.ID)
	assert.InDelta(t, 0.25, *record.Cost, 1e-9)
	assert.Equal(t, int64(10), record.Usage.InputTokens)
	assert.Equal(t, "child", child.EvaluationUsageHistorySnapshot()[0].ID)
	assert.Nil(t, New().Clone().EvaluationUsageHistory)
	empty := New()
	empty.EvaluationUsageHistory = []*Evaluation{}
	assert.NotNil(t, empty.Clone().EvaluationUsageHistory)
	assert.NotNil(t, empty.EvaluationUsageHistorySnapshot())
}

func TestEvaluationUsageHistoryConcurrent(t *testing.T) {
	t.Parallel()

	parent, child := New(), New()
	parent.AddSubSession(child)
	var wg sync.WaitGroup
	for i := range 50 {
		for _, sess := range []*Session{parent, child} {
			for range 2 {
				wg.Go(func() {
					cost := 1.0
					sess.AddEvaluationUsageRecord(&Evaluation{
						ID: fmt.Sprintf("request-%d", i), Cost: &cost, Usage: &chat.Usage{InputTokens: 1},
					})
				})
			}
			wg.Go(func() {
				for _, e := range sess.EvaluationUsageHistorySnapshot() {
					*e.Cost = 100
					e.Usage.InputTokens = 100
				}
				_ = sess.Clone()
				_ = parent.EvaluationUsageHistoryCount()
			})
		}
	}
	wg.Wait()
	assert.Equal(t, 100, parent.EvaluationUsageHistoryCount())
	for _, sess := range []*Session{parent, child} {
		records := sess.EvaluationUsageHistorySnapshot()
		require.Len(t, records, 50)
		for _, e := range records {
			assert.InDelta(t, 1.0, *e.Cost, 1e-9)
			assert.Equal(t, int64(1), e.Usage.InputTokens)
		}
	}
}
