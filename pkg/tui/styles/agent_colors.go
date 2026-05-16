package styles

import (
	"image/color"
	"sync"

	"charm.land/lipgloss/v2"
)

// AgentBadgeColors holds the resolved foreground and background colors for an agent badge.
type AgentBadgeColors struct {
	Fg color.Color
	Bg color.Color
}

// Fallback colors used when an agent is not in the registry or the cache is empty.
var (
	fallbackBadgeColors = AgentBadgeColors{
		Fg: lipgloss.Color("#ffffff"),
		Bg: lipgloss.Color("#1D63ED"),
	}
	fallbackBadgeStyle  = BaseStyle.Foreground(fallbackBadgeColors.Fg).Background(fallbackBadgeColors.Bg).Padding(0, 1)
	fallbackAccentStyle = BaseStyle.Foreground(lipgloss.Color("#98C379"))
)

// cachedBadgeStyle holds a precomputed badge style for a palette index.
type cachedBadgeStyle struct {
	colors AgentBadgeColors
	style  lipgloss.Style
}

// agentRegistry maps agent names to their index in the team list and holds
// precomputed styles for each palette entry.
var agentRegistry struct {
	sync.RWMutex

	indices      map[string]int
	badgeStyles  []cachedBadgeStyle
	accentStyles []lipgloss.Style
}

// SetAgentOrder updates the agent name → index mapping and rebuilds the style cache.
// Call this when the team info changes (e.g., on TeamInfoEvent).
func SetAgentOrder(agentNames []string) {
	agentRegistry.Lock()
	defer agentRegistry.Unlock()

	agentRegistry.indices = make(map[string]int, len(agentNames))
	for i, name := range agentNames {
		agentRegistry.indices[name] = i
	}

	rebuildAgentColorCache()
}

// rebuildAgentColorCache precomputes badge and accent styles from the current theme's hues.
// Must be called with agentRegistry.Lock held.
func rebuildAgentColorCache() {
	theme := CurrentTheme()

	hues := theme.Colors.AgentHues
	if len(hues) == 0 {
		hues = defaultAgentHues
	}

	bg := lipgloss.Color(theme.Colors.Background)
	badgeColors := generateBadgePalette(hues, bg)
	accentColors := generateAccentPalette(hues, bg)

	agentRegistry.badgeStyles = make([]cachedBadgeStyle, len(badgeColors))
	for i, bgColor := range badgeColors {
		r, g, b := ColorToRGB(bgColor)
		bgHex := RGBToHex(r, g, b)
		fgHex := bestForegroundHex(
			bgHex,
			theme.Colors.TextBright,
			theme.Colors.Background,
			"#000000",
			"#ffffff",
		)
		colors := AgentBadgeColors{
			Fg: lipgloss.Color(fgHex),
			Bg: bgColor,
		}
		agentRegistry.badgeStyles[i] = cachedBadgeStyle{
			colors: colors,
			style: BaseStyle.
				Foreground(colors.Fg).
				Background(colors.Bg).
				Padding(0, 1),
		}
	}

	agentRegistry.accentStyles = make([]lipgloss.Style, len(accentColors))
	for i, c := range accentColors {
		agentRegistry.accentStyles[i] = BaseStyle.Foreground(c)
	}
}

// InvalidateAgentColorCache rebuilds the cached agent styles.
// Call this after a theme change so colors are recalculated against the new background.
func InvalidateAgentColorCache() {
	agentRegistry.Lock()
	defer agentRegistry.Unlock()

	rebuildAgentColorCache()
}

// lookupAgentIndex returns the palette index for the given agent name
// and whether the agent was found. Must be called with agentRegistry.RLock held.
func lookupAgentIndex(agentName string) (int, bool) {
	idx, ok := agentRegistry.indices[agentName]
	return idx, ok
}

// AgentBadgeColorsFor returns the badge foreground/background colors for a given agent name.
func AgentBadgeColorsFor(agentName string) AgentBadgeColors {
	agentRegistry.RLock()
	defer agentRegistry.RUnlock()

	idx, ok := lookupAgentIndex(agentName)
	if !ok || len(agentRegistry.badgeStyles) == 0 {
		return fallbackBadgeColors
	}

	return agentRegistry.badgeStyles[idx%len(agentRegistry.badgeStyles)].colors
}

// AgentBadgeStyleFor returns a lipgloss badge style colored for the given agent.
func AgentBadgeStyleFor(agentName string) lipgloss.Style {
	agentRegistry.RLock()
	defer agentRegistry.RUnlock()

	idx, ok := lookupAgentIndex(agentName)
	if !ok || len(agentRegistry.badgeStyles) == 0 {
		return fallbackBadgeStyle
	}

	return agentRegistry.badgeStyles[idx%len(agentRegistry.badgeStyles)].style
}

// AgentAccentStyleFor returns a foreground-only style for agent names (used in sidebar).
func AgentAccentStyleFor(agentName string) lipgloss.Style {
	agentRegistry.RLock()
	defer agentRegistry.RUnlock()

	idx, ok := lookupAgentIndex(agentName)
	if !ok || len(agentRegistry.accentStyles) == 0 {
		return fallbackAccentStyle
	}

	return agentRegistry.accentStyles[idx%len(agentRegistry.accentStyles)]
}
