package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/banner"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type bannerScheduler struct{ now time.Time }

func (s *bannerScheduler) Now() time.Time { return s.now }
func (s *bannerScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(d); return f(s.now) }
}

func bannerPage(t *testing.T) (*chatPage, *animation.Runtime) {
	t.Helper()
	sess := session.New()
	a := app.New(t.Context(), &queueTestRuntime{}, sess)
	ar := animation.NewRuntimeWithScheduler(&bannerScheduler{now: time.Unix(1, 0)})
	p := New(ar, t.Context(), a, service.NewSessionState(sess), WithHideSidebar()).(*chatPage)
	p.SetSize(100, 20)
	t.Cleanup(func() { Cleanup(p) })
	return p, ar
}

func TestStartupBannerColorsSurviveChatLayout(t *testing.T) {
	previous := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(previous) })
	for _, name := range []string{"default-light", "default"} {
		t.Run(name, func(t *testing.T) {
			theme, err := styles.LoadTheme(name)
			require.NoError(t, err)
			styles.ApplyTheme(theme)
			p, ar := bannerPage(t)
			assertColors := func(lines []string) {
				t.Helper()
				want := uv.NewStyledString(strings.Join(lines, "\n")).Lines(ansi.GraphemeWidth)
				got := uv.NewStyledString(p.View()).Lines(ansi.GraphemeWidth)
				for _, row := range want {
					for _, expected := range row {
						if strings.TrimSpace(expected.String()) == "" {
							continue
						}
						found := false
						for _, gotRow := range got {
							for _, cell := range gotRow {
								if cell.String() == expected.String() && cell.Style.Fg != nil && expected.Style.Fg != nil && cell.Style.Fg == expected.Style.Fg {
									found = true
									break
								}
							}
						}
						assert.True(t, found, "missing cell %q with style %+v", expected.String(), expected.Style)
					}
				}
			}
			assertColors(banner.Render())
			cmd := StartStartupBanner(p)
			require.NotNil(t, cmd)
			for p.bannerTransition.Progress() < 0.7 {
				tick, ok := ar.Accept(cmd().(animation.TickMsg))
				require.True(t, ok)
				_, _ = p.Update(tick)
				cmd = ar.Continue()
				require.NotNil(t, cmd)
			}
			assertColors(banner.RenderFrame(p.bannerTransition.Progress(), p.computeSidebarLayout().chatWidth))
		})
	}
}

func TestStartupBannerAnimationFinishesAndUnregisters(t *testing.T) {
	p, ar := bannerPage(t)
	static := p.messagesView(p.computeSidebarLayout())
	cmd := StartStartupBanner(p)
	require.NotNil(t, cmd)
	require.EqualValues(t, 1, ar.ActiveCount())
	first := p.messagesView(p.computeSidebarLayout())
	require.NotEqual(t, static, first)
	var frames int
	for ar.HasActive() {
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		_, _ = p.Update(tick)
		require.True(t, tick.Dirty())
		cmd = ar.Continue()
		frames++
		require.Less(t, frames, 100)
	}
	assert.Greater(t, frames, 2)
	assert.Equal(t, static, p.messagesView(p.computeSidebarLayout()))
	assert.Equal(t, 2*time.Second, p.bannerTransition.Duration())
	assert.EqualValues(t, 0, ar.ActiveCount())
}

func TestStartupBannerAnimationCancelsOnHide(t *testing.T) {
	cases := []struct {
		name string
		hide func(*chatPage)
	}{
		{"setting", func(p *chatPage) { p.SetShowBanner(false) }},
		{"resize width", func(p *chatPage) { p.SetSize(banner.Width-1, 20) }},
		{"resize height", func(p *chatPage) { p.SetSize(100, banner.Height-1) }},
		{"user message", func(p *chatPage) { p.Update(runtime.UserMessage("hello", "", nil)) }},
		{"welcome message", func(p *chatPage) { p.Update(runtime.AgentInfo("root", "", "", "welcome")) }},
		{"assistant content", func(p *chatPage) { p.Update(runtime.AgentChoice("root", "", "hello")) }},
		{"tab or pane hidden", func(p *chatPage) { CancelStartupBanner(p) }},
		{"cleanup", func(p *chatPage) { Cleanup(p) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p, ar := bannerPage(t)
			tickCmd := StartStartupBanner(p)
			require.NotNil(t, tickCmd)
			tt.hide(p)
			assert.False(t, p.bannerTransition.Running())
			assert.EqualValues(t, 0, ar.ActiveCount())
			_, ok := ar.Accept(tickCmd().(animation.TickMsg))
			assert.False(t, ok, "stale tick must not restart the animation")
		})
	}
}

func TestStartupBannerNotStartedWhenNotVisible(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*chatPage)
	}{
		{"hidden", func(p *chatPage) { p.SetShowBanner(false) }},
		{"too narrow", func(p *chatPage) { p.SetSize(banner.Width-1, 20) }},
		{"too short", func(p *chatPage) { p.SetSize(100, banner.Height-1) }},
		{"welcome", func(p *chatPage) { p.Update(runtime.AgentInfo("root", "", "", "welcome")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ar := bannerPage(t)
			tc.setup(p)
			assert.Nil(t, StartStartupBanner(p))
			assert.EqualValues(t, 0, ar.ActiveCount())
		})
	}
}

func TestBannerPointerInteraction(t *testing.T) {
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight, msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			p, ar := bannerPage(t)
			p.bannerInteractive = true
			p.hideSidebar = false
			p.layoutSettings.SidebarPosition = position
			p.SetSize(160, 35)
			sl := p.computeSidebarLayout()
			chatY := 0
			if sl.mode != sidebarVertical && !sl.bandAtBottom {
				chatY = sl.sidebarHeight
			}
			x := styles.AppPadding + sl.chatStartX + sl.chatWidth/2
			y := chatY + (sl.chatHeight-banner.Height)/2 + 2
			static := p.View()
			_, cmd := p.Update(tea.MouseMotionMsg{X: x, Y: y})
			require.NotNil(t, cmd)
			tick, ok := ar.Accept(cmd().(animation.TickMsg))
			require.True(t, ok)
			_, _ = p.Update(tick)
			assert.NotEqual(t, static, p.View())
			require.True(t, p.bannerMotion.IsActive())
			LeaveBanner(p)
			for range 100 {
				cmd = ar.Continue()
				if cmd == nil {
					break
				}
				tick, ok = ar.Accept(cmd().(animation.TickMsg))
				require.True(t, ok)
				_, _ = p.Update(tick)
			}
			assert.False(t, p.bannerMotion.IsActive())
			assert.Equal(t, static, p.View())
		})
	}
}
