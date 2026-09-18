package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type asyncQueueRuntime struct {
	stubRuntime

	started chan struct{}
	release chan struct{}
	runs    chan string
	err     error
	store   session.Store
}

func (r *asyncQueueRuntime) SessionStore() session.Store { return r.store }
func (r *asyncQueueRuntime) enqueue() error              { close(r.started); <-r.release; return r.err }

func (r *asyncQueueRuntime) Steer(context.Context, runtime.QueuedMessage) error { return r.enqueue() }

func (r *asyncQueueRuntime) FollowUp(context.Context, runtime.QueuedMessage) error {
	return r.enqueue()
}

func (r *asyncQueueRuntime) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	r.runs <- sess.ID
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func TestAsyncInputResultStaysOnOriginTab(t *testing.T) {
	t.Parallel()
	for _, followUp := range []bool{false, true} {
		for _, outcome := range []string{"success", "failure-busy", "failure-idle"} {
			name := "steer/" + outcome
			if followUp {
				name = "follow-up/" + outcome
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				m := newTabLifecycleModel(t)
				rt := &asyncQueueRuntime{started: make(chan struct{}), release: make(chan struct{}), runs: make(chan string, 1)}
				if outcome != "success" {
					rt.err = errors.New("queue rejected")
				}
				sess := session.New(session.WithWorkingDir("/initial"))
				a := app.New(t.Context(), rt, sess)
				tabID := m.supervisor.ActiveID()
				m.supervisor.ReplaceRunnerApp(t.Context(), tabID, a, "/initial", nil)
				m.application = a
				m.bindTabSession(tabID, sess.ID)
				m.initSessionComponents(tabID, a, sess)
				origin := m.activeTab
				_, _ = origin.chatPage.Update(&runtime.StreamStartedEvent{SessionID: sess.ID})
				_, cmd := m.Update(messages.SendMsg{Content: "original input", FollowUp: followUp})
				done := make(chan tea.Msg, 1)
				go func() { done <- cmd() }()
				select {
				case <-rt.started:
				case <-time.After(5 * time.Second):
					t.Fatal("enqueue never started")
				}
				_, _ = m.handleSpawnSession("/other")
				other := m.activeTab
				if outcome == "failure-idle" {
					_, _ = origin.chatPage.Update(&runtime.StreamStoppedEvent{SessionID: sess.ID})
				}
				close(rt.release)
				result := <-done
				_, effects := m.Update(result)
				assert.Nil(t, effects, "hidden input results must not affect active chrome")
				assert.Zero(t, other.chatPage.QueueLength())
				assert.False(t, other.chatPage.IsWorking())
				if outcome == "failure-busy" {
					assert.Equal(t, 1, origin.chatPage.QueueLength())
				}
				if outcome == "failure-idle" {
					select {
					case got := <-rt.runs:
						assert.Equal(t, sess.ID, got)
					case <-time.After(5 * time.Second):
						t.Fatal("hidden fallback did not run")
					}
				}
			})
		}
	}
}

func TestAsyncInputResultAfterTabLifecycleChange(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"close", "clear", "restore", "reload-same", "restore-back", "branch", "switch-back"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			rt := &asyncQueueRuntime{started: make(chan struct{}), release: make(chan struct{}), runs: make(chan string, 1), err: errors.New("rejected")}
			original := session.New(session.WithWorkingDir("/initial"))
			rt.store = session.NewInMemorySessionStore()
			require.NoError(t, rt.store.AddSession(t.Context(), original))
			a := app.New(t.Context(), rt, original)
			id := m.supervisor.ActiveID()
			m.supervisor.ReplaceRunnerApp(t.Context(), id, a, "/initial", nil)
			m.application = a
			m.bindTabSession(id, original.ID)
			m.initSessionComponents(id, a, original)
			_, _ = m.activeTab.chatPage.Update(&runtime.StreamStartedEvent{SessionID: original.ID})
			_, cmd := m.Update(messages.SendMsg{Content: "old input"})
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			<-rt.started
			close(rt.release)
			result := <-done // completed, but not yet applied by Update
			switch change {
			case "close":
				_, _ = m.handleSpawnSession("/other")
				_, _ = m.handleCloseTab(id)
			case "clear":
				_, _ = m.handleClearSession()
			case "restore":
				_, _ = m.replaceActiveSession(t.Context(), session.New(session.WithWorkingDir("/initial")))
			case "reload-same":
				_, _ = m.replaceActiveSession(t.Context(), original)
			case "restore-back":
				_, _ = m.replaceActiveSession(t.Context(), session.New(session.WithWorkingDir("/initial")))
				_, _ = m.replaceActiveSession(t.Context(), original)
			case "branch":
				_, _ = m.handleBranchFromEdit(messages.BranchFromEditMsg{ParentSessionID: original.ID, BranchAtPosition: 0, Content: "branched"})
			case "switch-back":
				_, _ = m.handleSpawnSession("/other")
				_, _ = m.handleSwitchTab(id)
			}
			_, effects := m.Update(result)
			if change == "switch-back" {
				assert.Equal(t, 1, m.activeTab.chatPage.QueueLength())
				require.NotNil(t, effects)
			} else {
				assert.Zero(t, m.activeTab.chatPage.QueueLength())
				assert.False(t, m.activeTab.chatPage.IsWorking())
				assert.Nil(t, effects)
			}
		})
	}
}

func TestHiddenTabDispatchesAsyncSubmission(t *testing.T) {
	t.Parallel()
	for _, followUp := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "follow-up"}[followUp], func(t *testing.T) {
			t.Parallel()
			m := newTabLifecycleModel(t)
			rt := &asyncQueueRuntime{started: make(chan struct{}), release: make(chan struct{}), runs: make(chan string, 1)}
			sess := session.New(session.WithWorkingDir("/initial"))
			a := app.New(t.Context(), rt, sess)
			id := m.supervisor.ActiveID()
			m.supervisor.ReplaceRunnerApp(t.Context(), id, a, "/initial", nil)
			m.application = a
			m.bindTabSession(id, sess.ID)
			m.initSessionComponents(id, a, sess)
			origin := m.activeTab
			_, _ = origin.chatPage.Update(&runtime.StreamStartedEvent{SessionID: sess.ID})
			_, _ = m.handleSpawnSession("/other")

			_, cmd := m.Update(messages.RoutedMsg{SessionID: id, Inner: messages.SendMsg{Content: "hidden input", FollowUp: followUp}})
			require.NotNil(t, cmd, "submission is local work, not a visible UI effect")
			close(rt.release)
			result := cmd()
			select {
			case <-rt.started:
			default:
				t.Fatal("hidden submission was discarded")
			}
			_, effects := m.Update(result)
			assert.Nil(t, effects, "submission feedback must not affect the visible tab")
			assert.False(t, m.activeTab.chatPage.IsWorking())
			assert.Zero(t, m.activeTab.chatPage.QueueLength())
		})
	}
}
