// Package effort defines the canonical set of thinking-effort levels and
// provides per-provider mapping helpers. All provider packages should use
// this package instead of hard-coding effort strings.
package effort

import (
	"slices"
	"strings"
)

// Level represents a thinking effort level.
type Level string

// String returns the string representation of the Level.
func (l Level) String() string {
	return string(l)
}

const (
	None    Level = "none"
	Minimal Level = "minimal"
	Low     Level = "low"
	Medium  Level = "medium"
	High    Level = "high"
	XHigh   Level = "xhigh"
	Max     Level = "max"
)

// allLevels is the set of recognised non-adaptive effort levels.
var allLevels = map[Level]bool{
	None: true, Minimal: true, Low: true, Medium: true, High: true, XHigh: true, Max: true,
}

// adaptiveEfforts are the effort sub-levels valid after "adaptive/".
var adaptiveEfforts = map[Level]bool{
	Low: true, Medium: true, High: true, XHigh: true, Max: true,
}

// normalize lowercases and trims s for case-insensitive matching.
func normalize(s string) Level {
	return Level(strings.ToLower(strings.TrimSpace(s)))
}

// Parse normalises s (case-insensitive, trimmed) and returns the matching
// Level.  It returns ("", false) for unknown strings, adaptive values, and
// empty input.  Use [IsValid] for full validation including adaptive forms.
func Parse(s string) (Level, bool) {
	l := normalize(s)
	if allLevels[l] {
		return l, true
	}
	return "", false
}

// IsValid reports whether s is a recognised thinking_budget effort value.
// It accepts every [Level] constant, plain "adaptive", and the
// "adaptive/<effort>" form.
func IsValid(s string) bool {
	norm := normalize(s)
	if allLevels[norm] || norm == "adaptive" {
		return true
	}
	if after, ok := strings.CutPrefix(string(norm), "adaptive/"); ok {
		return adaptiveEfforts[Level(after)]
	}
	return false
}

// IsValidAdaptive reports whether sub is a valid effort for "adaptive/<sub>".
func IsValidAdaptive(sub string) bool {
	return adaptiveEfforts[normalize(sub)]
}

// ValidNames returns a human-readable list of accepted values, suitable for
// error messages.
func ValidNames() string {
	return "none, minimal, low, medium, high, xhigh, max, adaptive, adaptive/<effort>"
}

// ---------------------------------------------------------------------------
// Capability-driven level selection (TUI shift+tab cycling)
// ---------------------------------------------------------------------------

// orderedLevels is the canonical low-to-high ordering of selectable
// thinking-effort levels. None (thinking off) always sorts first.
var orderedLevels = []Level{None, Minimal, Low, Medium, High, XHigh, Max}

// explicitOnlyLevels are the top-tier levels that only a few models accept;
// they are offered only when a [LevelMap] explicitly declares support, so
// models without capability data never cycle onto a tier their API rejects.
var explicitOnlyLevels = map[Level]bool{XHigh: true, Max: true}

// LevelMap describes per-model thinking-level support. A level mapped to
// true is supported, a level mapped to false is explicitly unsupported, and
// an absent level is unspecified: supported by default, except for the
// explicit-only top tiers.
type LevelMap map[Level]bool

// SupportedLevels returns the ordered subset of thinking-effort levels a
// model supports, derived from its reasoning capability and level map.
// Models that cannot reason only support None. A nil map is valid and
// yields every level except the explicit-only top tiers.
func SupportedLevels(reasoning bool, m LevelMap) []Level {
	if !reasoning {
		return []Level{None}
	}
	supported := make([]Level, 0, len(orderedLevels))
	for _, l := range orderedLevels {
		v, present := m[l]
		if present && !v {
			continue
		}
		if !present && explicitOnlyLevels[l] {
			continue
		}
		supported = append(supported, l)
	}
	return supported
}

// Clamp maps requested onto the nearest level in supported. When requested
// is already supported it is returned unchanged. Otherwise the canonical
// ordering is scanned upward (toward higher effort) first and then downward,
// so a too-precise request degrades to the closest tier the model accepts.
// Falls back to the first supported level, or None when supported is empty.
func Clamp(supported []Level, requested Level) Level {
	if slices.Contains(supported, requested) {
		return requested
	}
	first := None
	if len(supported) > 0 {
		first = supported[0]
	}
	idx := slices.Index(orderedLevels, requested)
	if idx == -1 {
		return first
	}
	for i := idx + 1; i < len(orderedLevels); i++ {
		if slices.Contains(supported, orderedLevels[i]) {
			return orderedLevels[i]
		}
	}
	for i := idx - 1; i >= 0; i-- {
		if slices.Contains(supported, orderedLevels[i]) {
			return orderedLevels[i]
		}
	}
	return first
}

// NextSupportedLevel returns the level following current within supported,
// wrapping back to the first level. When current is not in supported (or
// supported is empty) the first supported level (or None) is returned.
func NextSupportedLevel(supported []Level, current Level) Level {
	if len(supported) == 0 {
		return None
	}
	if i := slices.Index(supported, current); i >= 0 {
		return supported[(i+1)%len(supported)]
	}
	return supported[0]
}

// ---------------------------------------------------------------------------
// Provider-specific mappings
// ---------------------------------------------------------------------------

// ForOpenAI returns the OpenAI reasoning_effort string for l.
// OpenAI accepts: none, minimal, low, medium, high, xhigh, max — none and max
// are gpt-5.6+ only, minimal is dropped on gpt-5.6+, and xhigh is gpt-5.2+.
// Per-model gating lives in pkg/modelinfo; this is the provider-wide
// vocabulary layer, so older models still reject none/xhigh/max API-side.
func ForOpenAI(l Level) (string, bool) {
	switch l {
	case None, Minimal, Low, Medium, High, XHigh, Max:
		return string(l), true
	default:
		return "", false
	}
}

// ForAnthropic returns the Anthropic output_config effort string for l.
// Anthropic accepts: low, medium, high, xhigh, max.
// xhigh is only supported by newer Claude models (e.g. Opus 4.7+).
// Minimal is mapped to low as the closest equivalent.
func ForAnthropic(l Level) (string, bool) {
	switch l {
	case Minimal:
		return string(Low), true
	case Low, Medium, High, XHigh, Max:
		return string(l), true
	default:
		return "", false
	}
}

// BedrockTokens maps l to a token budget for Bedrock Claude, which only
// supports token-based thinking budgets.
func BedrockTokens(l Level) (int, bool) {
	switch l {
	case Minimal:
		return 1024, true
	case Low:
		return 2048, true
	case Medium:
		return 8192, true
	case High:
		return 16384, true
	case XHigh, Max:
		return 32768, true
	default:
		return 0, false
	}
}

// ForGemini3 returns the Gemini 3 thinking-level string for l.
// Gemini 3 accepts: minimal, low, medium, high.
func ForGemini3(l Level) (string, bool) {
	switch l {
	case Minimal, Low, Medium, High:
		return string(l), true
	default:
		return "", false
	}
}
