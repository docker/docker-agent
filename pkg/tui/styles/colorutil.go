package styles

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// --- Hex parsing ---

// parseHexRGB parses a hex color string (#RGB or #RRGGBB) into normalized [0,1] sRGB components.
func parseHexRGB(hex string) (r, g, b float64, ok bool) {
	if !strings.HasPrefix(hex, "#") {
		return 0, 0, 0, false
	}

	h := strings.TrimPrefix(hex, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return 0, 0, 0, false
	}

	r8, err := strconv.ParseUint(h[0:2], 16, 8)
	if err != nil {
		return 0, 0, 0, false
	}
	g8, err := strconv.ParseUint(h[2:4], 16, 8)
	if err != nil {
		return 0, 0, 0, false
	}
	b8, err := strconv.ParseUint(h[4:6], 16, 8)
	if err != nil {
		return 0, 0, 0, false
	}

	return float64(r8) / 255.0, float64(g8) / 255.0, float64(b8) / 255.0, true
}

// ColorToRGB extracts normalized [0,1] sRGB components from a color.Color.
func ColorToRGB(c color.Color) (r, g, b float64) {
	ri, gi, bi, _ := c.RGBA()
	return float64(ri) / 65535, float64(gi) / 65535, float64(bi) / 65535
}

// RGBToHex formats normalized [0,1] sRGB components as a hex color string.
func RGBToHex(r, g, b float64) string {
	return fmt.Sprintf("#%02x%02x%02x", clamp8(r), clamp8(g), clamp8(b))
}

// RGBToColor converts normalized [0,1] sRGB components to a lipgloss color.
func RGBToColor(r, g, b float64) color.Color {
	return lipgloss.Color(RGBToHex(r, g, b))
}

func clamp8(v float64) int {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 255
	}
	return int(v*255 + 0.5)
}

// --- sRGB linearization ---

// sRGBToLinear converts an sRGB component [0,1] to linear light.
func sRGBToLinear(c float64) float64 {
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// linearToSRGB converts a linear light component [0,1] to sRGB.
func linearToSRGB(c float64) float64 {
	if c <= 0.0031308 {
		return c * 12.92
	}
	return 1.055*math.Pow(c, 1.0/2.4) - 0.055
}

// --- Luminance & contrast ---

// relativeLuminance returns the WCAG 2.x relative luminance of an sRGB color.
func relativeLuminance(r, g, b float64) float64 {
	return 0.2126*sRGBToLinear(r) + 0.7152*sRGBToLinear(g) + 0.0722*sRGBToLinear(b)
}

// relativeLuminanceHex returns the relative luminance for a hex color string.
func relativeLuminanceHex(hex string) (float64, bool) {
	r, g, b, ok := parseHexRGB(hex)
	if !ok {
		return 0, false
	}
	return relativeLuminance(r, g, b), true
}

// relativeLuminanceColor returns the relative luminance for a color.Color.
func relativeLuminanceColor(c color.Color) float64 {
	r, g, b := ColorToRGB(c)
	return relativeLuminance(r, g, b)
}

// contrastRatio returns the WCAG 2.x contrast ratio between two colors.
func contrastRatio(fg, bg color.Color) float64 {
	l1 := relativeLuminanceColor(fg)
	l2 := relativeLuminanceColor(bg)
	lighter := max(l1, l2)
	darker := min(l1, l2)
	return (lighter + 0.05) / (darker + 0.05)
}

// contrastRatioHex returns the WCAG contrast ratio between two hex color strings.
func contrastRatioHex(fgHex, bgHex string) (float64, bool) {
	fgLum, ok := relativeLuminanceHex(fgHex)
	if !ok {
		return 0, false
	}
	bgLum, ok := relativeLuminanceHex(bgHex)
	if !ok {
		return 0, false
	}

	l1, l2 := fgLum, bgLum
	if l2 > l1 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05), true
}

// bestForegroundHex picks the candidate hex color with the highest contrast ratio against bgHex.
func bestForegroundHex(bgHex string, candidates ...string) string {
	if len(candidates) == 0 {
		return ""
	}
	best := candidates[0]
	bestRatio := -1.0

	for _, cand := range candidates {
		ratio, ok := contrastRatioHex(cand, bgHex)
		if !ok {
			continue
		}
		if ratio > bestRatio {
			bestRatio = ratio
			best = cand
		}
	}
	return best
}

// --- Dynamic contrast helpers ---

const (
	// mutedContrastStrength controls how much the muted foreground shifts
	// away from the background (0.0 = invisible, 1.0 = full black/white).
	mutedContrastStrength = 0.45

	// minIndicatorContrast is the minimum WCAG contrast ratio for semantic
	// indicator colors (running dot, attention bang).
	minIndicatorContrast = 4.5

	// maxBoostSteps limits blend iterations in ensureContrast.
	maxBoostSteps = 20
)

// MutedContrastFg returns a foreground color that is visible but subtle against
// the given background. It blends the background toward white (for dark bg) or
// black (for light bg) by mutedContrastStrength.
func MutedContrastFg(bg color.Color) color.Color {
	rf, gf, bf := ColorToRGB(bg)
	lum := 0.299*rf + 0.587*gf + 0.114*bf

	var tgt float64
	if lum > 0.5 {
		tgt = 0.0
	} else {
		tgt = 1.0
	}

	s := mutedContrastStrength
	return RGBToColor(rf+(tgt-rf)*s, gf+(tgt-gf)*s, bf+(tgt-bf)*s)
}

// EnsureContrast returns fg unchanged if it already meets minIndicatorContrast
// against bg. Otherwise it progressively blends fg toward white (on dark bg) or
// black (on light bg) until the threshold is met, preserving the original hue direction.
func EnsureContrast(fg, bg color.Color) color.Color {
	if contrastRatio(fg, bg) >= minIndicatorContrast {
		return fg
	}

	rf, gf, bf := ColorToRGB(fg)
	bgR, bgG, bgB := ColorToRGB(bg)
	bgLum := relativeLuminance(bgR, bgG, bgB)

	var tR, tG, tB float64
	if bgLum > 0.5 {
		tR, tG, tB = 0, 0, 0
	} else {
		tR, tG, tB = 1, 1, 1
	}

	for step := 1; step <= maxBoostSteps; step++ {
		t := float64(step) / float64(maxBoostSteps)
		nr := rf + (tR-rf)*t
		ng := gf + (tG-gf)*t
		nb := bf + (tB-bf)*t
		candidate := RGBToColor(nr, ng, nb)
		if contrastRatio(candidate, bg) >= minIndicatorContrast {
			return candidate
		}
	}

	return RGBToColor(tR, tG, tB)
}

// --- HSL conversion ---

// rgbToHSL converts normalized [0,1] sRGB to HSL.
// H is in [0,360), S and L are in [0,1].
func rgbToHSL(r, g, b float64) (h, s, l float64) {
	maxC := max(r, max(g, b))
	minC := min(r, min(g, b))
	l = (maxC + minC) / 2

	if maxC == minC {
		return 0, 0, l
	}

	d := maxC - minC
	if l > 0.5 {
		s = d / (2.0 - maxC - minC)
	} else {
		s = d / (maxC + minC)
	}

	switch maxC {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	case b:
		h = (r-g)/d + 4
	}
	h *= 60

	return h, s, l
}

// hslToRGB converts HSL to normalized [0,1] sRGB.
// H is in [0,360), S and L are in [0,1].
func hslToRGB(h, s, l float64) (r, g, b float64) {
	if s == 0 {
		return l, l, l
	}

	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q

	h /= 360
	r = hueToRGB(p, q, h+1.0/3.0)
	g = hueToRGB(p, q, h)
	b = hueToRGB(p, q, h-1.0/3.0)
	return r, g, b
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t++
	}
	if t > 1 {
		t--
	}
	switch {
	case t < 1.0/6.0:
		return p + (q-p)*6*t
	case t < 1.0/2.0:
		return q
	case t < 2.0/3.0:
		return p + (q-p)*(2.0/3.0-t)*6
	default:
		return p
	}
}

// --- Palette generation ---

// defaultAgentHues provides 16 well-spaced default hue values for agent colors.
var defaultAgentHues = []float64{
	220, // Blue
	280, // Purple
	170, // Teal
	30,  // Orange
	330, // Pink
	140, // Green
	200, // Steel blue
	260, // Deep purple
	50,  // Gold
	0,   // Red
	185, // Dark teal
	20,  // Burnt orange
	235, // Indigo
	295, // Plum
	155, // Forest green
	340, // Crimson
}

// GenerateBadgePalette generates badge background colors from hues, adapting
// saturation and lightness based on the theme background.
// Dark backgrounds get lighter, more saturated badges; light backgrounds get darker ones.
func generateBadgePalette(hues []float64, bg color.Color) []color.Color {
	bgR, bgG, bgB := ColorToRGB(bg)
	bgLum := relativeLuminance(bgR, bgG, bgB)

	isDark := bgLum < 0.5

	colors := make([]color.Color, len(hues))
	for i, hue := range hues {
		var s, l float64
		if isDark {
			s = 0.65 + 0.10*math.Sin(float64(i)*0.7)
			l = 0.42 + 0.06*math.Cos(float64(i)*0.9)
		} else {
			s = 0.60 + 0.10*math.Sin(float64(i)*0.7)
			l = 0.38 + 0.06*math.Cos(float64(i)*0.9)
		}

		r, g, b := hslToRGB(hue, s, l)
		colors[i] = lipgloss.Color(RGBToHex(r, g, b))
	}
	return colors
}

// GenerateAccentPalette generates sidebar accent foreground colors from hues,
// adapting to the theme background for readability.
// Dark backgrounds get brighter accents; light backgrounds get darker ones.
func generateAccentPalette(hues []float64, bg color.Color) []color.Color {
	bgR, bgG, bgB := ColorToRGB(bg)
	bgLum := relativeLuminance(bgR, bgG, bgB)

	isDark := bgLum < 0.5

	colors := make([]color.Color, len(hues))
	for i, hue := range hues {
		var s, l float64
		if isDark {
			s = 0.55 + 0.15*math.Sin(float64(i)*0.5)
			l = 0.68 + 0.08*math.Cos(float64(i)*0.7)
		} else {
			s = 0.70 + 0.15*math.Sin(float64(i)*0.5)
			l = 0.30 + 0.06*math.Cos(float64(i)*0.7)
		}

		r, g, b := hslToRGB(hue, s, l)
		colors[i] = lipgloss.Color(RGBToHex(r, g, b))
	}
	return colors
}

// --- Perceptual distance ---

// colorDistanceCIE76 returns the Euclidean distance between two colors in CIELAB space.
// A value below ~25 means colors may be hard to distinguish at a glance.
func colorDistanceCIE76(c1, c2 color.Color) float64 {
	l1, a1, b1 := colorToLab(c1)
	l2, a2, b2 := colorToLab(c2)
	dl := l1 - l2
	da := a1 - a2
	db := b1 - b2
	return math.Sqrt(dl*dl + da*da + db*db)
}

// colorToLab converts a color.Color to CIELAB via XYZ (D65 illuminant).
func colorToLab(c color.Color) (l, a, b float64) {
	r, g, bl := ColorToRGB(c)
	// sRGB to linear
	rl := sRGBToLinear(r)
	gl := sRGBToLinear(g)
	bll := sRGBToLinear(bl)

	// Linear RGB to XYZ (D65)
	x := 0.4124564*rl + 0.3575761*gl + 0.1804375*bll
	y := 0.2126729*rl + 0.7151522*gl + 0.0721750*bll
	z := 0.0193339*rl + 0.1191920*gl + 0.9503041*bll

	// XYZ to Lab (D65 white point)
	x /= 0.95047
	y /= 1.00000
	z /= 1.08883

	x = labF(x)
	y = labF(y)
	z = labF(z)

	l = 116*y - 16
	a = 500 * (x - y)
	b = 200 * (y - z)
	return l, a, b
}

func labF(t float64) float64 {
	if t > 0.008856 {
		return math.Cbrt(t)
	}
	return 7.787*t + 16.0/116.0
}
