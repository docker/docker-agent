package tui

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestTerminalExecExcludesTimeAway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix true as a no-op editor and shell")
	}
	_, err := exec.LookPath("true")
	require.NoError(t, err)
	for _, name := range []string{"Ctrl+G while recording", "plan editor error", "shell in hidden pane"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("VISUAL", "true")
			m, _ := newTestModel(t)
			clock := &rootImmediateScheduler{now: time.Unix(1, 0)}
			m.ar = animation.NewRuntimeWithScheduler(clock)
			t.Cleanup(m.ar.Stop)
			m.tabBar = tabbar.New(m.ar, 0)
			ed := editor.New(nil, editor.WithAnimationRuntime(m.ar))
			m.activeTab.editor = ed
			_, pending := m.Update(ed.SetRecording(true)())
			require.Equal(t, animation.TickRate, m.ar.Now())

			var launch tea.Cmd
			switch name {
			case "Ctrl+G while recording":
				_, launch = m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
			case "plan editor error":
				dir := t.TempDir()
				t.Setenv("VISUAL", filepath.Join(dir, "missing-editor"))
				launch = m.execPlanEditor(planEditorClosedMsg{ref: plans.SharedRef("draft"), path: filepath.Join(dir, "draft.md")})
			case "shell in hidden pane":
				t.Setenv("SHELL", "true")
				m.supervisor = supervisor.New(nil)
				_, launch = m.Update(messages.StartShellMsg{})
				m.setPaneHidden(true)
			}
			require.NotNil(t, launch)
			queued := launch()
			_, unfiltered := m.Update(queued)
			require.NotNil(t, unfiltered, "hosts without a filter still dispatch the process")
			require.IsType(t, queued.(terminalExecMsg).exec, unfiltered())
			staleTick := pending()
			if !m.paneHidden {
				_, pending = m.Update(staleTick)
				require.Equal(t, 2*animation.TickRate, m.ar.Now(), "queuing the handoff must not pause the clock")
				staleTick = pending()
			}
			before := m.ar.Now()
			host := &terminalExecTestModel{root: m}
			filter := func(_ tea.Model, msg tea.Msg) tea.Msg {
				filtered := FilterTerminalHandoff(m, msg)
				if _, ok := msg.(terminalExecMsg); ok {
					host.handoff = true
					clock.now = clock.now.Add(1350 * time.Millisecond)
				}
				return filtered
			}
			host.launch = func() tea.Msg { return queued }
			host.waitForCallback = name != "shell in hidden pane"
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			program := tea.NewProgram(host, tea.WithContext(ctx), tea.WithFilter(filter), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
			_, err := program.Run()
			require.NoError(t, err)
			require.True(t, host.handoff)

			_, _ = m.Update(staleTick)
			require.Equal(t, before, m.ar.Now(), "a tick queued before handoff is stale")
			switch name {
			case "shell in hidden pane":
				assert.Nil(t, host.resume)
				assert.Nil(t, m.ar.Continue(), "returning to a hidden pane must not start ticks")
				host.resume = m.setPaneHidden(false)
			case "plan editor error":
				closed, ok := host.callback.(planEditorClosedMsg)
				require.True(t, ok)
				require.Error(t, closed.err)
			case "Ctrl+G while recording":
				assert.Equal(t, messages.RequestFocusMsg{Target: messages.PanelEditor}, host.callback)
			}
			require.NotNil(t, host.resume)
			_, _ = m.Update(host.resume())
			assert.Equal(t, before+animation.TickRate, m.ar.Now(), "the first tick after handoff excludes terminal-away time")
			assert.True(t, ed.IsRecording())
		})
	}
}

// Keep the real Bubble Tea exec path but leave unrelated startup messages out
// of the minimal root model. Run resume ticks only after the queued stale tick.
type terminalExecTestModel struct {
	root            *appModel
	launch, resume  tea.Cmd
	handoff         bool
	waitForCallback bool
	callback        tea.Msg
}

func (m *terminalExecTestModel) Init() tea.Cmd  { return m.launch }
func (m *terminalExecTestModel) View() tea.View { return tea.View{} }
func (m *terminalExecTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.handoff && m.root.terminalExecInProgress {
		_, m.resume = m.root.Update(msg)
		if !m.waitForCallback {
			return m, tea.Quit
		}
	}
	switch msg.(type) {
	case messages.RequestFocusMsg, planEditorClosedMsg:
		m.callback = msg
		return m, tea.Quit
	}
	return m, nil
}
