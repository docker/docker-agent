package contextbar

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/animation/animationtest"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestHoldAndRetargetFromDisplayedValue(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	m.SetWidth(40)
	m.SetContextUsageDirect(20, 100)

	require.NotNil(t, m.SetContextUsage(80, 100))
	m.contextPercent = 0.45
	assert.Contains(t, m.View(), "Context 45%")

	// Retargeting mid-flight starts from the displayed fill; repeating target is a no-op.
	require.Nil(t, m.SetContextUsage(10, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	assert.InDelta(t, 0.10, m.animTo, 0.001)
	assert.Nil(t, m.SetContextUsage(10, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)

	// Hold stops animation, shows pending label, and preserves displayed fill as next origin.
	m.Hold()
	assert.False(t, m.anim.Running())
	assert.Zero(t, runtime.ActiveCount())
	assert.Contains(t, m.View(), "Context —")

	require.NotNil(t, m.SetContextUsage(0, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	assert.InDelta(t, 0, m.animTo, 0.001)
	assert.Contains(t, m.View(), "Context 45%")
	m.Cancel()

	// Zero limit eases to empty; already empty track does not animate.
	m.SetContextUsageDirect(45, 100)
	require.NotNil(t, m.SetContextUsage(0, 0))
	assert.InDelta(t, 0, m.animTo, 0.001)
	assert.Contains(t, m.View(), "Context —")
	m.Cancel()

	m.SetContextUsageDirect(0, 0)
	assert.Nil(t, m.SetContextUsage(0, 0))
	assert.Zero(t, runtime.ActiveCount())
}

func TestDirectUpdateAndCancel(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)

	m.SetContextUsageDirect(50, 100)
	assert.InDelta(t, 0.5, m.contextPercent, 0.001)
	assert.Zero(t, runtime.ActiveCount())

	_ = m.SetContextUsage(75, 100)
	require.Equal(t, int32(1), runtime.ActiveCount())
	m.SetContextUsageDirect(75, 100)
	assert.True(t, m.anim.Running(), "same target preserves an in-flight animation")

	m.SetContextUsageDirect(80, 100)
	assert.InDelta(t, 0.8, m.contextPercent, 0.001)
	assert.False(t, m.anim.Running())
	assert.Zero(t, runtime.ActiveCount())

	m.Cancel() // idempotent
	m.SetContextUsageDirect(-10, 100)
	assert.Zero(t, m.contextPercent)
	m.SetContextUsageDirect(200, 100)
	assert.InDelta(t, 1.0, m.contextPercent, 0)
}

func TestLabelAnimatesWithFill(t *testing.T) {
	t.Parallel()
	s, ar := animationtest.NewClock(time.Unix(1, 0))
	m := New(ar)
	m.SetWidth(40)
	m.SetContextUsageDirect(20, 100)
	assert.Equal(t, "20%", m.Label())

	cmd := m.SetContextUsage(80, 100)
	require.NotNil(t, cmd)
	assert.Equal(t, "20%", m.Label())

	s.Advance(animDuration/4 - animation.TickRate)
	tick, ok := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	assert.NotEqual(t, "20%", m.Label())
	assert.NotEqual(t, "80%", m.Label())
	assert.Contains(t, m.View(), "Context "+m.Label())

	cmd = ar.Continue()
	require.NotNil(t, cmd)
	s.Advance(animDuration)
	tick, ok = ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	assert.Equal(t, "80%", m.Label())
	assert.Contains(t, m.View(), "Context 80%")
	assert.Zero(t, ar.ActiveCount())
}

func TestViewLayoutAndLeadingCell(t *testing.T) {
	m := New(animation.NewRuntime())
	for _, tt := range []struct {
		used  int64
		label string
	}{
		{5, " Context 5%"},
		{50, " Context 50%"},
		{100, " Context 100%"},
	} {
		m.SetContextUsageDirect(tt.used, 100)
		for _, width := range []int{1, 10, 13, 36} {
			m.SetWidth(width)
			label := tt.label
			if len(label) > width {
				label = ""
			}
			want := strings.Repeat(blockFill, width-len(label)) + label
			assert.Equal(t, want, ansi.Strip(m.View()), "usage %d at width %d", tt.used, width)
		}
	}

	// Positive usage always fills at least the leading cell.
	m.SetWidth(20)
	m.SetContextUsageDirect(1, 1000)
	positive := m.View()
	m.SetContextUsageDirect(0, 1000)
	zero := m.View()
	require.Equal(t, lipgloss.Width(zero), lipgloss.Width(positive))
	first, _ := utf8.DecodeRuneInString(ansi.Strip(positive))
	assert.Equal(t, blockFill, string(first))
	assert.NotEqual(t, strings.SplitN(zero, blockFill, 2)[0], strings.SplitN(positive, blockFill, 2)[0])
}

func TestFillColorAndThemeSwitch(t *testing.T) {
	assert.Equal(t, styles.Info, fillColor(0.2))
	assert.Equal(t, styles.Warning, fillColor(criticalThreshold))
	assert.Equal(t, styles.Error, fillColor(1))

	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New(animation.NewRuntime())
	m.SetWidth(48)
	m.SetContextUsageDirect(50, 100)
	for _, ref := range []string{"default-light", "nord", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		warm := m.View()
		r, g, b, _ := styles.EditorBg.RGBA()
		assert.Contains(t, warm, fmt.Sprintf("48;2;%d;%d;%dm", r>>8, g>>8, b>>8))
		assert.Equal(t, 48, lipgloss.Width(warm))
	}
}
