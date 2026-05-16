package styles

import (
	"math"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Hex parsing ---

func TestParseHexRGB_Valid6Digit(t *testing.T) {
	t.Parallel()
	r, g, b, ok := parseHexRGB("#FF8000")
	require.True(t, ok)
	assert.InDelta(t, 1.0, r, 0.01)
	assert.InDelta(t, 0.502, g, 0.01)
	assert.InDelta(t, 0.0, b, 0.01)
}

func TestParseHexRGB_Valid3Digit(t *testing.T) {
	t.Parallel()
	r, g, b, ok := parseHexRGB("#F00")
	require.True(t, ok)
	assert.InDelta(t, 1.0, r, 0.01)
	assert.InDelta(t, 0.0, g, 0.01)
	assert.InDelta(t, 0.0, b, 0.01)
}

func TestParseHexRGB_Invalid(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "FF0000", "#GG0000", "#FF00", "#FF000000"} {
		_, _, _, ok := parseHexRGB(input)
		assert.False(t, ok, "expected failure for %q", input)
	}
}

// --- RGB ↔ Hex roundtrip ---

func TestRGBToHex_Roundtrip(t *testing.T) {
	t.Parallel()
	hex := RGBToHex(0.2, 0.4, 0.6)
	r, g, b, ok := parseHexRGB(hex)
	require.True(t, ok)
	assert.InDelta(t, 0.2, r, 0.01)
	assert.InDelta(t, 0.4, g, 0.01)
	assert.InDelta(t, 0.6, b, 0.01)
}

func TestRGBToHex_BlackWhite(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "#000000", RGBToHex(0, 0, 0))
	assert.Equal(t, "#ffffff", RGBToHex(1, 1, 1))
}

// --- sRGB linearization roundtrip ---

func TestLinearization_Roundtrip(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{0, 0.1, 0.5, 0.9, 1.0} {
		result := linearToSRGB(sRGBToLinear(v))
		assert.InDelta(t, v, result, 0.001, "roundtrip failed for %f", v)
	}
}

// --- Luminance ---

func TestRelativeLuminance_BlackWhite(t *testing.T) {
	t.Parallel()
	assert.InDelta(t, 0.0, relativeLuminance(0, 0, 0), 0.001)
	assert.InDelta(t, 1.0, relativeLuminance(1, 1, 1), 0.001)
}

func TestRelativeLuminanceHex(t *testing.T) {
	t.Parallel()
	lum, ok := relativeLuminanceHex("#ffffff")
	require.True(t, ok)
	assert.InDelta(t, 1.0, lum, 0.001)

	lum, ok = relativeLuminanceHex("#000000")
	require.True(t, ok)
	assert.InDelta(t, 0.0, lum, 0.001)
}

// --- Contrast ratio ---

func TestContrastRatio_BlackWhite(t *testing.T) {
	t.Parallel()
	black := lipgloss.Color("#000000")
	white := lipgloss.Color("#ffffff")
	ratio := contrastRatio(black, white)
	assert.InDelta(t, 21.0, ratio, 0.1)
}

func TestContrastRatio_SameColor(t *testing.T) {
	t.Parallel()
	c := lipgloss.Color("#808080")
	ratio := contrastRatio(c, c)
	assert.InDelta(t, 1.0, ratio, 0.001)
}

func TestContrastRatioHex(t *testing.T) {
	t.Parallel()
	ratio, ok := contrastRatioHex("#000000", "#ffffff")
	require.True(t, ok)
	assert.InDelta(t, 21.0, ratio, 0.1)
}

func TestBestForegroundHex(t *testing.T) {
	t.Parallel()
	// On dark background, white should win
	best := bestForegroundHex("#000000", "#333333", "#ffffff")
	assert.Equal(t, "#ffffff", best)

	// On light background, black should win
	best = bestForegroundHex("#ffffff", "#000000", "#cccccc")
	assert.Equal(t, "#000000", best)
}

// --- HSL conversion ---

func TestRGBToHSL_Red(t *testing.T) {
	t.Parallel()
	h, s, l := rgbToHSL(1, 0, 0)
	assert.InDelta(t, 0, h, 0.1)
	assert.InDelta(t, 1.0, s, 0.01)
	assert.InDelta(t, 0.5, l, 0.01)
}

func TestRGBToHSL_Green(t *testing.T) {
	t.Parallel()
	h, s, l := rgbToHSL(0, 1, 0)
	assert.InDelta(t, 120, h, 0.1)
	assert.InDelta(t, 1.0, s, 0.01)
	assert.InDelta(t, 0.5, l, 0.01)
}

func TestRGBToHSL_Blue(t *testing.T) {
	t.Parallel()
	h, s, l := rgbToHSL(0, 0, 1)
	assert.InDelta(t, 240, h, 0.1)
	assert.InDelta(t, 1.0, s, 0.01)
	assert.InDelta(t, 0.5, l, 0.01)
}

func TestRGBToHSL_Gray(t *testing.T) {
	t.Parallel()
	h, s, l := rgbToHSL(0.5, 0.5, 0.5)
	_ = h // hue is undefined for gray
	assert.InDelta(t, 0.0, s, 0.01)
	assert.InDelta(t, 0.5, l, 0.01)
}

func TestHSLToRGB_Roundtrip(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		r, g, b float64
	}{
		{1, 0, 0},
		{0, 1, 0},
		{0, 0, 1},
		{0.5, 0.5, 0.5},
		{0.2, 0.6, 0.8},
	}
	for _, tc := range testCases {
		h, s, l := rgbToHSL(tc.r, tc.g, tc.b)
		r, g, b := hslToRGB(h, s, l)
		assert.InDelta(t, tc.r, r, 0.01, "r mismatch for input %v", tc)
		assert.InDelta(t, tc.g, g, 0.01, "g mismatch for input %v", tc)
		assert.InDelta(t, tc.b, b, 0.01, "b mismatch for input %v", tc)
	}
}

// --- Dynamic contrast helpers ---

func TestMutedContrastFg_DarkBg(t *testing.T) {
	t.Parallel()
	bg := lipgloss.Color("#1C1C22")
	fg := MutedContrastFg(bg)
	// Should produce a lighter color than the background
	bgLum := relativeLuminanceColor(bg)
	fgLum := relativeLuminanceColor(fg)
	assert.Greater(t, fgLum, bgLum)
}

func TestMutedContrastFg_LightBg(t *testing.T) {
	t.Parallel()
	bg := lipgloss.Color("#eff1f5")
	fg := MutedContrastFg(bg)
	// Should produce a darker color than the background
	bgLum := relativeLuminanceColor(bg)
	fgLum := relativeLuminanceColor(fg)
	assert.Less(t, fgLum, bgLum)
}

func TestEnsureContrast_AlreadySufficient(t *testing.T) {
	t.Parallel()
	fg := lipgloss.Color("#ffffff")
	bg := lipgloss.Color("#000000")
	result := EnsureContrast(fg, bg)
	// White on black already has 21:1, should be unchanged
	r1, g1, b1 := ColorToRGB(fg)
	r2, g2, b2 := ColorToRGB(result)
	assert.InDelta(t, r1, r2, 0.01)
	assert.InDelta(t, g1, g2, 0.01)
	assert.InDelta(t, b1, b2, 0.01)
}

func TestEnsureContrast_BoostsLowContrast(t *testing.T) {
	t.Parallel()
	fg := lipgloss.Color("#333333")
	bg := lipgloss.Color("#222222")
	result := EnsureContrast(fg, bg)
	ratio := contrastRatio(result, bg)
	assert.GreaterOrEqual(t, ratio, minIndicatorContrast)
}

// --- Palette generation ---

func TestGenerateBadgePalette_CorrectLength(t *testing.T) {
	t.Parallel()
	bg := lipgloss.Color("#1C1C22")
	palette := generateBadgePalette(defaultAgentHues, bg)
	assert.Len(t, palette, len(defaultAgentHues))
}

func TestGenerateBadgePalette_AllDistinct(t *testing.T) {
	t.Parallel()
	bg := lipgloss.Color("#1C1C22")
	palette := generateBadgePalette(defaultAgentHues, bg)
	hexSet := make(map[string]bool)
	for _, c := range palette {
		r, g, b := ColorToRGB(c)
		hex := RGBToHex(r, g, b)
		hexSet[hex] = true
	}
	assert.Len(t, hexSet, len(palette), "all generated colors should be distinct")
}

func TestGenerateAccentPalette_CorrectLength(t *testing.T) {
	t.Parallel()
	bg := lipgloss.Color("#1C1C22")
	palette := generateAccentPalette(defaultAgentHues, bg)
	assert.Len(t, palette, len(defaultAgentHues))
}

func TestGenerateBadgePalette_DarkVsLight(t *testing.T) {
	t.Parallel()
	darkBg := lipgloss.Color("#1C1C22")
	lightBg := lipgloss.Color("#eff1f5")
	darkPalette := generateBadgePalette(defaultAgentHues[:1], darkBg)
	lightPalette := generateBadgePalette(defaultAgentHues[:1], lightBg)

	// Same hue should produce different lightness for dark vs light bg
	darkLum := relativeLuminanceColor(darkPalette[0])
	lightLum := relativeLuminanceColor(lightPalette[0])
	assert.Greater(t, math.Abs(darkLum-lightLum), 0.01, "dark and light themes should produce different badge lightness")
}

// --- Perceptual distance ---

func TestColorDistanceCIE76_Identical(t *testing.T) {
	t.Parallel()
	c := lipgloss.Color("#FF0000")
	assert.InDelta(t, 0, colorDistanceCIE76(c, c), 0.001)
}

func TestColorDistanceCIE76_BlackWhite(t *testing.T) {
	t.Parallel()
	black := lipgloss.Color("#000000")
	white := lipgloss.Color("#ffffff")
	dist := colorDistanceCIE76(black, white)
	assert.Greater(t, dist, 50.0, "black and white should be very far apart in CIELAB")
}

func TestColorDistanceCIE76_SimilarColors(t *testing.T) {
	t.Parallel()
	c1 := lipgloss.Color("#FF0000")
	c2 := lipgloss.Color("#FF1100")
	dist := colorDistanceCIE76(c1, c2)
	assert.Less(t, dist, 10.0, "very similar colors should have small distance")
}

// --- ColorToRGB ---

func TestColorToRGB_KnownValues(t *testing.T) {
	t.Parallel()
	c := lipgloss.Color("#ff0000")
	r, g, b := ColorToRGB(c)
	assert.InDelta(t, 1.0, r, 0.01)
	assert.InDelta(t, 0.0, g, 0.01)
	assert.InDelta(t, 0.0, b, 0.01)
}

// --- defaultAgentHues ---

func TestDefaultAgentHues_Length(t *testing.T) {
	t.Parallel()
	assert.Len(t, defaultAgentHues, 16)
}

func TestDefaultAgentHues_InRange(t *testing.T) {
	t.Parallel()
	for i, h := range defaultAgentHues {
		assert.GreaterOrEqual(t, h, 0.0, "hue %d out of range", i)
		assert.Less(t, h, 360.0, "hue %d out of range", i)
	}
}

// --- Helper to verify color.Color interface ---

func TestRGBToColor_ImplementsInterface(t *testing.T) {
	t.Parallel()
	c := RGBToColor(0.5, 0.5, 0.5)
	r, g, b, a := c.RGBA()
	assert.NotZero(t, r)
	assert.NotZero(t, g)
	assert.NotZero(t, b)
	assert.NotZero(t, a)
}

// --- CIELAB internals ---

func TestLabF_BelowThreshold(t *testing.T) {
	t.Parallel()
	// labF should handle very small values
	result := labF(0.001)
	assert.False(t, math.IsNaN(result))
	assert.False(t, math.IsInf(result, 0))
}
