package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestTmuxVisibilityHidden(t *testing.T) {
	t.Parallel()
	tty, err := os.Create(filepath.Join(t.TempDir(), "tty"))
	require.NoError(t, err)
	defer tty.Close()
	otherTTY := filepath.Join(t.TempDir(), "other-tty")
	require.NoError(t, os.WriteFile(otherTTY, nil, 0o600))
	for _, tt := range []struct {
		name, output string
		hidden       bool
	}{
		{"detached or background window", "0 0 1", true},
		{"inactive split pane", "1 0 0", false},
		{"active pane", "1 0 1", false},
		{"linked window with multiple clients", "2 0 0", false},
		{"zoomed sibling", "1 1 0", true},
		{"zoomed pane", "1 1 1", false},
		{"detached zoomed pane", "0 1 1", true},
		{"missing format", "0 1", false},
		{"invalid clients", "unknown 0 1", false},
		{"negative clients", "-1 0 1", false},
		{"invalid zoom", "0 unknown 1", false},
		{"invalid active pane", "0 0 unknown", false},
		{"empty", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.hidden, tmuxVisibilityHidden(tt.output+" "+tty.Name()+"\n", tty))
		})
	}
	for _, tt := range []struct {
		name, clients string
		hidden        bool
	}{
		{"inactive tree preview", "0 copy-mode tree-mode\n", false},
		{"other client's preview", "0\n0 tree-mode\n", false},
		{"client preview", "0 client-mode\n", false},
		{"ordinary modes", "0 copy-mode view-mode buffer-mode\n", true},
		{"unknown mode", "0 future-preview-mode\n", false},
		{"empty client record", "\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.hidden, tmuxVisibilityHidden("0 0 1 "+tty.Name()+"\n"+tt.clients, tty))
		})
	}
	assert.True(t, tmuxVisibilityHidden("1 1 0 "+tty.Name()+"\n0\n0\n", tty), "ordinary clients respect zoom")
	assert.False(t, tmuxVisibilityHidden("0 0 1 "+tty.Name()+"\n0\n1\n", tty), "control clients may display noncurrent windows")
	assert.False(t, tmuxVisibilityHidden("1 1 0 "+tty.Name()+"\n1\n", tty), "control clients may display zoomed siblings")
	assert.False(t, tmuxVisibilityHidden("0 0 1 "+tty.Name()+"\nunknown\n", tty), "unknown client modes fail open")
	assert.False(t, tmuxVisibilityHidden("0 0 1 "+otherTTY, tty), "inherited tmux env must not hide another terminal")
	assert.False(t, tmuxVisibilityHidden("0 0 1 /missing/tty", tty))
}

func TestTmuxVisibilityProbeFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	assert.False(t, tmuxPaneHidden(t.Context(), "%0", os.Stdout))
}

func TestTmuxVisibilityProbeFailureResumesHiddenPane(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{})
	t.Cleanup(root.ar.Stop)
	root.tmuxVisibilityProbe = func(ctx context.Context) bool {
		return tmuxPaneHidden(ctx, "%0", os.Stdout)
	}
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	sub := root.ar.Subscribe()
	t.Cleanup(sub.Stop)
	require.Nil(t, sub.Start())
	_, cmd := root.Update(root.checkTmuxVisibilityCmd()())
	require.False(t, root.paneHidden)
	msgs := collectMsgs(cmd)
	require.True(t, hasMsg[animation.TickMsg](msgs))
	require.True(t, hasMsg[tmuxVisibilityPollMsg](msgs))
}

func TestNoTmuxSkipsVisibilityPolling(t *testing.T) {
	for _, env := range []struct{ tmux, pane string }{{"", ""}, {"", "%0"}, {"/unused,1,0", ""}} {
		t.Setenv("TMUX", env.tmux)
		t.Setenv("TMUX_PANE", env.pane)
		root, _ := newTestModel(t)
		assert.Nil(t, root.initTmuxVisibilityCmd())
		assert.Nil(t, root.tmuxVisibilityProbe)
	}
}

func TestTmuxVisibilityProbeWiredIntoInit(t *testing.T) {
	t.Setenv("TMUX", "/unused,1,0")
	t.Setenv("TMUX_PANE", "%0")
	root, _, _ := frozenClockRoot(t, 120, 40)
	require.NotNil(t, root.tmuxVisibilityProbe, "harness calls the production Init")
	assert.Nil(t, root.initTmuxVisibilityCmd(), "do not start a second poll chain")
}

func TestTmuxVisibilityPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root, _ := newTestModel(t)
		root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{})
		t.Cleanup(root.ar.Stop)
		page := &countingChatPage{text: "stable", dirtyTick: true}
		root.activeTab.chatPage = page
		root.activeTab.sessionState = &service.SessionState{}
		root.ready, root.leanMode = true, true
		before := root.View()
		calls, hidden := 0, true
		root.tmuxVisibilityProbe = func(ctx context.Context) bool {
			calls++
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.Equal(t, time.Second, time.Until(deadline))
			return hidden
		}

		probe := root.checkTmuxVisibilityCmd()
		require.Zero(t, calls, "subprocess work must stay outside Update")
		_, poll := root.Update(probe())
		require.Equal(t, 1, calls)
		require.True(t, root.paneHidden, "detached startup needs no BlurMsg")
		require.Equal(t, before, root.View())
		sub := root.ar.Subscribe()
		t.Cleanup(sub.Stop)
		require.Nil(t, sub.Start(), "new hidden animations retain subscriptions without ticks")
		require.Nil(t, root.ar.EnsureRunning())

		// Visibility returns through the poll, without a FocusMsg.
		hidden = false
		require.NotNil(t, poll)
		_, probe = root.Update(poll())
		require.Equal(t, 1, calls)
		_, cmd := root.Update(probe())
		require.Equal(t, 2, calls)
		require.False(t, root.paneHidden)
		require.Equal(t, before, root.View())
		require.Equal(t, 2, page.views)
		msgs := collectMsgs(cmd)
		require.True(t, hasMsg[animation.TickMsg](msgs), "visibility resumes animations")
		require.True(t, hasMsg[tmuxVisibilityPollMsg](msgs), "continue polling when visible")

		_, _ = root.Update(tmuxVisibilityMsg{})
		require.True(t, root.viewCacheValid, "unchanged polls do not compose an idle frame")
		require.Equal(t, before, root.View())
		require.Equal(t, 2, page.views)
	})
}

func TestTmuxVisibilityFocusOverridesStaleProbe(t *testing.T) {
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{})
	t.Cleanup(root.ar.Stop)
	root.tmuxVisibilityProbe = func(context.Context) bool { return true }
	stale := root.checkTmuxVisibilityCmd()
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	_, _ = root.Update(tea.FocusMsg{})
	require.False(t, root.paneHidden)
	_, poll := root.Update(stale())
	require.False(t, root.paneHidden, "late snapshot cannot freeze a newly visible pane")
	require.NotNil(t, poll, "stale results must not stop polling")

	// A later probe can hide even a focused terminal: focus-events may be off.
	_, _ = root.Update(root.checkTmuxVisibilityCmd()())
	require.True(t, root.paneHidden)
	_, _ = root.Update(tea.FocusMsg{})
	require.False(t, root.paneHidden, "even duplicate focus proves visibility")
}

func TestTmuxHiddenRejectsQueuedAnimationTick(t *testing.T) {
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{})
	t.Cleanup(root.ar.Stop)
	sub := root.ar.Subscribe()
	t.Cleanup(sub.Stop)
	queued := sub.Start()
	require.NotNil(t, queued)
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	root.viewCacheValid = true
	_, cmd := root.Update(queued())
	assert.Nil(t, cmd)
	assert.True(t, root.viewCacheValid)
	assert.Zero(t, root.ar.Now())
}

func TestTmuxVisibilityQuitIgnoresPendingProbe(t *testing.T) {
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntime()
	t.Cleanup(root.ar.Stop)
	root.tmuxVisibilityProbe = func(context.Context) bool { return true }
	probe := root.checkTmuxVisibilityCmd()
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	_ = root.quitCmd()
	_, cmd := root.Update(probe())
	assert.Nil(t, cmd)
	assert.False(t, root.paneHidden)
	_, cmd = root.Update(tmuxVisibilityPollMsg{})
	assert.Nil(t, cmd)
}
