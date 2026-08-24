package tui

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func populateScrollableRoot(t *testing.T) *appModel {
	t.Helper()
	root, _, _ := wallClockRoot(t, 100, 35)
	for i := range 30 {
		msg := session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: fmt.Sprintf("message %02d\n%s", i, strings.Repeat("content line\n", 4)),
		})
		root.application.Session().Messages = append(root.application.Session().Messages, session.NewMessageItem(msg))
	}
	_ = root.chatPage.Init()
	root.handleWindowResize(100, 35)
	root.chatPage.ScrollToBottom()
	root.viewCacheValid = false
	return root
}

func TestEffectiveScrollInputRendersImmediately(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{name: "wheel", msg: messages.WheelCoalescedMsg{Delta: -1, X: 1, Y: 1}},
		{name: "key", msg: tea.KeyPressMsg{Code: tea.KeyPgUp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := populateScrollableRoot(t)
			root.focusedPanel = PanelContent
			_ = root.chatPage.FocusMessages()
			before := root.View().Content

			_, _ = root.Update(tc.msg)
			after := root.View().Content

			require.NotEqual(t, before, after, "effective input reused the stale root frame")
			root.ar.Stop()
		})
	}
}

func TestStreamChunkRendersBeforeRecoveryClick(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(tea.Msg) tea.Msg
	}{
		{name: "direct", wrap: func(msg tea.Msg) tea.Msg { return msg }},
		{name: "routed", wrap: func(msg tea.Msg) tea.Msg {
			return messages.RoutedMsg{SessionID: "profile", Inner: msg}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			seed := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first chunk"})
			root.application.Session().Messages = append(root.application.Session().Messages, session.NewMessageItem(seed))
			_ = root.chatPage.Init()
			root.viewCacheValid = false
			_ = root.View()

			_, _ = root.Update(tc.wrap(agentruntime.AgentChoice("root", "profile", " STREAM-IDLE-MARKER")))
			immediate := root.View().Content
			_, _ = root.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: root.height - 1})
			afterClick := root.View().Content

			require.Contains(t, ansi.Strip(immediate), "STREAM-IDLE-MARKER", "chunk stayed hidden until pointer input")
			require.Equal(t, immediate, afterClick, "inert boundary click repaired a stale frame")
			root.ar.Stop()
		})
	}
}

type cacheProgramAck struct{ done chan struct{} }

type cacheProgramModel struct{ root *appModel }

func (m *cacheProgramModel) Init() tea.Cmd { return nil }

func (m *cacheProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ack, ok := msg.(cacheProgramAck); ok {
		close(ack.done)
		return m, nil
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *cacheProgramModel) View() tea.View { return m.root.View() }

type cacheProgramWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *cacheProgramWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *cacheProgramWriter) snapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestActualProgramWritesAfterEffectiveIdleInput(t *testing.T) {
	root := populateScrollableRoot(t)
	model := &cacheProgramModel{root: root}
	writer := &cacheProgramWriter{}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(writer), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	require.Eventually(t, func() bool { return writer.snapshot() != "" }, time.Second, time.Millisecond)
	ack := func() {
		done := make(chan struct{})
		program.Send(cacheProgramAck{done: done})
		<-done
	}
	ack()

	beforeWheel := len(writer.snapshot())
	program.Send(messages.WheelCoalescedMsg{Delta: -1, X: 1, Y: 1})
	ack()
	require.Eventually(t, func() bool { return len(writer.snapshot()) > beforeWheel }, time.Second, time.Millisecond,
		"effective wheel input produced no terminal write")

	program.Send(messages.WheelCoalescedMsg{Delta: 1_000_000, X: 1, Y: 1})
	ack()
	program.Send(agentruntime.AgentChoice("root", "profile", " PROGRAM-STREAM-MARKER"))
	ack()
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(writer.snapshot()), "PROGRAM-STREAM-MARKER")
	}, time.Second, time.Millisecond, "stream chunk produced no terminal write while idle")

	program.Quit()
	require.NoError(t, <-done)
	root.ar.Stop()
}

func TestNoOpPointerAndWheelReuseRootCache(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.focusedPanel = PanelContent
	_ = root.chatPage.FocusMessages()
	_, _ = root.Update(tea.KeyPressMsg{Code: 'g'}) // top boundary
	first := root.View()
	for range 100 {
		_, _ = root.Update(messages.WheelCoalescedMsg{Delta: -1, X: 1, Y: 1})
		if got := root.View(); got.Content != first.Content || !root.viewCacheValid {
			t.Fatal("top-boundary wheel invalidated cached root view")
		}
	}
	_, _ = root.Update(tea.MouseMotionMsg{X: 0, Y: root.height - 1})
	first = root.View()
	for range 100 {
		_, _ = root.Update(tea.MouseMotionMsg{X: 0, Y: root.height - 1})
		if got := root.View(); got.Content != first.Content || !root.viewCacheValid {
			t.Fatal("identical no-op motion invalidated cached root view")
		}
	}
	root.ar.Stop()
}
