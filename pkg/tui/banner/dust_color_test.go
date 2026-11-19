package banner

import (
	"bytes"
	"image/color"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

var foregroundRGB = regexp.MustCompile(`\x1b\[38;2;(\d+);(\d+);(\d+)m`)

func testContrast(fg, bg color.Color) float64 {
	luminance := func(c color.Color) float64 {
		r, g, b := styles.ColorToRGB(c)
		linear := func(v float64) float64 {
			if v <= 0.04045 {
				return v / 12.92
			}
			return math.Pow((v+0.055)/1.055, 2.4)
		}
		return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
	}
	a, b := luminance(fg), luminance(bg)
	return (max(a, b) + 0.05) / (min(a, b) + 0.05)
}

func TestDustFrameThemeContrast(t *testing.T) {
	previousBackground := styles.Background
	previousPolarity := styles.TerminalIsDark()
	t.Cleanup(func() { styles.Background = previousBackground; styles.SetTerminalDark(previousPolarity) })
	for _, name := range []string{"default-light", "gruvbox-light", "default", "solarized-dark"} {
		t.Run(name, func(t *testing.T) {
			theme, err := styles.LoadTheme(name)
			require.NoError(t, err)
			styles.Background = lipgloss.Color(theme.Colors.Background)
			for _, polarity := range []bool{true, false} {
				styles.SetTerminalDark(polarity)
				colors := rowColors(styles.Background)
				for _, color := range colors {
					assert.GreaterOrEqual(t, testContrast(color, styles.Background), 4.45)
				}
				for _, progress := range []float64{0.1, 0.3, 0.5, 0.7, 0.85} {
					frame := RenderFrame(progress, 120)
					require.Len(t, frame, Height)
					for _, line := range frame {
						for _, parts := range foregroundRGB.FindAllStringSubmatch(line, -1) {
							var rgb [3]uint8
							for i := range rgb {
								v, err := strconv.ParseUint(parts[i+1], 10, 8)
								require.NoError(t, err)
								rgb[i] = uint8(v)
							}
							fg := color.RGBA{R: rgb[0], G: rgb[1], B: rgb[2], A: 255}
							assert.GreaterOrEqual(t, testContrast(fg, styles.Background), 4.45)
						}
					}
				}
			}
		})
	}
}

func TestLightBannerColorOutput(t *testing.T) {
	previousBackground := styles.Background
	t.Cleanup(func() { styles.Background = previousBackground })
	theme, err := styles.LoadTheme("default-light")
	require.NoError(t, err)
	styles.Background = lipgloss.Color(theme.Colors.Background)

	for _, lines := range [][]string{Render(), RenderFrame(0.3, 100), RenderFrame(0.7, 100), RenderFrame(settledAt, 100)} {
		coloredRows := 0
		for _, line := range lines {
			matches := foregroundRGB.FindAllStringSubmatch(line, -1)
			if len(matches) > 0 {
				coloredRows++
			}
			for _, parts := range matches {
				assert.Equal(t, []string{"56", "56", "56"}, parts[1:])
			}
		}
		assert.Positive(t, coloredRows)
	}

	original := strings.Join(Render(), "\n")
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256, colorprofile.ANSI, colorprofile.ASCII, colorprofile.NoTTY} {
		t.Run(profile.String(), func(t *testing.T) {
			var out bytes.Buffer
			writer := colorprofile.Writer{Forward: &out, Profile: profile}
			_, err := writer.WriteString(original)
			require.NoError(t, err)
			assert.Equal(t, ansi.Strip(original), ansi.Strip(out.String()))
			switch profile {
			case colorprofile.TrueColor:
				assert.Equal(t, original, out.String())
			case colorprofile.ANSI256, colorprofile.ANSI:
				assert.Empty(t, foregroundRGB.FindAllString(out.String(), -1))
				assert.NotEqual(t, ansi.Strip(out.String()), out.String())
			case colorprofile.ASCII:
				assert.Empty(t, foregroundRGB.FindAllString(out.String(), -1))
			case colorprofile.NoTTY:
				assert.Equal(t, ansi.Strip(original), out.String())
			}
		})
	}
}

func TestLightGreyOnChatBackgrounds(t *testing.T) {
	for _, background := range []string{"#fafafa", "#fbf1c7", "#828282", "#808080", "#ff8080"} {
		t.Run(background, func(t *testing.T) {
			bg := lipgloss.Color(background)
			colors := rowColors(bg)
			require.Len(t, colors, Height)
			for _, c := range colors {
				r, g, b, _ := c.RGBA()
				assert.Equal(t, r, g)
				assert.Equal(t, r, b)
				assert.LessOrEqual(t, r, uint32(56*257))
				assert.Equal(t, hex(colors[0]), hex(c))
				assert.GreaterOrEqual(t, testContrast(c, bg), 4.5)
			}
		})
	}
}

func TestSunsetPaletteOnChatBackgrounds(t *testing.T) {
	dark := []string{"#b28aff", "#d981d5", "#ff78aa", "#ff8c95", "#ffa080", "#ffb46b"}
	for _, name := range []string{"default", "solarized-dark"} {
		t.Run(name, func(t *testing.T) {
			theme, err := styles.LoadTheme(name)
			require.NoError(t, err)
			bg := lipgloss.Color(theme.Colors.Background)
			colors := rowColors(bg)
			for i, c := range colors {
				assert.Equal(t, hex(styles.EnsureContrast(lipgloss.Color(dark[i]), bg)), hex(c), "row %d", i)
				assert.GreaterOrEqual(t, testContrast(c, bg), 4.5, "row %d", i)
				if i > 0 {
					assert.NotEqual(t, hex(colors[i-1]), hex(c), "row %d", i)
				}
				if name == "default" {
					assert.Equal(t, dark[i], hex(c), "theme preserves palette row %d", i)
				}
			}
		})
	}
}
