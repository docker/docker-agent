package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func TestStartupBannerAnimationRootLifecycle(t *testing.T) {
	scheduler := &rootImmediateScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	root, _, _ := harnessRoot(t, 120, 40, ar)
	root.ready = false
	root.startupBannerAnimation = true
	_, cmd := root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	require.EqualValues(t, 1, ar.ActiveCount())
	frames := []string{root.View().Content}
	var tick animation.TickMsg
	for _, msg := range collectMsgs(cmd) {
		if v, ok := msg.(animation.TickMsg); ok {
			tick = v
		}
	}
	for i := 0; ar.HasActive(); i++ {
		require.Less(t, i, 100)
		_, next := root.Update(tick)
		frames = append(frames, root.View().Content)
		if !ar.HasActive() {
			break
		}
		found := false
		for _, msg := range collectMsgs(next) {
			if v, ok := msg.(animation.TickMsg); ok {
				tick, found = v, true
			}
		}
		require.True(t, found)
	}
	require.Greater(t, len(frames), 3)
	assert.NotEqual(t, frames[0], frames[1])
	assert.NotEqual(t, frames[0], frames[len(frames)-1])
	assert.Contains(t, frames[len(frames)-1], banner.Render()[0])
	_, _ = root.Update(tea.WindowSizeMsg{Width: 121, Height: 40})
	assert.EqualValues(t, 0, ar.ActiveCount(), "resize must not replay the animation")
}

func TestStartupBannerAnimationOnlyFirstEligibleSize(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		enabled, lean, hidden bool
		width                 int
	}{
		{"disabled", false, false, false, 120},
		{"lean", true, true, false, 120},
		{"hidden pane", true, false, true, 120},
		{"narrow", true, false, false, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
			root, _, _ := harnessRoot(t, 120, 40, ar)
			root.ready = false
			root.startupBannerAnimation, root.leanMode, root.paneHidden = tc.enabled, tc.lean, tc.hidden
			root.handleWindowResize(tc.width, 40)
			assert.EqualValues(t, 0, ar.ActiveCount())
			root.leanMode, root.paneHidden = false, false
			root.handleWindowResize(120, 40)
			assert.EqualValues(t, 0, ar.ActiveCount())
		})
	}
}

func TestStartupBannerAnimationCanceledOnTabSwitch(t *testing.T) {
	root := newTabLifecycleModel(t)
	root.activeTab.chatPage.SetShowBanner(true)
	root.handleWindowResize(120, 40)
	first := root.activeTab.chatPage
	require.NotNil(t, chat.StartStartupBanner(first))
	require.True(t, root.ar.HasActive())
	_, _ = root.handleSpawnSession("/second")
	assert.False(t, root.ar.HasActive())
}

func TestStartupBannerAnimationCanceledWhenPaneHidden(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	root, _, _ := harnessRoot(t, 120, 40, ar)
	require.NotNil(t, chat.StartStartupBanner(root.activeTab.chatPage))
	root.setPaneHidden(true)
	assert.EqualValues(t, 0, ar.ActiveCount())
	root.setPaneHidden(false)
	assert.EqualValues(t, 0, ar.ActiveCount())
}
