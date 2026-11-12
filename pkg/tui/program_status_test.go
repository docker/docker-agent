package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/programstatus"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func statusOutput(cmd tea.Cmd) string {
	var output strings.Builder
	for _, msg := range collectMsgs(cmd) {
		if raw, ok := msg.(tea.RawMsg); ok {
			fmt.Fprint(&output, raw.Msg)
		}
	}
	return output.String()
}

func TestProgramStatusLifecycle(t *testing.T) {
	t.Parallel()
	for _, lean := range []bool{false, true} {
		t.Run(fmt.Sprintf("lean=%t", lean), func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			m.leanMode = lean
			m.programStatus = &programstatus.Reporter{}
			id, root := m.supervisor.ActiveID(), m.application.Session().ID
			assert.Contains(t, statusOutput(m.refreshProgramStatus()), "state=idle")
			_, cmd := m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStarted(root, "root")))
			assert.Contains(t, statusOutput(cmd), "state=working")
			_, cmd = m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStopped("child", "child", "normal")))
			assert.NotContains(t, statusOutput(cmd), "state=done")
			_, cmd = m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStopped(root, "root", "normal")))
			assert.Contains(t, statusOutput(cmd), "state=done")
			_, cmd = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			assert.Contains(t, statusOutput(cmd), "state=idle")
		})
	}
}

func TestProgramStatusBackgroundPrompt(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	id, root := m.supervisor.ActiveID(), m.application.Session().ID
	_, _ = m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStarted(root, "root")))
	_, _ = m.handleSpawnSession("/other")
	otherID := m.supervisor.ActiveID()
	_, _ = m.Update(queuedRuntimeDelivery(m, otherID, runtime.StreamStarted(m.application.Session().ID, "other")))
	prompt := &runtime.ElicitationRequestEvent{SessionID: root, ElicitationID: "question"}
	_, cmd := m.Update(queuedRuntimeDelivery(m, id, prompt))
	assert.Contains(t, statusOutput(cmd), "state=blocked")
	assert.Equal(t, "question", m.tabs[id].programStatus.Status().Kind)
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	assert.NotContains(t, statusOutput(cmd), "state=idle", "typing in another tab cannot retire the prompt")
	_, _ = m.Update(messages.SwitchTabMsg{SessionID: id})
	require.Same(t, prompt, m.dialogMgr.TopBackgroundEvent())
	_, cmd = m.Update(dialog.CloseDialogMsg{})
	assert.Contains(t, statusOutput(cmd), "state=working")
}

func TestProgramStatusRetirementAndStaleDelivery(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	id := m.supervisor.ActiveID()
	prompt := &runtime.ElicitationRequestEvent{ElicitationID: "old"}
	queued := queuedRuntimeDelivery(m, id, prompt)
	_, cmd := m.Update(queued)
	assert.Contains(t, statusOutput(cmd), "state=blocked")
	_, cmd = m.Update(messages.ClearSessionMsg{})
	assert.Contains(t, statusOutput(cmd), "state=idle")
	_, cmd = m.Update(queued)
	assert.Empty(t, statusOutput(cmd))
}

func TestProgramStatusLatestWriteAndExit(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	id, root := m.supervisor.ActiveID(), m.application.Session().ID
	_, start := m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStarted(root, "root")))
	_, stop := m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStopped(root, "root", "error")))
	assert.Contains(t, statusOutput(start), "state=error", "even an older command publishes the latest status")
	assert.Empty(t, statusOutput(stop))
	// Supervisor shutdown may already have removed its runners before quitCmd.
	m.supervisor.Shutdown()
	assert.NotContains(t, statusOutput(m.quitCmd()), "state=idle", "don't overwrite an unseen failure on exit")
	assert.Contains(t, m.activeTab.programStatus.Status().Sequence(), "state=error")
}

func TestProgramStatusDisabled(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	assert.Nil(t, m.refreshProgramStatus())
	id := m.supervisor.ActiveID()
	_, cmd := m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStarted(m.application.Session().ID, "root")))
	assert.NotContains(t, statusOutput(cmd), "]7501;")
}

func TestProgramStatusDetection(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{false, true} {
		m := newTabLifecycleModel(t)
		WithProgramStatusProbe(&programstatus.Reporter{})(m)
		assert.Nil(t, m.refreshProgramStatus())
		id, root := m.supervisor.ActiveID(), m.application.Session().ID
		_, cmd := m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStarted(root, "root")))
		assert.Empty(t, statusOutput(cmd), "no reporting before feature detection")
		if supported {
			_, cmd = m.Update(uv.UnknownOscEvent("\x1b]7501;?:future=1\a"))
			assert.Contains(t, statusOutput(cmd), "state=working")
			_, cmd = m.Update(programStatusTimeoutMsg{})
			assert.Empty(t, statusOutput(cmd))
			assert.False(t, m.programStatusUnsupported)
		} else {
			_, cmd = m.Update(programStatusTimeoutMsg{})
			assert.Empty(t, statusOutput(cmd))
			assert.Nil(t, m.retireProgramStatus())
			var output strings.Builder
			m.programStatus.Finish(&output)
			assert.Empty(t, output.String())
		}
	}
}

func TestProgramStatusAcceptedOperation(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	_, cmd := m.Update(messages.SendMsg{Content: "Work before the first stream event"})
	assert.Contains(t, statusOutput(cmd), "state=working")
	assert.Equal(t, "working", m.activeTab.programStatus.Status().State)
	m.activeTab.chatPage.SetInterruptMode(messages.InterruptModeNone)
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, "idle", m.activeTab.programStatus.Status().State)
	id, root := m.supervisor.ActiveID(), m.application.Session().ID
	_, cmd = m.Update(queuedRuntimeDelivery(m, id, runtime.StreamStopped(root, "root", "normal")))
	assert.NotContains(t, statusOutput(cmd), "state=done")
}

func TestProgramStatusClosesDetachedPrompt(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	id := m.supervisor.ActiveID()
	prompt := &runtime.ElicitationRequestEvent{SessionID: "job", ElicitationID: "pending"}
	_, _ = m.Update(queuedRuntimeDelivery(m, id, prompt))
	assert.Equal(t, "blocked", m.activeTab.programStatus.Status().State)
	require.True(t, m.dialogMgr.Open())
	_, cmd := m.Update(queuedRuntimeDelivery(m, id, &runtime.ElicitationClosedEvent{SessionID: "job", ElicitationID: "pending"}))
	assert.Contains(t, statusOutput(cmd), "state=idle")
	assert.False(t, m.dialogMgr.Open())
}

func TestProgramStatusIgnoresReplacedOperationStop(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	root := m.application.Session().ID
	m.application.BeginOperation(t.Context())
	first := &runtime.StreamStartedEvent{AgentContext: runtime.AgentContext{OperationID: 1}, SessionID: root}
	_, _ = m.Update(first)
	m.activeTab.programStatus.Cancel(root)
	m.application.BeginOperation(t.Context())
	m.activeTab.programStatus.Start(root)
	_, cmd := m.Update(&runtime.StreamStoppedEvent{AgentContext: runtime.AgentContext{OperationID: 1}, SessionID: root, Reason: "canceled"})
	assert.Nil(t, cmd)
	assert.Equal(t, "working", m.activeTab.programStatus.Status().State)
	_, _ = m.Update(&runtime.StreamStartedEvent{AgentContext: runtime.AgentContext{OperationID: 2}, SessionID: root})
	assert.Equal(t, "working", m.activeTab.programStatus.Status().State)
}

func TestProgramStatusScopedCloseAfterClosure(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.programStatus = &programstatus.Reporter{}
	id := m.supervisor.ActiveID()
	for _, name := range []string{"A", "B"} {
		_, _ = m.Update(queuedRuntimeDelivery(m, id, &runtime.ElicitationRequestEvent{ElicitationID: name, SessionID: "job"}))
	}
	_, _ = m.Update(queuedRuntimeDelivery(m, id, &runtime.ElicitationClosedEvent{ElicitationID: "B", SessionID: "job"}))
	_, _ = m.Update(dialog.CloseDialogMsg{ElicitationID: "B"})
	require.NotNil(t, m.dialogMgr.TopBackgroundEvent())
	assert.Equal(t, "A", m.dialogMgr.TopBackgroundEvent().(*runtime.ElicitationRequestEvent).ElicitationID)
	assert.Equal(t, "blocked", m.activeTab.programStatus.Status().State)
}
