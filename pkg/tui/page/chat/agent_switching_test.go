package chat

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// agentSwitchCall records one SetAgentSwitching invocation on the sidebar.
type agentSwitchCall struct {
	switching bool
	from, to  string
}

// recordingSidebar wraps the real sidebar to record the delegation-related
// calls the chat page makes on it (and the results the sidebar returned). It
// only survives handlers that do not forward events through sidebar.Update
// (which replaces p.sidebar with the unwrapped model), which is exactly the
// set under test here.
type recordingSidebar struct {
	sidebar.Model

	switching []agentSwitchCall
	results   []sidebar.AgentSwitchResult
	activity  []string
}

func (r *recordingSidebar) SetAgentSwitching(switching bool, fromAgent, toAgent string) sidebar.AgentSwitchResult {
	r.switching = append(r.switching, agentSwitchCall{switching: switching, from: fromAgent, to: toAgent})
	res := r.Model.SetAgentSwitching(switching, fromAgent, toAgent)
	r.results = append(r.results, res)
	return res
}

func (r *recordingSidebar) SetAgentActivity(agentName string) tea.Cmd {
	r.activity = append(r.activity, agentName)
	return r.Model.SetAgentActivity(agentName)
}

// newSwitchingTestPage builds a chat page whose sidebar records delegation
// calls.
func newSwitchingTestPage(t *testing.T) (*chatPage, *recordingSidebar) {
	t.Helper()
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	rec := &recordingSidebar{Model: p.sidebar}
	p.sidebar = rec
	// Tests start transfers without always stopping them; the cancel clears
	// the sidebar's animation subscription so no registration leaks on the
	// global coordinator. Sending through p.sidebar reaches the real model
	// even while it is still the wrapper (Update is promoted from the
	// embedded Model) and after a handler unwrapped it.
	t.Cleanup(func() {
		_, _ = p.sidebar.Update(msgtypes.StreamCancelledMsg{})
	})
	return p, rec
}

// TestAgentSwitchingReturnAddsTransition verifies the end of a transfer_task
// hop — matching its recorded start — adds the "child returned control to
// parent" transition to the chat and reaches the sidebar's return
// presentation.
func TestAgentSwitchingReturnAddsTransition(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)

	handled, cmd := p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	require.NotNil(t, cmd)

	out := ansi.Strip(p.messages.View())
	assert.Contains(t, out, "researcher")
	assert.Contains(t, out, "root")
	assert.Contains(t, out, types.AgentReturnLabel)

	require.Len(t, rec.switching, 2, "the sidebar sees both hop boundaries")
	assert.Equal(t, agentSwitchCall{switching: false, from: "researcher", to: "root"}, rec.switching[1])
	assert.True(t, rec.results[1].Accepted, "the stop closed its recorded start")
}

func TestEscKeepsOnlyTheTransferThatProducedAVisibleReply(t *testing.T) {
	t.Parallel()

	sess := session.New()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	p := New(ar, t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess),
		WithInterruptMode(msgtypes.InterruptModeNone)).(*chatPage)
	p.messages.SetSize(100, 40)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel

	transfer := tools.Tool{Name: transfertask.ToolNameTransferTask}
	call := tools.ToolCall{ID: "transfer", Function: tools.FunctionCall{Name: transfer.Name, Arguments: `{"agent":"researcher","task":"Find the facts."}`}}
	silent := tools.ToolCall{ID: "silent", Function: tools.FunctionCall{Name: transfer.Name, Arguments: `{"agent":"reviewer","task":"Check the facts."}`}}
	ordinary := tools.ToolCall{ID: "shell", Function: tools.FunctionCall{Name: "shell"}}
	retracted := tools.ToolCall{ID: "retracted", Function: tools.FunctionCall{Name: transfer.Name, Arguments: `{"agent":"editor","task":"Edit the facts."}`}}
	for _, event := range []runtime.Event{
		runtime.StreamStarted(sess.ID, "root"),
		runtime.AgentChoice("root", sess.ID, "I will delegate this."),
		runtime.ToolCall(call, transfer, "root"),
		runtime.AgentSwitching(true, "root", "researcher"),
		runtime.StreamStarted("child", "researcher"),
		runtime.AgentChoice("researcher", "child", "Here are the facts."),
		runtime.ToolCall(ordinary, tools.Tool{Name: "shell"}, "researcher"),
		runtime.ToolCall(silent, transfer, "researcher"),
		runtime.PartialToolCall(retracted, transfer, "researcher"),
		runtime.ToolCallsRetracted([]string{retracted.ID}, "researcher"),
	} {
		handled, _ := p.handleRuntimeEvent(event)
		require.True(t, handled)
	}
	before := ansi.Strip(p.messages.View())
	require.Regexp(t, `(?s)I will delegate this\..*root\s+calls\s+researcher.*Here are the facts\.`, before)
	require.Contains(t, before, "shell")
	require.Contains(t, before, "reviewer")
	require.True(t, ar.HasActive())

	_, cmd := p.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	for _, msg := range runTimerCmd(t, cmd) {
		_, _ = p.update(msg)
	}
	_, _ = p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))

	after := ansi.Strip(p.messages.View())
	assert.Regexp(t, `(?s)I will delegate this\..*root\s+calls\s+researcher.*Here are the facts\.`, after)
	assert.Equal(t, 1, strings.Count(after, "researcher"))
	assert.Contains(t, after, "● Find the facts.")
	assert.NotContains(t, after, "Running for")
	assert.NotContains(t, after, "✓")
	assert.NotContains(t, after, "✗")
	assert.NotContains(t, after, types.AgentReturnLabel)
	assert.NotContains(t, after, "shell")
	assert.NotContains(t, after, "reviewer")
	assert.NotContains(t, after, "editor")
	assert.False(t, ar.HasActive())
}

func TestTranscriptTransferAfterSilentChildSwitch(t *testing.T) {
	t.Parallel()

	for _, end := range []string{"late error", "cancel", "recover"} {
		t.Run(end, func(t *testing.T) {
			p, _ := newSwitchingTestPage(t)
			t.Cleanup(p.ar.Stop)
			p.messages.SetSize(100, 40)
			parent := p.app.Session().ID
			def := tools.Tool{Name: transfertask.ToolNameTransferTask}
			call := tools.ToolCall{ID: "transfer", Function: tools.FunctionCall{Name: def.Name, Arguments: `{"agent":"router","task":"Find facts."}`}}
			result := tools.ResultError("Child failed after replying.")
			emit := func(events ...runtime.Event) {
				for _, event := range events {
					handled, _ := p.handleRuntimeEvent(event)
					require.Truef(t, handled, "%T", event)
				}
			}
			p.messages.AddUserMessage("Find the facts.")
			emit(runtime.StreamStarted(parent, "root"), runtime.ToolCall(call, def, "root"),
				runtime.AgentSwitching(true, "root", "router"), runtime.StreamStarted("child", "router"),
				runtime.AgentChoice("researcher", "child", "Here are the facts."))
			out := ansi.Strip(p.messages.View())
			require.Regexp(t, `(?s)root\s+calls\s+router.*router\s*→\s*researcher.*Here are the facts\.`, out)

			if end == "cancel" || end == "recover" {
				if end == "cancel" {
					_, _ = p.update(msgtypes.StreamCancelledMsg{ShowMessage: true})
				} else {
					emit(runtime.SessionRecovered(parent))
				}
				out = ansi.Strip(p.messages.View())
				assert.Contains(t, out, "● Find facts.")
				assert.Equal(t, end == "cancel", strings.Contains(out, "stream cancelled"))
				assert.NotContains(t, out, types.AgentReturnLabel)
			} else {
				emit(runtime.AgentSwitching(false, "router", "root"),
					runtime.ToolCallResponse(call.ID, def, result, result.Output, "root"),
					runtime.AgentChoice("root", parent, "Continuing despite the failure."))
				out = ansi.Strip(p.messages.View())
				assert.Contains(t, out, "✗ Find facts.")
				assert.Regexp(t, `(?s)Here are the facts\..*router\s+returned control to\s+root.*Continuing despite the failure\.`, out)
			}
			assert.Regexp(t, `(?s)root\s+calls\s+router.*router\s*→\s*researcher.*Here are the facts\.`, out)
			assert.Equal(t, 1, strings.Count(out, "→"))
		})
	}
}

// TestAgentSwitchingStartAddsNoTransition verifies the start of a hop adds no
// return transition (the transfer_task tool call already narrates it) while
// still reaching the sidebar.
func TestAgentSwitchingStartAddsNoTransition(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)

	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	require.Len(t, rec.switching, 1)
	assert.Equal(t, agentSwitchCall{switching: true, from: "root", to: "researcher"}, rec.switching[0])
}

// TestAgentSwitchingStaleStopAddsNothing verifies a stop the sidebar did not
// accept — no recorded start to close — adds no transition: an empty stack
// stays empty and a stale stop cannot steal a different in-flight hop.
func TestAgentSwitchingStaleStopAddsNothing(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	// Stop on an empty stack: no label.
	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	require.Len(t, rec.results, 1)
	assert.False(t, rec.results[0].Accepted)

	// A different hop is in flight; the stale stop must not pop it nor label.
	handled, _ = p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "coder"))
	require.True(t, handled)
	handled, _ = p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	require.Len(t, rec.results, 3)
	assert.False(t, rec.results[2].Accepted, "the stale stop must not match the root→coder hop")

	// The genuine stop still labels.
	handled, _ = p.handleRuntimeEvent(runtime.AgentSwitching(false, "coder", "root"))
	require.True(t, handled)
	assert.Contains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
}

// TestAgentSwitchingReturnAfterCancelStaysSilent verifies hop boundaries
// unwinding after the user cancelled the stream are dropped entirely: the
// delegation they belong to was already torn down visually.
func TestAgentSwitchingReturnAfterCancelStaysSilent(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	// The hop started before the cancel; its end races the cancel teardown.
	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)
	p.streamCancelled = true

	handled, cmd := p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	assert.Nil(t, cmd)
	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	assert.Len(t, rec.switching, 1, "the sidebar sees no boundary after the cancel")
}

// TestChildActivityEventsAckSidebar verifies the child-activity signals —
// reasoning, content, and (partial) tool calls, which models may emit without
// any text — all acknowledge the sidebar's outbound transfer presentation.
func TestChildActivityEventsAckSidebar(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)
	p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))

	events := []tea.Msg{
		runtime.AgentChoiceReasoning("researcher", "s1", "thinking…"),
		runtime.AgentChoice("researcher", "s1", "found it"),
		runtime.PartialToolCall(tools.ToolCall{ID: "t1"}, tools.Tool{Name: "shell"}, "researcher"),
	}
	for _, evt := range events {
		handled, _ := p.handleRuntimeEvent(evt)
		require.Truef(t, handled, "%T must be handled", evt)
	}

	assert.Equal(t, []string{"researcher", "researcher", "researcher"}, rec.activity,
		"each useful child event acknowledges the transfer")
}

// TestChildActivityIgnoredAfterCancel verifies content events of a cancelled
// stream do not reach the sidebar's activity tracking.
func TestChildActivityIgnoredAfterCancel(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)
	p.streamCancelled = true

	handled, _ := p.handleRuntimeEvent(runtime.AgentChoice("researcher", "s1", "late chunk"))
	require.True(t, handled)
	assert.Empty(t, rec.activity)
}

// TestScheduleTransferTimersWrapsInRoutedMsg executes the real timer command
// produced for a routed page (with a synthetic short duration) and verifies
// the expiry is wrapped in a messages.RoutedMsg addressed to the page's tab,
// while a page without a routing identity delivers the raw payload.
func TestScheduleTransferTimersWrapsInRoutedMsg(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		type payload struct{ n int }

		p, _ := newSwitchingTestPage(t)
		p.SetRoutingID("tab-1")
		cmd := p.scheduleTransferTimers([]sidebar.TransferTimer{{Duration: time.Millisecond, Msg: payload{n: 42}}})
		require.NotNil(t, cmd)

		msgs := runTimerCmd(t, cmd)
		require.Len(t, msgs, 1)
		assert.Equal(t, msgtypes.RoutedMsg{SessionID: "tab-1", Inner: payload{n: 42}}, msgs[0],
			"the expiry is addressed to the owning tab")

		p.SetRoutingID("")
		cmd = p.scheduleTransferTimers([]sidebar.TransferTimer{{Duration: time.Millisecond, Msg: payload{n: 7}}})
		msgs = runTimerCmd(t, cmd)
		require.Len(t, msgs, 1)
		assert.Equal(t, payload{n: 7}, msgs[0], "standalone pages deliver the raw payload")
	})
}

// runTimerCmd executes a (possibly batched) command, following nested batches,
// and returns the messages it produces.
func runTimerCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, inner := range batch {
			msgs = append(msgs, runTimerCmd(t, inner)...)
		}
		return msgs
	}
	return []tea.Msg{msg}
}

func TestAgentSwitchingReturnsLocalWorkPerUpdate(t *testing.T) {
	t.Parallel()

	p, _ := newSwitchingTestPage(t)
	p.SetRoutingID("tab-1")

	_, start := p.UpdateEffects(runtime.AgentSwitching(true, "root", "researcher"))
	require.NotNil(t, start.Local, "a start arms min/max timers")
	assert.Nil(t, p.effects, "collection ends when UpdateEffects returns")

	_, stop := p.UpdateEffects(runtime.AgentSwitching(false, "researcher", "root"))
	require.NotNil(t, stop.Local, "an accepted stop arms the Return timer")

	_, cancelled := p.UpdateEffects(msgtypes.StreamCancelledMsg{})
	assert.Nil(t, cancelled.Local, "updates without boundaries must not redispatch prior work")
	require.NotNil(t, start.Local, "later updates must not mutate earlier results")
}

// TestRoutedTimerExpiryDrivesSidebarOnOwnerPage chains the real pieces: the
// hop boundary's timers are wrapped for the page's tab, and delivering their
// inner payloads back to the page (as the appModel's routing does) drives the
// sidebar's presentation windows.
func TestRoutedTimerExpiryDrivesSidebarOnOwnerPage(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p, rec := newSwitchingTestPage(t)
		p.SetRoutingID("tab-1")
		p.sessionState.SetCurrentAgentName("root")

		handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
		require.True(t, handled)
		require.Contains(t, ansi.Strip(p.sidebar.View()), transferBoxMarker, "the outbound box shows on the hop start")
		require.Len(t, rec.results, 1)
		timers := rec.results[0].Timers
		require.Len(t, timers, 2)

		// Rewrap each real payload through the page's routed tick (short
		// duration), check the envelope targets this tab, and deliver the
		// unwrapped payload to the page as handleRoutedMsg does.
		for _, timer := range timers {
			msg := p.routedTimerCmd(sidebar.TransferTimer{Duration: time.Millisecond, Msg: timer.Msg})()
			routed, ok := msg.(msgtypes.RoutedMsg)
			require.True(t, ok)
			assert.Equal(t, "tab-1", routed.SessionID)

			_, _ = p.Update(routed.Inner)
		}

		// Min then max elapsed without activity: the outbound box is gone.
		assert.NotContains(t, ansi.Strip(p.sidebar.View()), transferBoxMarker)
	})
}

// transferBoxMarker is the visible marker of the sidebar transfer box (the
// box title embedded in the rounded top border).
const transferBoxMarker = "─ Transfer "
