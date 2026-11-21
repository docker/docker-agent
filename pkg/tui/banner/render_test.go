package banner

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRenderPreservesArt(t *testing.T) {
	t.Parallel()

	lines := Render()
	require.Len(t, lines, Height)
	for i, line := range lines {
		assert.Equal(t, Lines[i], strings.TrimRight(ansi.Strip(line), " "))
		assert.Equal(t, Width, ansi.StringWidth(line))
	}
	assert.NotEqual(t, Lines, lines, "the rendered banner includes color")
}

func TestRenderKeepsRowsAlignedWhenCentered(t *testing.T) {
	t.Parallel()

	const viewportWidth = Width + 20
	centered := lipgloss.PlaceHorizontal(viewportWidth, lipgloss.Center, strings.Join(Render(), "\n"))
	lines := strings.Split(ansi.Strip(centered), "\n")
	require.Len(t, lines, Height)
	for y, line := range lines {
		assert.Equal(t, strings.Repeat(" ", 10)+Lines[y], strings.TrimRight(line, " "))
	}
}

func TestSettledFrameMatchesCenteredStaticBanner(t *testing.T) {
	for _, width := range []int{Width, Width + 1, Width + 20, Width + 21, 240} {
		static := lipgloss.PlaceHorizontal(width, lipgloss.Center, strings.Join(Render(), "\n"))
		for _, progress := range []float64{settledAt - 1e-6, settledAt, 1} {
			frame := lipgloss.PlaceHorizontal(width, lipgloss.Center, strings.Join(RenderFrame(progress, width), "\n"))
			if progress < settledAt {
				assert.Equal(t, ansi.Strip(static), ansi.Strip(frame), "width %d, progress %g", width, progress)
			} else {
				assert.Equal(t, static, frame, "width %d, progress %g", width, progress)
			}
		}
	}
}

func TestRowColorsStayReadableOnThemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		theme string
		light bool
	}{
		{theme: "default"},
		{theme: "solarized-dark"},
		{theme: "default-light", light: true},
		{theme: "gruvbox-light", light: true},
	}
	for _, tt := range tests {
		t.Run(tt.theme, func(t *testing.T) {
			t.Parallel()

			theme, err := styles.LoadTheme(tt.theme)
			require.NoError(t, err)
			bg := lipgloss.Color(theme.Colors.Background)
			colors := rowColors(bg)
			require.Len(t, colors, Height)

			got := make([]string, len(colors))
			for y, c := range colors {
				got[y] = hex(c)
				// EnsureContrast leaves a color unchanged once it meets the minimum contrast.
				assert.Equal(t, got[y], hex(styles.EnsureContrast(c, bg)), "row %d is readable", y)
			}
			for y := 1; y < len(got); y++ {
				if tt.light {
					assert.Equal(t, got[0], got[y], "light rows use one grey")
				} else {
					assert.NotEqual(t, got[y-1], got[y], "dark rows stay distinct")
				}
			}
		})
	}
}

func hex(c color.Color) string {
	return styles.RGBToHex(styles.ColorToRGB(c))
}

func TestWindblownDeterminismClippingAndHold(t *testing.T) {
	for _, width := range []int{Width, Width + 1, Width + 44, 240} {
		assert.Nil(t, RenderFrame(0, Width-1))
		for frame := range 21 {
			progress := float64(frame) / 20.3
			got := RenderFrame(progress, width)
			assert.Equal(t, got, RenderFrame(progress, width))
			require.Len(t, got, Height)
			for _, line := range got {
				assert.LessOrEqual(t, ansi.StringWidth(line), width)
			}
			if progress >= settledAt {
				assert.Equal(t, Render(), got)
			}
		}
		first := RenderFrame(0, width)
		for _, line := range first {
			assert.Empty(t, strings.TrimSpace(ansi.Strip(line)))
		}
		var entered bool
		for frame := 1; frame <= 5; frame++ {
			for _, line := range RenderFrame(float64(frame)/20.3, width) {
				if stripped := ansi.Strip(line); stripped != "" && stripped[0] != ' ' {
					entered = true
				}
			}
		}
		assert.True(t, entered, "particles cross pane margin at width %d", width)
		for _, p := range []float64{settledAt, 0.99, 1, 2} {
			assert.Equal(t, Render(), RenderFrame(p, width))
		}
	}
}

func TestParticlesFormStreamThenSettleLeftToRight(t *testing.T) {
	for _, width := range []int{Width, Width + 44, 240} {
		var earlyVisible, middleDocker, middleAgent, lateDocker, lateAgent int
		minEarlyY, maxEarlyY := Height*4, 0
		for y, line := range Lines {
			for x, r := range []rune(line) {
				if r == ' ' {
					continue
				}
				for dx := range 2 {
					for dy := range 4 {
						if byte(r-'\u2800')&brailleBits[dx][dy] == 0 {
							continue
						}
						xPixel, yPixel := 2*x+dx, 4*y+dy
						px, _, settled := dustPosition(xPixel, yPixel, width, 0)
						assert.Negative(t, px)
						assert.False(t, settled)
						px, py, _ := dustPosition(xPixel, yPixel, width, 0.08)
						if px >= 0 && px < width*2 && py >= 0 && py < Height*4 {
							earlyVisible++
							minEarlyY = min(minEarlyY, py)
							maxEarlyY = max(maxEarlyY, py)
						}
						for _, sample := range []struct {
							p             float64
							docker, agent *int
						}{
							{0.5, &middleDocker, &middleAgent}, {0.7, &lateDocker, &lateAgent},
						} {
							px, py, done := dustPosition(xPixel, yPixel, width, sample.p)
							if done {
								assert.Equal(t, (width-Width)/2*2+xPixel, px)
								assert.Equal(t, yPixel, py)
								if x < agentStart {
									*sample.docker++
								} else {
									*sample.agent++
								}
							}
						}
						px, py, settled = dustPosition(xPixel, yPixel, width, settledAt)
						assert.True(t, settled)
						assert.Equal(t, (width-Width)/2*2+xPixel, px)
						assert.Equal(t, yPixel, py)
					}
				}
			}
		}
		assert.Positive(t, earlyVisible)
		assert.LessOrEqual(t, maxEarlyY-minEarlyY, 8, "early stream stays narrow")
		assert.Positive(t, middleDocker)
		assert.Zero(t, middleAgent)
		assert.Greater(t, lateDocker, middleDocker)
		assert.Positive(t, lateAgent)
	}
}
