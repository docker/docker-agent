package messages

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newCompactParityModel(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntime()
	m := newModel(ar, 80, 40, &service.SessionState{})
	t.Cleanup(ar.Stop)
	t.Cleanup(m.StopAnimations)
	return m
}

func requireWorkBefore(t *testing.T, m *model, label string, answers ...string) {
	t.Helper()
	text := ansi.Strip(m.View())
	last := strings.Index(text, label)
	require.GreaterOrEqual(t, last, 0, text)
	for _, answer := range answers {
		at := strings.Index(text, answer)
		require.Greater(t, at, last, text)
		require.Equal(t, 1, strings.Count(text, answer), text)
		last = at
	}
}

func TestWorkOrderLiveProseBeforeActivity(t *testing.T) {
	for _, kind := range []string{"reasoning", "tool"} {
		t.Run(kind, func(t *testing.T) {
			m := newCompactParityModel(t)
			m.SetSize(90, 40)
			m.SetCompactWorkLog(true)
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			m.AppendAssistantContent("s", "one", "root", "firstanswer")
			requireWorkBefore(t, m, "Working", "firstanswer")
			transient := m.work.transient
			require.NotNil(t, transient)
			userEnd := m.work.ends[0]
			require.Empty(t, strings.TrimSpace(ansi.Strip(m.renderedLine(userEnd))))
			require.Contains(t, ansi.Strip(m.renderedLine(userEnd+1)), "Working")
			if kind == "reasoning" {
				m.AppendReasoningContent("s", "one", "root", "privatework")
			} else {
				m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}, tools.Tool{Name: "probe"}, types.ToolStatusRunning)
			}
			requireWorkBefore(t, m, "Working", "firstanswer")
			require.Same(t, transient, m.work.blocks[2])
			require.Nil(t, m.work.transient)
			require.Less(t, m.lineOffsets[2], m.lineOffsets[1])
			require.Equal(t, "firstanswer", m.messages[1].Content)
			idx, _ := m.globalLineToMessageLineCached(m.lineOffsets[1] + 1)
			require.Equal(t, 1, idx, "reordered rows still hit their original source")

			m.AppendAssistantContent("s", "two", "root", "secondanswer")
			requireWorkBefore(t, m, "Working", "firstanswer", "secondanswer")
			m.SetWorkStatus(false, 0)
			requireWorkBefore(t, m, "Work", "firstanswer", "secondanswer")
			m.work.blocks[2].SetExpandedImmediately(true)
			m.invalidateAllItems()
			requireWorkBefore(t, m, "Work", "firstanswer", "secondanswer")

			m.AddUserMessage("nextrequest")
			m.AppendAssistantContent("s", "three", "root", "thirdanswer")
			m.AppendReasoningContent("s", "three", "root", "nextwork")
			sources := slices.Clone(m.messages)
			m.View()
			require.Less(t, m.lineOffsets[2], m.lineOffsets[1])
			require.Less(t, m.lineOffsets[3], m.lineOffsets[4])
			require.Less(t, m.lineOffsets[6], m.lineOffsets[5])
			m.SetCompactWorkLog(false)
			m.View()
			require.Equal(t, sources, m.messages)
			require.Less(t, m.lineOffsets[1], m.lineOffsets[2])
			require.Less(t, m.lineOffsets[5], m.lineOffsets[6])
		})
	}
}

func TestWorkOrderPlainOnlyHeaderIsTransient(t *testing.T) {
	m := newCompactParityModel(t)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendAssistantContent("s", "one", "root", "plainanswer")
	requireWorkBefore(t, m, "Working", "plainanswer")
	m.SetWorkStatus(false, 0)
	frame := ansi.Strip(m.View())
	require.Empty(t, m.work.blocks)
	require.Nil(t, m.work.transient)
	require.Contains(t, frame, "plainanswer")
	require.NotContains(t, frame, "Working")
	require.Equal(t, m.work.ends[0], m.lineOffsets[1], "the ordinary assistant action row owns separation")
}

func TestWorkOrderReplayCombinedParts(t *testing.T) {
	for _, activity := range []string{"reasoning", "tool"} {
		t.Run(activity, func(t *testing.T) {
			sess := session.New(session.WithID("s"))
			sess.AddMessage(session.UserMessage("request"))
			answer := &chat.Message{Role: chat.MessageRoleAssistant, Content: "firstanswer"}
			if activity == "reasoning" {
				answer.ReasoningContent = "privatework"
			} else {
				answer.ToolCalls = []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}}
			}
			sess.AddMessage(session.NewAgentMessage("root", answer))
			m := newCompactParityModel(t)
			m.SetCompactWorkLog(true)
			m.LoadFromSession(sess, nil)
			requireWorkBefore(t, m, "Work", "firstanswer")
			require.Equal(t, "firstanswer", answer.Content)
		})
	}
}

func TestWorkOrderRefreshPreservesReorderedOffsets(t *testing.T) {
	m := newCompactParityModel(t)
	m.SetSize(60, 40)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.AppendToLastMessage("root", "firstanswer")
	m.AppendReasoning("root", "privatework")
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "secondanswer"))
	requireWorkBefore(t, m, "Work", "firstanswer", "secondanswer")
	anchor := m.lineOffsets[2]
	m.messages[1].Content += "\n\nadded paragraph"
	m.views[1] = m.createMessageView(m.messages[1])
	m.invalidateItem(1)
	requireWorkBefore(t, m, "Work", "firstanswer", "added paragraph", "secondanswer")
	require.Equal(t, anchor, m.lineOffsets[2])
	before := slices.Clone(m.lineOffsets)
	m.hoveredMessageIndex = 1
	m.refreshRenderedItem(1)
	m.View()
	require.True(t, m.refreshRenderedItem(1), "a steady boundary still supports the fast refresh")
	require.Equal(t, before[2], m.lineOffsets[2], "hover must not move the reordered Work anchor")
	for _, index := range []int{1, 3} {
		idx, _ := m.globalLineToMessageLineCached(m.lineOffsets[index])
		require.Equal(t, index, idx, "hover must preserve the original sources")
	}
}
