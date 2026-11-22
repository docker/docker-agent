package spinner

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSpinnerLabelFollowsThemeChange(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	dark := styles.DefaultTheme()
	styles.ApplyTheme(dark)
	fixedStyle := styles.SpinnerDotsAccentStyle
	s := New(animation.NewSnapshotRuntime(0), ModeBoth, fixedStyle).(*spinner)
	s.SetMessage("ABCDE")
	s.lightPosition = 1

	want := func() string {
		return fixedStyle.Render(s.RawFrame()) + " " + strings.Join([]string{
			styles.SpinnerTextBrightStyle.Render("A"),
			styles.SpinnerTextBrightestStyle.Render("B"),
			styles.SpinnerTextBrightStyle.Render("C"),
			styles.SpinnerTextDimStyle.Render("D"),
			styles.SpinnerTextDimmestStyle.Render("E"),
		}, "")
	}
	before := s.View()
	require.Equal(t, want(), before)

	reloaded := *dark
	reloaded.Colors.Accent = "#123456"
	reloaded.Colors.SpinnerBrightest = "#234567"
	reloaded.Colors.SpinnerBright = "#345678"
	reloaded.Colors.SpinnerDim = "#456789"
	styles.ApplyTheme(&reloaded)
	after := s.View()
	require.Equal(t, want(), after)
	require.NotEqual(t, before, after)
	require.Equal(t, 1, s.lightPosition, "the theme change does not reset the light sweep")
}

type spinnerClock struct{ now time.Time }

func (c *spinnerClock) Now() time.Time { return c.now }

func (c *spinnerClock) Tick(_ time.Duration, message func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return message(c.now) }
}

func TestSpinnerDotsCacheFollowsThemeChanges(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	dark := styles.DefaultTheme()
	styles.ApplyTheme(dark)
	oldStyle := styles.SpinnerDotsAccentStyle
	clock := &spinnerClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	styleCalls := 0
	s := NewThemed(ar, ModeSpinnerOnly, func() lipgloss.Style {
		styleCalls++
		return styles.SpinnerDotsAccentStyle
	}).(*spinner)
	cmd := s.Init()
	t.Cleanup(s.Stop)
	advance := func(elapsed time.Duration) {
		t.Helper()
		clock.now = clock.now.Add(elapsed)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		_, _ = s.Update(tick)
		require.True(t, tick.Dirty())
		cmd = ar.Continue()
	}

	checkCache := func(style lipgloss.Style) {
		t.Helper()
		for i, frame := range animation.Chat.Frames() {
			require.Equal(t, style.Render(frame), s.styledSpinnerFrames[i])
		}
	}
	checkCache(oldStyle)
	for i, frame := range animation.Chat.Frames() {
		if i > 0 {
			advance(spinnerFrameDuration)
		}
		require.Equal(t, oldStyle.Render(frame), s.View())
	}
	require.Equal(t, 1, styleCalls, "the entire first cycle uses the pre-rendered frames")
	last := s.RawFrame()

	// A file reload can change colors without changing the theme reference.
	reloaded := *dark
	reloaded.Colors.Accent = "#123456"
	styles.ApplyTheme(&reloaded)
	newStyle := styles.SpinnerDotsAccentStyle
	require.NotEqual(t, oldStyle.Render(last), newStyle.Render(last))
	require.Equal(t, last, s.RawFrame(), "theme changes do not advance or reset the clock")
	require.Equal(t, newStyle.Render(last), s.View())
	checkCache(newStyle)
	for _, frame := range animation.Chat.Frames() {
		advance(spinnerFrameDuration)
		require.Equal(t, newStyle.Render(frame), s.View())
	}
	require.Equal(t, 2, styleCalls, "the entire frame set is rebuilt once after applying a theme")
}

func TestFixedSpinnerDotsKeepExplicitStyleAcrossThemeChanges(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#abcdef"))
	s := NewWithFrames(animation.NewSnapshotRuntime(spinnerFrameDuration), ModeSpinnerOnly, style, animation.Frames{"x", "y"})
	require.Equal(t, style.Render("y"), s.View())
	styles.ApplyThemeRef(styles.DefaultLightThemeRef)
	require.Equal(t, style.Render("y"), s.View())
	require.Equal(t, style.Render("y"), s.Reset().View())
}
