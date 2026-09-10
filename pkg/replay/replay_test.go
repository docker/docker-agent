package replay_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/replay"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func call(name, args string) replay.ToolCall {
	return replay.ToolCall{Name: name, Arguments: args}
}

func turn(content string, calls ...replay.ToolCall) replay.Turn {
	return replay.Turn{Content: content, ToolCalls: calls}
}

// indexed renumbers turns the way TurnsOf would, so hand-built fixtures compare
// cleanly against extracted ones.
func indexed(turns ...replay.Turn) []replay.Turn {
	for i := range turns {
		turns[i].Index = i
	}
	return turns
}

func TestCompare_IdenticalBehaviour(t *testing.T) {
	t.Parallel()

	a := indexed(turn("looking", call("read_file", `{"path":"a"}`)), turn("done"))
	b := indexed(turn("having a look", call("read_file", `{"path":"a"}`)), turn("finished"))

	got := replay.Compare(a, b)
	assert.True(t, got.Identical(), "different prose with identical tool calls is identical behaviour")
	assert.Equal(t, 2, got.TurnsMatched)
	assert.Nil(t, got.Divergence)
}

// The point of the package: prose differences must never register.
func TestCompare_ProseIsNeverADivergence(t *testing.T) {
	t.Parallel()

	a := indexed(turn("I will now read the file."))
	b := indexed(turn("Sure! Let me take a look at that file for you."))

	assert.True(t, replay.Compare(a, b).Identical())
}

func TestCompare_DifferentToolName(t *testing.T) {
	t.Parallel()

	a := indexed(turn("", call("read_file", `{"path":"a"}`)))
	b := indexed(turn("", call("shell", `{"cmd":"cat a"}`)))

	got := replay.Compare(a, b)
	require.NotNil(t, got.Divergence)
	assert.Equal(t, replay.KindToolCalls, got.Divergence.Kind)
	assert.Equal(t, 0, got.Divergence.TurnIndex)
	assert.Zero(t, got.TurnsMatched)
}

func TestCompare_SameToolDifferentArguments(t *testing.T) {
	t.Parallel()

	a := indexed(turn("", call("read_file", `{"path":"a"}`)))
	b := indexed(turn("", call("read_file", `{"path":"b"}`)))

	got := replay.Compare(a, b)
	require.NotNil(t, got.Divergence)
	assert.Equal(t, replay.KindToolCalls, got.Divergence.Kind)
}

// The same set of calls in a different order is a different plan.
func TestCompare_ToolCallOrderMatters(t *testing.T) {
	t.Parallel()

	a := indexed(turn("", call("read_file", "{}"), call("shell", "{}")))
	b := indexed(turn("", call("shell", "{}"), call("read_file", "{}")))

	assert.False(t, replay.Compare(a, b).Identical())
}

func TestCompare_ReportsFirstDivergenceOnly(t *testing.T) {
	t.Parallel()

	a := indexed(
		turn("", call("read_file", `{"path":"a"}`)),
		turn("", call("read_file", `{"path":"b"}`)),
		turn("", call("read_file", `{"path":"c"}`)),
	)
	b := indexed(
		turn("", call("read_file", `{"path":"a"}`)),
		turn("", call("shell", `{"cmd":"x"}`)),
		turn("", call("shell", `{"cmd":"y"}`)),
	)

	got := replay.Compare(a, b)
	require.NotNil(t, got.Divergence)
	assert.Equal(t, 1, got.Divergence.TurnIndex, "the first difference, not the last")
	assert.Equal(t, 1, got.TurnsMatched)
	assert.Equal(t, "read_file", got.Divergence.A.ToolCalls[0].Name)
	assert.Equal(t, "shell", got.Divergence.B.ToolCalls[0].Name)
}

func TestCompare_LengthMismatchAfterMatchingPrefix(t *testing.T) {
	t.Parallel()

	short := indexed(turn("", call("read_file", "{}")))
	long := indexed(turn("", call("read_file", "{}")), turn("", call("shell", "{}")))

	t.Run("second run continued", func(t *testing.T) {
		t.Parallel()
		got := replay.Compare(short, long)
		require.NotNil(t, got.Divergence)
		assert.Equal(t, replay.KindExtraTurn, got.Divergence.Kind)
		assert.Equal(t, 1, got.Divergence.TurnIndex)
		assert.Nil(t, got.Divergence.A)
		require.NotNil(t, got.Divergence.B)
	})

	t.Run("second run stopped early", func(t *testing.T) {
		t.Parallel()
		got := replay.Compare(long, short)
		require.NotNil(t, got.Divergence)
		assert.Equal(t, replay.KindMissingTurn, got.Divergence.Kind)
		require.NotNil(t, got.Divergence.A)
		assert.Nil(t, got.Divergence.B)
	})
}

func TestCompare_EmptyRuns(t *testing.T) {
	t.Parallel()

	assert.True(t, replay.Compare(nil, nil).Identical())

	got := replay.Compare(nil, indexed(turn("", call("shell", "{}"))))
	require.NotNil(t, got.Divergence)
	assert.Equal(t, replay.KindExtraTurn, got.Divergence.Kind)
}

func TestTurnsOf(t *testing.T) {
	t.Parallel()

	sess := &session.Session{Messages: []session.Item{
		{Message: &session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "do it"}}},
		{Message: &session.Message{AgentName: "root", Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "reading",
			ToolCalls: []tools.ToolCall{
				{Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`}},
			},
		}}},
		{Message: &session.Message{Message: chat.Message{Role: chat.MessageRoleTool, Content: "contents"}}},
		{Message: &session.Message{AgentName: "root", Message: chat.Message{
			Role: chat.MessageRoleAssistant, Content: "done",
		}}},
		{}, // a non-message item (e.g. a compaction summary)
	}}

	got := replay.TurnsOf(sess)

	require.Len(t, got, 2, "only assistant messages are turns")
	assert.Equal(t, 0, got[0].Index)
	assert.Equal(t, "root", got[0].Agent)
	assert.Equal(t, "reading", got[0].Content)
	require.Len(t, got[0].ToolCalls, 1)
	assert.Equal(t, "read_file", got[0].ToolCalls[0].Name)
	assert.Equal(t, 1, got[1].Index)
	assert.Empty(t, got[1].ToolCalls)
}

func TestTurnsOf_NilSession(t *testing.T) {
	t.Parallel()
	assert.Empty(t, replay.TurnsOf(nil))
}

func TestCompareSessions(t *testing.T) {
	t.Parallel()

	mk := func(toolName string) *session.Session {
		return &session.Session{Messages: []session.Item{
			{Message: &session.Message{Message: chat.Message{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{Function: tools.FunctionCall{Name: toolName, Arguments: "{}"}},
				},
			}}},
		}}
	}

	assert.True(t, replay.CompareSessions(mk("read_file"), mk("read_file")).Identical())
	assert.False(t, replay.CompareSessions(mk("read_file"), mk("shell")).Identical())
}

func TestPrintResult(t *testing.T) {
	t.Parallel()

	t.Run("identical", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		replay.PrintResult(&buf, replay.Compare(
			indexed(turn("", call("read_file", "{}"))),
			indexed(turn("", call("read_file", "{}"))),
		), "old", "new")
		assert.Contains(t, buf.String(), "Identical behaviour")
	})

	t.Run("divergence names both sides", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		replay.PrintResult(&buf, replay.Compare(
			indexed(turn("", call("read_file", `{"path":"a"}`))),
			indexed(turn("", call("shell", `{"cmd":"x"}`))),
		), "old", "new")

		out := buf.String()
		assert.Contains(t, out, "First divergence at turn 0")
		assert.Contains(t, out, "read_file")
		assert.Contains(t, out, "shell")
		assert.Contains(t, out, "old")
		assert.Contains(t, out, "new")
		assert.Contains(t, out, "downstream of the divergence")
	})

	t.Run("a final answer is labelled", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		replay.PrintResult(&buf, replay.Compare(
			indexed(turn("all done")),
			indexed(turn("", call("shell", "{}"))),
		), "old", "new")
		assert.Contains(t, buf.String(), "no tool calls")
	})

	t.Run("huge arguments are truncated", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		huge := strings.Repeat("x", 5000)
		replay.PrintResult(&buf, replay.Compare(
			indexed(turn("", call("write_file", huge))),
			indexed(turn("", call("shell", "{}"))),
		), "old", "new")
		assert.Less(t, buf.Len(), 1000, "a large payload must not flood the report")
		assert.Contains(t, buf.String(), "…")
	})
}

func TestResult_IsJSONSerializable(t *testing.T) {
	t.Parallel()

	r := replay.Compare(
		indexed(turn("", call("read_file", "{}"))),
		indexed(turn("", call("shell", "{}"))),
	)
	data, err := json.Marshal(r)
	require.NoError(t, err)

	var round replay.Result
	require.NoError(t, json.Unmarshal(data, &round))
	require.NotNil(t, round.Divergence)
	assert.Equal(t, replay.KindToolCalls, round.Divergence.Kind)
}
