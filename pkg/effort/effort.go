// Package effort defines the canonical set of thinking-effort levels and
// provides per-provider mapping helpers. All provider packages should use
// this package instead of hard-coding effort strings.
package effort

import "strings"

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
// Provider-specific mappings
// ---------------------------------------------------------------------------

// ForOpenAI returns the OpenAI reasoning_effort string for l.
// OpenAI accepts: minimal, low, medium, high, xhigh.
func ForOpenAI(l Level) (string, bool) {
	switch l {
	case Minimal, Low, Medium, High, XHigh:
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
