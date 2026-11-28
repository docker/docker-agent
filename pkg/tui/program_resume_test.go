package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
)

func TestResumeClearOnlyOnIntentionalExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		msg      any
		canceled bool
		want     int
	}{
		{"confirmed", dialog.ExitConfirmedMsg{}, false, 1},
		{"slash exit", messages.ExitSessionMsg{}, false, 1},
		{"canceled", dialog.ExitConfirmedMsg{}, true, 0},
		{"automatic exit", messages.ExitAfterFirstResponseMsg{}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t)
			neutralizeExitFunc(t, m)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.shutdownDone = ctx.Done()
			if tc.canceled {
				cancel()
			}
			cleared := 0
			m.resumeClear = func() { cleared++ }
			m.Update(tc.msg)
			assert.Equal(t, tc.want, cleared)
		})
	}
}

func TestProgramResumeReportsAfterContinue(t *testing.T) {
	m := newTabLifecycleModel(t)
	continued, updates := 0, 0
	m.resumeContinued = func() { continued++ }
	m.resumeUpdate = func(_ *app.App, _ bool) {
		assert.Equal(t, 1, continued)
		updates++
	}
	m.Update(tea.ResumeMsg{})
	assert.Equal(t, 1, updates)
}

func TestProgramResumeTracksActiveTabAndClear(t *testing.T) {
	m := newTabLifecycleModel(t)
	var ids []string
	m.resumeUpdate = func(a *app.App, pending bool) {
		require.False(t, pending)
		ids = append(ids, a.Session().ID)
	}
	first := m.application.Session().ID
	m.Update(messages.SpawnSessionMsg{WorkingDir: "/second"})
	second := m.application.Session().ID
	require.NotEqual(t, first, second)
	assert.Equal(t, second, ids[len(ids)-1])
	m.Update(messages.SwitchTabMsg{SessionID: first})
	assert.Equal(t, first, ids[len(ids)-1])
	m.Update(messages.CloseTabMsg{SessionID: first})
	assert.Equal(t, second, ids[len(ids)-1])
	m.Update(messages.ClearSessionMsg{})
	assert.NotEqual(t, second, ids[len(ids)-1])
}

func TestExplicitSessionDoesNotReplaceSavedTabs(t *testing.T) {
	old := paths.GetDataDir()
	paths.SetDataDir(t.TempDir())
	defer paths.SetDataDir(old)
	ts, err := tuistate.New(t.Context())
	require.NoError(t, err)
	defer ts.Close()
	require.NoError(t, ts.AddTab(t.Context(), "saved", "/tmp"))
	require.NoError(t, ts.SetActiveTab(t.Context(), "saved"))
	m := newTabLifecycleModel(t)
	m.explicitSession = true
	m.tuiStore = ts
	m.restoreTabs(t.Context(), ts, m.supervisor, nil, m.application, m.supervisor.ActiveID(), "/tmp")
	tabs, active, err := ts.GetTabs(t.Context())
	require.NoError(t, err)
	require.Len(t, tabs, 1)
	assert.Equal(t, "saved", tabs[0].SessionID)
	assert.Equal(t, "saved", active)
	assert.Nil(t, m.activeTab.pendingRestore)
}
