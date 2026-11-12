package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type stalledDeliveryRuntime struct {
	stubRuntime

	emit func(agentruntime.Event)
}

func (r *stalledDeliveryRuntime) OnBackgroundEvent(emit func(agentruntime.Event)) {
	r.emit = emit
}

type stallDeliveryMsg struct{}

type stalledDeliveryModel struct {
	*streamingMotionModel

	stalled    chan struct{}
	resume     chan struct{}
	subscribed chan struct{}
	started    chan struct{}
	stopped    chan struct{}
}

func (m *stalledDeliveryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(stallDeliveryMsg); ok {
		close(m.stalled)
		<-m.resume
		return m, nil
	}
	if routed, ok := msg.(messages.RoutedMsg); ok {
		if _, ok := routed.Inner.(*agentruntime.SessionTitleEvent); ok {
			select {
			case <-m.subscribed:
			default:
				close(m.subscribed)
			}
		}
	}
	_, cmd := m.streamingMotionModel.Update(msg)
	if !m.root.activeTab.chatPage.IsWorking() {
		select {
		case <-m.started:
			select {
			case <-m.stopped:
			default:
				close(m.stopped)
			}
		default:
		}
	} else {
		select {
		case <-m.started:
		default:
			close(m.started)
		}
	}
	return m, cmd
}

func TestActualProgramStalledDeliveryKeepsFinalResponse(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(map[bool]string{false: "visible", true: "hidden"}[hidden], func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 120, 40)
			rt := &stalledDeliveryRuntime{}
			a := app.New(t.Context(), rt, root.application.Session())
			root.supervisor.ReplaceRunnerApp(t.Context(), "profile", a, "", nil)
			root.application = a
			root.activeTab.chatPage = chat.New(root.ar, t.Context(), a, root.activeTab.sessionState, chat.WithHideSidebar())
			root.handleWindowResize(120, 40)
			model := &stalledDeliveryModel{
				streamingMotionModel: &streamingMotionModel{root: root, ready: make(chan struct{})},
				stalled:              make(chan struct{}),
				resume:               make(chan struct{}),
				subscribed:           make(chan struct{}),
				started:              make(chan struct{}),
				stopped:              make(chan struct{}),
			}
			program := startTestProgram(t, root, model, tea.WithOutput(&wallClockCountingWriter{}))
			<-model.ready
			root.supervisor.SetProgram(program)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			witness := make(chan struct{}, 1)
			registered := make(chan struct{}, 1)
			go a.SubscribeWith(ctx, func(msg tea.Msg) {
				switch msg.(type) {
				case *agentruntime.StreamStoppedEvent:
					witness <- struct{}{}
				case *agentruntime.SessionTitleEvent:
					select {
					case registered <- struct{}{}:
					default:
					}
				}
			})
			require.Eventually(t, func() bool {
				rt.emit(agentruntime.SessionTitle("profile", "subscription-ready"))
				select {
				case <-model.subscribed:
					select {
					case <-registered:
						return true
					default:
					}
				default:
				}
				return false
			}, 5*time.Second, 10*time.Millisecond)
			rt.emit(agentruntime.StreamStarted("profile", "root"))
			select {
			case <-model.started:
			case <-time.After(30 * time.Second):
				t.Fatal("the TUI did not receive stream start")
			}
			if hidden {
				program.Send(tmuxVisibilityMsg{hidden: true})
				programAck(t, program)
			}
			program.Send(stallDeliveryMsg{})
			<-model.stalled
			t.Cleanup(func() {
				select {
				case <-model.resume:
				default:
					close(model.resume)
				}
			})
			for range 1100 {
				rt.emit(agentruntime.NewTokenUsageEvent("profile", "root", &agentruntime.Usage{}))
			}
			rt.emit(agentruntime.AgentChoice("root", "profile", "FINAL-RESPONSE-AFTER-STALL", "answer"))
			rt.emit(agentruntime.StreamStopped("profile", "root", "normal"))
			select {
			case <-witness:
			case <-time.After(30 * time.Second):
				t.Fatal("a stalled TUI blocked fan-out to other subscribers")
			}
			close(model.resume)
			select {
			case <-model.stopped:
			case <-time.After(30 * time.Second):
				t.Fatal("the TUI did not receive stream completion")
			}
			if hidden {
				program.Send(tmuxVisibilityMsg{})
				programAck(t, program)
			}
			require.Contains(t, ansi.Strip(programFrame(t, program)), "FINAL-RESPONSE-AFTER-STALL")
		})
	}
}
