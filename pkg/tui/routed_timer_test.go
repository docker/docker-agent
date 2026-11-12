package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// effectRecordingPage records delivery and returns independently classified commands.
type effectRecordingPage struct {
	mockChatPage

	updates []tea.Msg
	effects chat.Effects
}

func (p *effectRecordingPage) UpdateEffects(msg tea.Msg) (chat.Page, chat.Effects) {
	p.updates = append(p.updates, msg)
	return p, p.effects
}

type (
	uiMarkerMsg     struct{}
	timerMarkerMsg  struct{}
	globalMarkerMsg struct{}
)

// newRoutedTestModel wires an appModel around a real supervisor holding two
// sessions (the first added is active) with one chat page each, built by
// makePage.
func newRoutedTestModel(t *testing.T, makePage func(sess *session.Session, routingID string) chat.Page) (m *appModel, activeID, backgroundID string) {
	t.Helper()
	m, _ = newTestModel(t)

	sv := supervisor.New(nil)
	sessA, sessB := session.New(), session.New()
	activeID = sv.AddSession(t.Context(), nil, sessA, "", nil)
	backgroundID = sv.AddSession(t.Context(), nil, sessB, "", nil)
	m.supervisor = sv
	require.Equal(t, activeID, sv.ActiveID())

	m.ensureTab(activeID).chatPage = makePage(sessA, activeID)
	m.ensureTab(backgroundID).chatPage = makePage(sessB, backgroundID)
	m.activeTab = m.tabs[activeID]
	m.ensureTab(activeID).sessionState = service.NewSessionState(sessA)
	m.ensureTab(backgroundID).sessionState = service.NewSessionState(sessB)
	return m, activeID, backgroundID
}

func TestHandleRoutedMsg_InactiveTabDispatchesEffects(t *testing.T) {
	t.Parallel()

	m, _, backgroundID := newRoutedTestModel(t, func(*session.Session, string) chat.Page {
		return &effectRecordingPage{
			effects: chat.Effects{
				Visible: func() tea.Msg { return uiMarkerMsg{} },
				Local:   func() tea.Msg { return timerMarkerMsg{} },
				Global:  func() tea.Msg { return globalMarkerMsg{} },
			},
		}
	})
	background := m.tabs[backgroundID].chatPage.(*effectRecordingPage)

	inner := runtime.AgentSwitching(true, "root", "scout")
	_, cmd := m.Update(messages.RoutedMsg{SessionID: backgroundID, Inner: inner})

	require.Len(t, background.updates, 1, "the inner message reaches the owning page")
	assert.Same(t, inner, background.updates[0])

	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[timerMarkerMsg](msgs), "the page's routed timers are dispatched")
	assert.True(t, hasMsg[globalMarkerMsg](msgs), "global effects are dispatched")
	assert.False(t, hasMsg[uiMarkerMsg](msgs), "UI-only cmds of hidden pages stay discarded")

	// A routed message for an unknown (closed) tab is dropped entirely.
	_, cmd = m.Update(messages.RoutedMsg{SessionID: "gone", Inner: inner})
	assert.Nil(t, cmd)
	assert.Len(t, background.updates, 1)
}

// newRealChatPage builds a real chat page bound to a fresh app around sess,
// with its routing identity set and sized so its sidebar renders. The stream
// cancel on cleanup stops any animations a test started (e.g. a transfer's
// rail), so no registration leaks on the global animation coordinator.
func newRealChatPage(t *testing.T, sess *session.Session, routingID string) chat.Page {
	t.Helper()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	page := chat.New(animation.NewRuntime(), t.Context(), app.New(t.Context(), stubRuntime{}, sess), ss)
	page.SetRoutingID(routingID)
	_ = page.SetSize(140, 40)
	t.Cleanup(func() {
		_, _ = page.Update(messages.StreamCancelledMsg{})
	})
	return page
}

// TestHandleRoutedMsg_TransferOnHiddenTabArmsTimersAndStaysLocal exercises
// the real pieces end to end: a transfer_task start routed to a hidden tab
// shows the transfer box on that tab only (never on the active one) and
// still arms the presentation timers — the command handleRoutedMsg returns —
// without dispatching visible-only commands.
func TestHandleRoutedMsg_TransferOnHiddenTabArmsTimersAndStaysLocal(t *testing.T) {
	t.Parallel()

	m, activeID, backgroundID := newRoutedTestModel(t, func(sess *session.Session, routingID string) chat.Page {
		return newRealChatPage(t, sess, routingID)
	})

	const transferBoxMarker = "─ Transfer "
	_, cmd := m.Update(messages.RoutedMsg{
		SessionID: backgroundID,
		Inner:     runtime.AgentSwitching(true, "root", "scout"),
	})

	assert.NotNil(t, cmd, "the hidden tab's presentation timers run as local work")
	assert.Contains(t, ansi.Strip(m.tabs[backgroundID].chatPage.View()), transferBoxMarker,
		"the hop's box shows on its owning tab")
	assert.NotContains(t, ansi.Strip(m.tabs[activeID].chatPage.View()), transferBoxMarker,
		"the active tab shows nothing for another tab's hop")
}

func TestPageEffectsDispatchPreservesComposition(t *testing.T) {
	t.Parallel()
	for _, visible := range []bool{false, true} {
		t.Run(map[bool]string{false: "hidden", true: "visible"}[visible], func(t *testing.T) {
			t.Parallel()
			var calls []string
			leaf := func(name string) tea.Cmd {
				return func() tea.Msg { calls = append(calls, name); return name }
			}
			effects := chat.Effects{
				Local:   tea.Batch(leaf("local-1"), leaf("local-2")),
				Visible: tea.Sequence(leaf("visible-1"), leaf("visible-2")),
				Global:  tea.Sequence(leaf("global-1"), leaf("global-2")),
			}
			cmd := effects.Cmd(visible)
			assert.Empty(t, calls, "selection must not execute commands in Update")
			msgs := collectMsgs(cmd)
			want := []tea.Msg{"local-1", "local-2", "global-1", "global-2"}
			if visible {
				want = []tea.Msg{"local-1", "local-2", "visible-1", "visible-2", "global-1", "global-2"}
			}
			assert.Equal(t, want, msgs, "nested batches and native sequences must survive dispatch")
			assert.Len(t, calls, len(want), "each selected command runs exactly once")
		})
	}
}
