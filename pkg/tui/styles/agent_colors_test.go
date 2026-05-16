package styles

import (
	"fmt"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Agent registry and color assignment tests ---

func TestAgentBadgeStyleFor_UsesRegisteredOrder(t *testing.T) {
	SetAgentOrder([]string{"root", "git-agent", "docs-writer"})
	defer SetAgentOrder(nil)

	// Each agent should get a distinct style based on its position.
	r1 := AgentBadgeStyleFor("root").Render("x")
	r2 := AgentBadgeStyleFor("git-agent").Render("x")
	r3 := AgentBadgeStyleFor("docs-writer").Render("x")
	assert.NotEqual(t, r1, r2)
	assert.NotEqual(t, r2, r3)
	assert.NotEqual(t, r1, r3)
}

func TestAgentBadgeStyleFor_UnknownAgentReturnsFallback(t *testing.T) {
	SetAgentOrder([]string{"root", "git-agent"})
	defer SetAgentOrder(nil)

	// Unknown agent should get the fallback style, same as calling with no registration.
	s := AgentBadgeStyleFor("unknown-agent").Render("x")
	require.NotEmpty(t, s)
}

func TestAgentBadgeStyleFor_WrapsAroundPaletteSize(t *testing.T) {
	agentRegistry.RLock()
	size := len(agentRegistry.badgeStyles)
	agentRegistry.RUnlock()
	require.Positive(t, size)

	agents := make([]string, size+3)
	for i := range agents {
		agents[i] = fmt.Sprintf("agent-%d", i)
	}
	SetAgentOrder(agents)
	defer SetAgentOrder(nil)

	// The last agent wraps around, so it should match the style at (size+2)%size.
	last := AgentBadgeStyleFor(agents[len(agents)-1]).Render("x")
	wrapped := AgentBadgeStyleFor(agents[(size+2)%size]).Render("x")
	assert.Equal(t, last, wrapped)
}

func TestAgentBadgeStyleFor_EmptyRegistryReturnsFallback(t *testing.T) {
	SetAgentOrder(nil)
	defer SetAgentOrder(nil)

	s := AgentBadgeStyleFor("anything").Render("x")
	require.NotEmpty(t, s)
}

func TestSetAgentOrder_UpdatesRegistry(t *testing.T) {
	SetAgentOrder([]string{"a", "b", "c"})
	defer SetAgentOrder(nil)

	styleA1 := AgentBadgeStyleFor("a").Render("x")
	styleC1 := AgentBadgeStyleFor("c").Render("x")
	assert.NotEqual(t, styleA1, styleC1)

	// Swap order: a and c should exchange styles.
	SetAgentOrder([]string{"c", "b", "a"})
	styleA2 := AgentBadgeStyleFor("a").Render("x")
	styleC2 := AgentBadgeStyleFor("c").Render("x")
	assert.Equal(t, styleA1, styleC2, "c at index 0 should match a's previous index-0 style")
	assert.Equal(t, styleC1, styleA2, "a at index 2 should match c's previous index-2 style")
}

// --- Style rendering tests ---

func TestAgentBadgeStyleFor_ProducesDifferentStylesPerIndex(t *testing.T) {
	SetAgentOrder([]string{"root", "docs-writer"})
	defer SetAgentOrder(nil)

	rendered1 := AgentBadgeStyleFor("root").Render("root")
	rendered2 := AgentBadgeStyleFor("docs-writer").Render("docs-writer")

	require.NotEmpty(t, rendered1)
	require.NotEmpty(t, rendered2)
	assert.NotEqual(t, rendered1, rendered2)
}

func TestAgentBadgeStyleFor_Deterministic(t *testing.T) {
	SetAgentOrder([]string{"root"})
	defer SetAgentOrder(nil)

	s1 := AgentBadgeStyleFor("root").Render("root")
	s2 := AgentBadgeStyleFor("root").Render("root")
	assert.Equal(t, s1, s2)
}

func TestAgentAccentStyleFor_Deterministic(t *testing.T) {
	SetAgentOrder([]string{"root"})
	defer SetAgentOrder(nil)

	s1 := AgentAccentStyleFor("root").Render("root")
	s2 := AgentAccentStyleFor("root").Render("root")
	assert.Equal(t, s1, s2)
}

func TestAgentBadgeColorsFor_HasFgAndBg(t *testing.T) {
	SetAgentOrder([]string{"root"})
	defer SetAgentOrder(nil)

	colors := AgentBadgeColorsFor("root")
	assert.NotNil(t, colors.Fg)
	assert.NotNil(t, colors.Bg)
}

// --- Cache tests ---

func TestSetAgentOrder_PopulatesCache(t *testing.T) {
	SetAgentOrder([]string{"root", "docs-writer"})
	defer SetAgentOrder(nil)

	agentRegistry.RLock()
	defer agentRegistry.RUnlock()

	assert.NotEmpty(t, agentRegistry.badgeStyles)
	assert.NotEmpty(t, agentRegistry.accentStyles)
	assert.Len(t, agentRegistry.accentStyles, len(agentRegistry.badgeStyles))
}

func TestInvalidateAgentColorCache_RebuildsCachedStyles(t *testing.T) {
	SetAgentOrder([]string{"root"})
	defer SetAgentOrder(nil)

	before := AgentBadgeStyleFor("root").Render("root")
	InvalidateAgentColorCache()
	after := AgentBadgeStyleFor("root").Render("root")

	assert.Equal(t, before, after, "cache rebuild with same theme should produce identical styles")
}

func TestAgentBadgeStyleFor_UsesCachedStyle(t *testing.T) {
	SetAgentOrder([]string{"a", "b"})
	defer SetAgentOrder(nil)

	for range 100 {
		s := AgentBadgeStyleFor("b").Render("b")
		require.NotEmpty(t, s)
	}
}

// --- Layer 1: WCAG contrast validation across all themes ---

const (
	// minBadgeContrast is the WCAG AA minimum for normal text.
	minBadgeContrast = 4.5
	// minAccentContrast is the WCAG AA minimum for large/bold text.
	minAccentContrast = 3.0
)

func TestAllBuiltinThemes_AgentBadgeContrast(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			theme, err := LoadTheme(ref)
			require.NoError(t, err)

			hues := theme.Colors.AgentHues
			if len(hues) == 0 {
				hues = defaultAgentHues
			}

			bg := lipgloss.Color(theme.Colors.Background)
			badgeColors := generateBadgePalette(hues, bg)

			for i, badgeBg := range badgeColors {
				r, g, b := ColorToRGB(badgeBg)
				bgHex := RGBToHex(r, g, b)
				fgHex := bestForegroundHex(
					bgHex,
					theme.Colors.TextBright,
					theme.Colors.Background,
					"#000000",
					"#ffffff",
				)
				fg := lipgloss.Color(fgHex)

				ratio := contrastRatio(fg, badgeBg)
				assert.GreaterOrEqual(t, ratio, minBadgeContrast,
					"badge %d (bg=%s, fg=%s) contrast %.2f < %.1f in theme %s",
					i, bgHex, fgHex, ratio, minBadgeContrast, ref)
			}
		})
	}
}

func TestAllBuiltinThemes_AgentAccentContrast(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			theme, err := LoadTheme(ref)
			require.NoError(t, err)

			hues := theme.Colors.AgentHues
			if len(hues) == 0 {
				hues = defaultAgentHues
			}

			bg := lipgloss.Color(theme.Colors.Background)
			accentColors := generateAccentPalette(hues, bg)

			for i, accent := range accentColors {
				ratio := contrastRatio(accent, bg)
				r, g, b := ColorToRGB(accent)
				hex := RGBToHex(r, g, b)
				assert.GreaterOrEqual(t, ratio, minAccentContrast,
					"accent %d (%s) contrast %.2f < %.1f against bg %s in theme %s",
					i, hex, ratio, minAccentContrast, theme.Colors.Background, ref)
			}
		})
	}
}

// --- Layer 1: Pairwise color distinctness across all themes ---

const (
	// minColorDistance is the minimum CIE76 ΔE between adjacent palette entries.
	// Below ~15 colors become hard to distinguish at a glance.
	minColorDistance = 10.0
)

func TestAllBuiltinThemes_AgentBadgeDistinctness(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			theme, err := LoadTheme(ref)
			require.NoError(t, err)

			hues := theme.Colors.AgentHues
			if len(hues) == 0 {
				hues = defaultAgentHues
			}

			bg := lipgloss.Color(theme.Colors.Background)
			palette := generateBadgePalette(hues, bg)

			for i := range palette {
				for j := i + 1; j < len(palette); j++ {
					dist := colorDistanceCIE76(palette[i], palette[j])
					assert.GreaterOrEqual(t, dist, minColorDistance,
						"badge colors %d and %d are too similar (ΔE=%.1f) in theme %s",
						i, j, dist, ref)
				}
			}
		})
	}
}

func TestAllBuiltinThemes_AgentAccentDistinctness(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			theme, err := LoadTheme(ref)
			require.NoError(t, err)

			hues := theme.Colors.AgentHues
			if len(hues) == 0 {
				hues = defaultAgentHues
			}

			bg := lipgloss.Color(theme.Colors.Background)
			palette := generateAccentPalette(hues, bg)

			for i := range palette {
				for j := i + 1; j < len(palette); j++ {
					dist := colorDistanceCIE76(palette[i], palette[j])
					assert.GreaterOrEqual(t, dist, minColorDistance,
						"accent colors %d and %d are too similar (ΔE=%.1f) in theme %s",
						i, j, dist, ref)
				}
			}
		})
	}
}

// --- Layer 2: Color audit report (run with -v) ---

func TestAgentColorAuditReport(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			theme, err := LoadTheme(ref)
			require.NoError(t, err)

			hues := theme.Colors.AgentHues
			if len(hues) == 0 {
				hues = defaultAgentHues
			}

			bg := lipgloss.Color(theme.Colors.Background)
			badges := generateBadgePalette(hues, bg)
			accents := generateAccentPalette(hues, bg)

			t.Logf("\n=== Agent Color Audit: %s (bg: %s) ===", theme.Name, theme.Colors.Background)
			t.Logf("%-5s %-10s %-10s %-10s %-12s %-10s %-10s",
				"Idx", "Hue", "Badge", "Badge FG", "Badge CR", "Accent", "Accent CR")
			t.Logf("%-5s %-10s %-10s %-10s %-12s %-10s %-10s",
				"---", "---", "---", "---", "---", "---", "---")

			for i := range hues {
				br, bg2, bb := ColorToRGB(badges[i])
				badgeHex := RGBToHex(br, bg2, bb)

				fgHex := bestForegroundHex(badgeHex,
					theme.Colors.TextBright, theme.Colors.Background,
					"#000000", "#ffffff")
				fg := lipgloss.Color(fgHex)
				badgeCR := contrastRatio(fg, badges[i])

				ar, ag, ab := ColorToRGB(accents[i])
				accentHex := RGBToHex(ar, ag, ab)
				accentCR := contrastRatio(accents[i], bg)

				badgeStatus := "✓"
				if badgeCR < minBadgeContrast {
					badgeStatus = "✗"
				}
				accentStatus := "✓"
				if accentCR < minAccentContrast {
					accentStatus = "✗"
				}

				t.Logf("%-5d %-10.0f %-10s %-10s %s %-9.2f %-10s %s %.2f",
					i, hues[i], badgeHex, fgHex, badgeStatus, badgeCR,
					accentHex, accentStatus, accentCR)
			}

			// Log minimum pairwise distances
			minBadgeDist := 999.0
			minAccentDist := 999.0
			for i := range badges {
				for j := i + 1; j < len(badges); j++ {
					if d := colorDistanceCIE76(badges[i], badges[j]); d < minBadgeDist {
						minBadgeDist = d
					}
					if d := colorDistanceCIE76(accents[i], accents[j]); d < minAccentDist {
						minAccentDist = d
					}
				}
			}
			t.Logf("\nMin badge pairwise ΔE:  %.1f (threshold: %.1f)", minBadgeDist, minColorDistance)
			t.Logf("Min accent pairwise ΔE: %.1f (threshold: %.1f)", minAccentDist, minColorDistance)
		})
	}
}
