package tui

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

type delayedModelRuntime struct {
	stubRuntime

	started chan struct{}
	release chan struct{}
	lists   atomic.Int32
	err     error
}

func (*delayedModelRuntime) SupportsModelSwitching() bool { return true }
func (r *delayedModelRuntime) RefreshModelsCatalog(context.Context) error {
	close(r.started)
	<-r.release
	return r.err
}

func (r *delayedModelRuntime) AvailableModels(context.Context) []runtime.ModelChoice {
	r.lists.Add(1)
	return []runtime.ModelChoice{{Name: "original-model", Ref: "original-model"}}
}

func TestModelRefreshStaysWithOriginatingPage(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"none", "switch", "close", "clear", "restore", "reload-same", "switch-back"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			rt := &delayedModelRuntime{started: make(chan struct{}), release: make(chan struct{})}
			original := session.New(session.WithWorkingDir("/initial"))
			a := app.New(t.Context(), rt, original)
			id := m.supervisor.ActiveID()
			m.supervisor.ReplaceRunnerApp(t.Context(), id, a, "/initial", nil)
			m.application = a
			m.bindTabSession(id, original.ID)
			m.initSessionComponents(id, a, original)
			_, cmd := m.handleRefreshModelPicker("original")
			batch, ok := cmd().(tea.BatchMsg)
			require.True(t, ok)
			require.Len(t, batch, 2)
			done := make(chan tea.Msg, 1)
			go func() { done <- batch[1]() }()
			<-rt.started
			switch change {
			case "switch":
				_, _ = m.handleSpawnSession("/other")
			case "close":
				_, _ = m.handleCloseTab(id)
			case "clear":
				_, _ = m.handleClearSession()
			case "restore":
				_, _ = m.replaceActiveSession(t.Context(), session.New(session.WithWorkingDir("/initial")))
			case "reload-same":
				_, _ = m.replaceActiveSession(t.Context(), original)
			case "switch-back":
				_, _ = m.handleSpawnSession("/other")
				_, _ = m.handleSwitchTab(id)
			}
			close(rt.release)
			result := <-done
			assert.Equal(t, int32(1), rt.lists.Load(), "catalog lookup must use the captured app")
			_, effects := m.Update(result)
			if change == "none" || change == "switch-back" {
				var opened bool
				for _, event := range collectMsgs(effects) {
					scoped, ok := event.(modelPickerRefreshEffect)
					require.True(t, ok)
					if _, ok := scoped.inner.(dialog.OpenDialogMsg); ok {
						opened = true
					}
					_, _ = m.Update(event)
				}
				assert.True(t, opened)
				assert.True(t, m.dialogMgr.Open())
			} else {
				assert.Nil(t, effects, "stale or hidden refresh must not open a dialog or toast")
			}
		})
	}
}

func TestModelRefreshErrorIsScoped(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	rt := &delayedModelRuntime{started: make(chan struct{}), release: make(chan struct{}), err: errors.New("discovery failed")}
	m.application = app.New(t.Context(), rt, session.New())
	_, cmd := m.handleRefreshModelPicker("")
	batch := cmd().(tea.BatchMsg)
	close(rt.release)
	result := batch[1]()
	_, _ = m.handleSpawnSession("/other")
	_, effects := m.Update(result)
	assert.Nil(t, effects)
}

func TestModelRefreshEffectsRemainScoped(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	rt := &delayedModelRuntime{started: make(chan struct{}), release: make(chan struct{})}
	m.application = app.New(t.Context(), rt, session.New())
	_, cmd := m.handleRefreshModelPicker("")
	batch := cmd().(tea.BatchMsg)
	close(rt.release)
	_, effects := m.Update(batch[1]())
	require.NotNil(t, effects)
	_, _ = m.handleSpawnSession("/other")
	for _, event := range collectMsgs(effects) {
		_, next := m.Update(event)
		assert.Nil(t, next)
	}
	assert.False(t, m.dialogMgr.Open(), "a switch between refresh completion and dialog delivery must not leak the dialog")
}

func TestModelRefreshAfterLastTabCloseFailure(t *testing.T) {
	t.Parallel()
	m := newTabLifecycleModel(t)
	rt := &delayedModelRuntime{started: make(chan struct{}), release: make(chan struct{})}
	m.application = app.New(t.Context(), rt, session.New())
	_, cmd := m.handleRefreshModelPicker("")
	batch := cmd().(tea.BatchMsg)
	close(rt.release)
	result := batch[1]()
	// A failed replacement retains activeTab for display, but its runner and map entry are gone.
	id := m.supervisor.ActiveID()
	m.supervisor.CloseSession(id)
	delete(m.tabs, id)
	_, effects := m.Update(result)
	assert.Nil(t, effects)
}
