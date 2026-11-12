package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// stubDialog is a minimal dialog.Dialog that records when its size is set.
// We rely on the fact that the dialog.Manager calls SetSize on opening so
// that re-using the same instance is observable from the layer count.
type stubDialog struct {
	dialog.BaseDialog

	id string
}

func (s *stubDialog) Init() tea.Cmd { return nil }
func (s *stubDialog) Update(tea.Msg) (layout.Model, tea.Cmd) {
	return s, nil
}
func (s *stubDialog) View() string             { return "stub:" + s.id }
func (s *stubDialog) SetSize(w, h int) tea.Cmd { return s.BaseDialog.SetSize(w, h) }
func (s *stubDialog) Position() (row, col int) { return 0, 0 }

func TestReplayPendingEventRestoresAllStashedDialogs(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	first := &runtime.ElicitationRequestEvent{ElicitationID: "first"}
	second := &runtime.ElicitationRequestEvent{ElicitationID: "second"}
	one, two := &stubDialog{id: "one"}, &stubDialog{id: "two"}
	for _, entry := range []dialog.OpenDialogMsg{{Model: one, OriginatingEvent: first}, {Model: two, OriginatingEvent: second}} {
		_, _ = m.dialogMgr.Update(entry)
	}
	_, switchCmd := m.handleSpawnSession("/other")
	assert.False(t, m.dialogMgr.Open(), "all attention dialogs are parked synchronously")
	assert.False(t, hasMsg[dialog.CloseDialogMsg](collectMsgs(switchCmd)), "no late close can pop the new tab's dialog")
	_, _ = m.handleSwitchTab(id)
	opened := m.dialogMgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true })
	require.Len(t, opened, 2)
	assert.Same(t, first, opened[0].OriginatingEvent)
	assert.Same(t, one, opened[0].Model)
	assert.Same(t, second, opened[1].OriginatingEvent)
	assert.Same(t, two, opened[1].Model)
	assert.Nil(t, m.tabs[id].attentionDialogs)
	assert.Nil(t, m.replayPendingEvent(id), "replay must not duplicate already-consumed prompts")
}

func TestReplayPendingEventDiscardsStaleStash(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	id := m.supervisor.ActiveID()
	old := &runtime.ElicitationRequestEvent{}
	fresh := &runtime.ElicitationRequestEvent{}
	stashed := &stubDialog{id: "old"}
	m.activeTab.attentionDialogs = map[tea.Msg]dialog.Dialog{old: stashed}
	m.activeTab.state.Prepend(fresh)
	_ = m.replayPendingEvent(id)
	assert.Same(t, fresh, m.dialogMgr.TopBackgroundEvent())
	assert.NotSame(t, stashed, m.dialogMgr.TopDialog())
	assert.Nil(t, m.activeTab.attentionDialogs)
}

func TestReplayPendingEventNoPendingClearsStash(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	m.activeTab.attentionDialogs = map[tea.Msg]dialog.Dialog{&runtime.ElicitationRequestEvent{}: &stubDialog{}}
	assert.Nil(t, m.replayPendingEvent(m.supervisor.ActiveID()))
	assert.Nil(t, m.activeTab.attentionDialogs)
}
