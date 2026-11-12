package chat

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestForkSkillReplyDoesNotMergeWithParent(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sess := session.New()
		const url = "https://github.com/docker/gordon/pull/2287"
		const reply = "Opened PR #2287 — fix(proxy): flush a ping before waiting for Bedrock stream events."
		call := tools.ToolCall{ID: "skill", Function: tools.FunctionCall{Name: "run_skill"}}
		p := renderAppEventBatches(t, sess, [][]runtime.Event{{
			runtime.StreamStarted(sess.ID, "root"),
			runtime.ToolCall(call, tools.Tool{Name: "run_skill"}, "root"),
			runtime.StreamStarted("skill-session", "root"),
			runtime.AgentChoice("root", "skill-session", strings.TrimSuffix(url, "2287")),
			runtime.AgentChoice("root", "skill-session", "2287"),
			runtime.StreamStopped("skill-session", "root", "normal"),
			runtime.ToolCallResponse("skill", tools.Tool{Name: "run_skill"}, tools.ResultSuccess(url), url, "root"),
			runtime.AgentChoice("root", sess.ID, "Opened PR #2287"),
			runtime.AgentChoice("root", sess.ID, strings.TrimPrefix(reply, "Opened PR #2287")),
			runtime.StreamStopped(sess.ID, "root", "normal"),
		}})

		require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
		view := ansi.Strip(p.messages.View())
		require.Contains(t, view, url)
		require.Contains(t, view, reply)
		require.NotContains(t, view, url+"Opened")
		require.False(t, p.working)
	})
}

func TestSameAgentContentSessionBoundaries(t *testing.T) {
	t.Parallel()

	for _, reasoning := range []bool{false, true} {
		name := "text"
		if reasoning {
			name = "reasoning"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
			contentEvent := runtime.AgentChoice
			messageType := types.MessageTypeAssistant
			if reasoning {
				contentEvent = runtime.AgentChoiceReasoning
				messageType = types.MessageTypeAssistantReasoningBlock
			}
			for _, event := range []runtime.Event{
				runtime.StreamStarted(sess.ID, "root"),
				contentEvent("root", sess.ID, "parent"),
				contentEvent("root", "", " legacy chunk"),
				runtime.StreamStarted("first-skill", "root"),
				runtime.StreamStarted("second-skill", "root"),
				contentEvent("root", "first-skill", "first"),
				contentEvent("root", "first-skill", " chunk"),
				contentEvent("root", "second-skill", "second"),
				contentEvent("root", "first-skill", "first again"),
			} {
				_, _ = p.handleRuntimeEvent(event)
			}
			require.Equal(t, 4, p.messages.MessageTypeCount(messageType))
		})
	}
}

func TestSameSessionNewStreamStartsNewMessage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	for range 2 {
		_, _ = p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
		_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "reply"))
		_, _ = p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	}
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
}

func TestSameAgentMediaSessionsStaySeparate(t *testing.T) {
	t.Parallel()

	p, _ := newGeneratedMediaTestPage(t, &resolverTestRuntime{})
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", "child", "child text"))
	_, _ = p.handleRuntimeEvent(assistantMessageAdded("child", workspaceImagePart("child.png", "child.png", "child")))
	_, _ = p.handleRuntimeEvent(assistantMessageAdded("parent", workspaceImagePart("parent.png", "parent.png", "parent")))
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", "parent", "parent text"))
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
}

func TestSiblingStreamLifecycleDoesNotSplitContent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 40)
	for _, event := range []runtime.Event{
		runtime.StreamStarted(sess.ID, "root"),
		runtime.StreamStarted("first-skill", "root"),
		runtime.AgentChoice("root", "first-skill", "https://github.com/"),
		runtime.StreamStarted("second-skill", "root"),
		runtime.AgentChoice("root", "first-skill", "docker/gordon/pull/"),
		runtime.StreamStopped("second-skill", "root", "normal"),
		runtime.AgentChoice("root", "first-skill", "2287"),
	} {
		_, _ = p.handleRuntimeEvent(event)
	}
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAssistant))
	require.Contains(t, ansi.Strip(p.messages.View()), "https://github.com/docker/gordon/pull/2287")
}

// renderAppEventBatches exercises the same throttled subscription used by the TUI.
// Call inside synctest; each batch is flushed before the next one is sent.
func renderAppEventBatches(t *testing.T, sess *session.Session, batches [][]runtime.Event) *chatPage {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan runtime.Event, 32)
	a := app.New(ctx, &skillOutputRuntime{events: events}, sess)
	p := New(animation.NewRuntime(), ctx, a, service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(160, 40)
	received := make(chan tea.Msg, 128)
	go a.SubscribeWith(ctx, func(msg tea.Msg) { received <- msg })
	synctest.Wait()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	a.Run(runCtx, stop, "run skill", nil)
	drain := func() {
		for {
			select {
			case msg := <-received:
				_, _ = p.handleRuntimeEvent(msg)
			default:
				return
			}
		}
	}
	for _, batch := range batches {
		for _, event := range batch {
			events <- event
		}
		synctest.Wait()
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // Advance the throttle timer inside synctest's fake clock.
		synctest.Wait()
		drain()
	}
	close(events)
	synctest.Wait()
	drain()
	return p
}

type skillOutputRuntime struct {
	queueTestRuntime

	events <-chan runtime.Event
}

func (r *skillOutputRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	return r.events
}

func TestAppBatchedSkillContentKeepsSessionIdentity(t *testing.T) {
	t.Parallel()

	for _, reasoning := range []bool{false, true} {
		name := "text"
		if reasoning {
			name = "reasoning"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				contentEvent := runtime.AgentChoice
				messageType := types.MessageTypeAssistant
				if reasoning {
					contentEvent = runtime.AgentChoiceReasoning
					messageType = types.MessageTypeAssistantReasoningBlock
				}
				sess := session.New()
				p := renderAppEventBatches(t, sess, [][]runtime.Event{
					{
						runtime.StreamStarted(sess.ID, "root"),
						runtime.StreamStarted("child", "root"),
						contentEvent("root", "child", "first"),
					},
					{
						contentEvent("root", "child", " second"),
						contentEvent("root", "child", " third"),
						runtime.StreamStopped("child", "root", "normal"),
						runtime.StreamStopped(sess.ID, "root", "normal"),
					},
				})
				require.Equal(t, 1, p.messages.MessageTypeCount(messageType))
			})
		})
	}
}

func TestAppBatchedParallelSkillContentStaysSeparate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sess := session.New()
		p := renderAppEventBatches(t, sess, [][]runtime.Event{{
			runtime.StreamStarted(sess.ID, "root"),
			runtime.StreamStarted("first", "root"),
			runtime.StreamStarted("second", "root"),
			runtime.AgentChoice("root", "first", "first "),
			runtime.AgentChoice("root", "first", "reply"),
			runtime.AgentChoice("root", "second", "second reply"),
			runtime.AgentChoice("root", "first", "first again"),
			runtime.StreamStopped("first", "root", "normal"),
			runtime.StreamStopped("second", "root", "normal"),
			runtime.StreamStopped(sess.ID, "root", "normal"),
		}})
		require.Equal(t, 3, p.messages.MessageTypeCount(types.MessageTypeAssistant))
	})
}

func TestDelayedCancellationCleanupDoesNotSplitNextReply(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 40)
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "old reply"))
	for i := range 4 {
		_, _ = p.handleRuntimeEvent(runtime.ToolCall(tools.ToolCall{ID: strconv.Itoa(i), Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, "root"))
	}
	_, p.msgCancel = context.WithCancel(t.Context())
	_ = p.cancelStream(false)
	_, _ = p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "canceled"))
	p.messages.AddUserMessage("next question")
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	_, _ = p.update(msgtypes.StreamCancelledMsg{})
	for _, chunk := range []string{"next", " answer", "."} {
		_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, chunk))
	}
	_, _ = p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))

	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
	require.Contains(t, ansi.Strip(p.messages.View()), "next answer.")
}
