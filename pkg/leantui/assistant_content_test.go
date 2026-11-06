package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func leanTranscriptText(m *model) string {
	return ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, nil), "\n"))
}

func TestCanonicalAssistantContentReconcilesLeanTranscript(t *testing.T) {
	t.Parallel()
	for _, partial := range []string{"", "FINAL-", "ANSWER", "FINAL-ANSWER"} {
		t.Run(partial, func(t *testing.T) {
			m := bareModel(40)
			m.handleEvent(t.Context(), runtime.StreamStarted("session", "root"))
			if partial != "" {
				m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", partial, "answer"))
			}
			msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "answer", Content: "FINAL-ANSWER"})
			for range 2 {
				m.handleEvent(t.Context(), runtime.MessageAdded("session", msg, "root"))
			}
			require.Equal(t, 1, strings.Count(leanTranscriptText(m), "FINAL-ANSWER"))
		})
	}
}

func TestLeanRetryMessageIDsKeepAnswerVisible(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", "[diagnostic](", "attempt-one"))
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", "FINAL-ANSWER)", "attempt-two"))
	require.Contains(t, leanTranscriptText(m), "FINAL-ANSWER")
}

func TestLeanInterleavedSessionsDoNotMergeMarkdown(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "child", "[diagnostic](", "answer"))
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "parent", "FINAL-ANSWER)", "answer"))
	require.Contains(t, leanTranscriptText(m), "FINAL-ANSWER")
}

func TestRetiredLeanEventsCannotAffectReplacement(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	old := leanEvent{generation: 0, inner: runtime.AgentChoice("root", "same-session", "OLD-ANSWER", "old")}
	m.eventGeneration.Add(1)
	m.handleEvent(t.Context(), old)
	m.handleEvent(t.Context(), leanEvent{generation: 1, inner: runtime.AgentChoice("root", "same-session", "NEW-ANSWER", "new")})
	require.NotContains(t, leanTranscriptText(m), "OLD-ANSWER")
	require.Contains(t, leanTranscriptText(m), "NEW-ANSWER")
}

func TestLeanCanonicalContentCannotRevealSuppressedToolXML(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "answer", Content: `safe prefix<tool_call>{"arguments":"PRIVATE-TOOL-ARGUMENT`})
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", "safe prefix", "answer"))
	m.handleEvent(t.Context(), runtime.MessageAdded("session", msg, "root"))
	require.Contains(t, leanTranscriptText(m), "safe prefix")
	require.NotContains(t, leanTranscriptText(m), "PRIVATE-TOOL-ARGUMENT")
}

func TestLeanRecoveryClearsNestedBusyWithoutDrainingQueue(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	for range 3 {
		m.handleEvent(t.Context(), runtime.StreamStarted("session", "root"))
	}
	m.queue = []ui.PendingUserMessage{{Content: "must not run"}}
	m.handleEvent(t.Context(), runtime.SessionRecovered(""))
	require.Zero(t, m.streamDepth)
	require.False(t, m.busy)
	require.Len(t, m.queue, 1)
}

func TestLeanRecoveryRestoresRootContextUsage(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	m.handleEvent(t.Context(), runtime.StreamStarted("root", "root"))
	m.handleEvent(t.Context(), runtime.NewTokenUsageEvent("root", "root", &runtime.Usage{ContextLength: 100}))
	m.handleEvent(t.Context(), runtime.StreamStarted("child", "child"))
	m.handleEvent(t.Context(), runtime.NewTokenUsageEvent("child", "child", &runtime.Usage{ContextLength: 999}))
	m.handleEvent(t.Context(), runtime.SessionRecovered(""))
	m.handleEvent(t.Context(), runtime.StreamStarted("root", "root"))
	m.handleEvent(t.Context(), runtime.StreamStopped("root", "root", "normal"))
	require.Equal(t, int64(100), m.status.ContextLength)
}

func TestReasoningResetClearsCommittedAndPendingSegments(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	ctx := t.Context()
	for _, e := range []runtime.Event{
		runtime.AgentChoiceReasoning("root", "other-session", "OTHER-SESSION", "answer"),
		runtime.AgentChoiceReasoning("root", "s", "OTHER-MESSAGE", "other-message"),
		runtime.AgentChoiceReasoning("root", "s", "DECLINED-COMMITTED", "answer"),
		runtime.AgentChoice("root", "s", "KEPT-TEXT", "answer"),
		runtime.PartialToolCall(tools.ToolCall{ID: "kept-tool", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, "root"),
		runtime.AgentChoiceReasoning("root", "s", "DECLINED-PENDING", "answer"),
		runtime.AgentChoiceReasoningReset("root", "s", "answer"),
	} {
		m.handleEvent(ctx, e)
	}
	frame := leanTranscriptText(m)
	require.NotContains(t, frame, "DECLINED")
	require.Contains(t, frame, "OTHER-SESSION")
	require.Contains(t, frame, "OTHER-MESSAGE")
	require.Contains(t, frame, "KEPT-TEXT")
	require.Equal(t, 1, m.screen.Transcript.ToolCount())
	m.handleEvent(ctx, runtime.AgentChoiceReasoning("root", "s", "FALLBACK", "answer"))
	frame = leanTranscriptText(m)
	require.Contains(t, frame, "FALLBACK")
	require.NotContains(t, frame, "DECLINED")
}
