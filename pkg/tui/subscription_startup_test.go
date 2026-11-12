package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestSupervisorRegistersBeforeProgramReadyWithOtherSubscriber(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.supervisor.Shutdown()
	rt := &stalledDeliveryRuntime{}
	a := app.New(t.Context(), rt, root.application.Session())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	witness := make(chan struct{}, 1)
	ready := make(chan struct{})
	go a.SubscribeReliable(ctx, func(msg tea.Msg) {
		if _, ok := msg.(*runtime.StreamStoppedEvent); ok {
			witness <- struct{}{}
		}
	}, app.WithSubscriptionReady(func() { close(ready) }))
	<-ready
	root.supervisor = supervisor.New(nil)
	root.supervisor.AddSession(t.Context(), a, a.Session(), "", nil)
	root.application = a
	root.activeTab.state = nil
	root.activeTab.chatPage = chat.New(root.ar, t.Context(), a, root.activeTab.sessionState, chat.WithHideSidebar())
	root.handleWindowResize(120, 40)
	rt.emit(runtime.StreamStarted("profile", "root"))
	rt.emit(runtime.AgentChoice("root", "profile", "ANSWER-BEFORE-PROGRAM-READY", "answer"))
	rt.emit(runtime.StreamStopped("profile", "root", "normal"))
	select {
	case <-witness:
	case <-time.After(5 * time.Second):
		t.Fatal("other subscriber did not drain events")
	}
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	program := startStreamingMotionProgram(t, model, tea.WithOutput(&wallClockCountingWriter{}))
	root.supervisor.SetProgram(program)
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(programFrame(t, program)), "ANSWER-BEFORE-PROGRAM-READY")
	}, 10*time.Second, 10*time.Millisecond)
}
