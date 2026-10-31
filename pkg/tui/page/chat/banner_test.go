package chat

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type bannerScheduler struct{ now time.Time }

func (s *bannerScheduler) Now() time.Time { return s.now }
func (s *bannerScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	s.now = s.now.Add(d)
	delivered := s.now
	return func() tea.Msg { return f(delivered) }
}

func newBannerTestPage(t *testing.T, ar *animation.Runtime, art string) *chatPage {
	t.Helper()
	b, err := tuibanner.Parse(art)
	require.NoError(t, err)
	p := New(ar, t.Context(), nil, &service.SessionState{}, WithBanner(b)).(*chatPage)
	p.SetSize(160, 40)
	return p
}

func bannerText(p *chatPage) string {
	return ansi.Strip(p.messagesView(p.computeSidebarLayout()))
}

func TestCustomBannerReplacesTheDefault(t *testing.T) {
	t.Parallel()

	p := newBannerTestPage(t, animation.NewRuntime(), "my project")

	assert.False(t, p.bannerPlaying())
	assert.Contains(t, bannerText(p), "my project")
	assert.NotContains(t, bannerText(p), tuibanner.Lines[0])
}

func TestAnimatedBannerAdvancesWithTheAnimationClock(t *testing.T) {
	t.Parallel()

	ar := animation.NewRuntimeWithScheduler(&bannerScheduler{now: time.Unix(1, 0)})
	p := newBannerTestPage(t, ar, "first\n\f\nsecond\n")
	require.True(t, p.bannerPlaying())
	assert.Contains(t, bannerText(p), "first")

	cmd := p.bannerSub.Start()
	require.NotNil(t, cmd)
	redraws := 0
	for ar.Now() < tuibanner.FrameDuration {
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		p.tickBanner(tick)
		if tick.Dirty() {
			redraws++
		}
		cmd = ar.Continue()
		require.NotNil(t, cmd)
	}

	assert.Equal(t, 1, redraws, "only a frame change redraws")
	assert.Contains(t, bannerText(p), "second")
}

func TestAnimatedBannerStopsTickingOnceTheConversationStarts(t *testing.T) {
	t.Parallel()

	ar := animation.NewRuntimeWithScheduler(&bannerScheduler{now: time.Unix(1, 0)})
	p := newBannerTestPage(t, ar, "first\n\f\nsecond\n")
	cmd := p.bannerSub.Start()
	require.Equal(t, int32(1), ar.ActiveCount())

	p.showStartupBanner = false
	tick, ok := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	p.tickBanner(tick)

	assert.False(t, tick.Dirty())
	assert.Zero(t, ar.ActiveCount())
	assert.Nil(t, ar.Continue())
}
