package modelinfo

import (
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/effort"
)

// SupportedThinkingLevels returns the ordered thinking-effort levels a
// reasoning-capable model accepts for user-selectable cycling. It combines
// the provider API's effort vocabulary with per-model gating of the optional
// top tiers: not every model accepts xhigh or max, and offering them blindly
// makes the API reject the request. Callers are expected to have already
// established that the model reasons at all.
func SupportedThinkingLevels(provider, modelID string) []effort.Level {
	return effort.SupportedLevels(true, thinkingLevelMap(provider, modelID))
}

// thinkingLevelMap builds the per-model capability map consumed by
// [effort.SupportedLevels].
func thinkingLevelMap(provider, modelID string) effort.LevelMap {
	family := providerFamily(provider)
	if family != "openai" && IsOpenAIVendor(provider, modelID) {
		// A gateway (Vercel AI Gateway, OpenRouter, ...) fronts many vendors'
		// models under one provider name; an explicit "openai/" qualifier on
		// the model id pins its effort vocabulary to OpenAI's regardless of
		// which gateway provider name is configured.
		family = "openai"
	}
	switch family {
	case "anthropic":
		// The Anthropic effort scale starts at low ([effort.ForAnthropic]
		// maps minimal onto low), so offering minimal would duplicate low.
		m := effort.LevelMap{effort.Minimal: false}
		for _, top := range anthropicTopEfforts(modelID) {
			m[top] = true
		}
		return m
	case "openai":
		m := effort.LevelMap{}
		for _, top := range openAITopEfforts(modelID) {
			m[top] = true
		}
		if openAIDropsMinimalEffort(modelID) {
			// gpt-5.6+ (including every later generation, e.g. gpt-6) dropped
			// minimal from its accepted efforts; offering it would make the
			// TUI's Shift+Tab cycle land on a value the API 400s on.
			m[effort.Minimal] = false
		}
		if len(m) == 0 {
			return nil
		}
		return m
	case "google":
		return nil
	default:
		// Unknown providers (e.g. dmr) get the conservative low/medium/high
		// scale; minimal is far from universally accepted.
		return effort.LevelMap{effort.Minimal: false}
	}
}

// providerFamily normalises a provider type onto the model family whose API
// defines the thinking-level vocabulary, tolerating aliases such as
// "amazon-bedrock" (hosting Anthropic models), "vertexai" (Gemini), or
// "chatgpt" (the Codex backend, which serves OpenAI models).
func providerFamily(providerType string) string {
	p := normalize(providerType)
	switch {
	case strings.Contains(p, "anthropic"), strings.Contains(p, "claude"), strings.Contains(p, "bedrock"):
		return "anthropic"
	case strings.Contains(p, "google"), strings.Contains(p, "gemini"), strings.Contains(p, "vertex"):
		return "google"
	case strings.Contains(p, "openai"), strings.Contains(p, "azure"), strings.Contains(p, "chatgpt"):
		return "openai"
	default:
		return p
	}
}

// anthropicTopEfforts returns the explicit-only effort tiers (xhigh and/or
// max) a Claude model accepts beyond the universal low/medium/high ladder, in
// canonical ascending order. It returns nil for models that top out at high.
//
// xhigh and max are independent capabilities, not a single ladder: Opus 4.6
// and Sonnet 4.6 accept max without xhigh, while Opus 4.7+, Fable 5 and
// Mythos 5 accept both. Returning the supported subset (rather than one "top"
// tier) lets the Shift+Tab cycle offer every tier a model really has.
//
// Authoritative per-level availability:
// https://platform.claude.com/docs/en/build-with-claude/effort
//
// Works on bare Anthropic IDs ("claude-opus-4-7", "claude-opus-4.7") as well
// as Bedrock-style IDs with regional prefixes ("us.anthropic.claude-opus-4-7").
func anthropicTopEfforts(modelID string) []effort.Level {
	hasXHigh, hasMax := anthropicTopTierSupport(normalize(modelID))
	switch {
	case hasXHigh && hasMax:
		return []effort.Level{effort.XHigh, effort.Max}
	case hasMax:
		return []effort.Level{effort.Max}
	case hasXHigh:
		return []effort.Level{effort.XHigh}
	default:
		return nil
	}
}

// anthropicTopTierSupport reports whether the normalized Claude model id m
// accepts the xhigh and max effort tiers. The capability matrix it encodes is
// quoted in [anthropicTopEfforts]'s authoritative reference.
func anthropicTopTierSupport(m string) (hasXHigh, hasMax bool) {
	if bare, ok := bedrockClaudeModelName(m); ok {
		m = bare
	}
	switch {
	case strings.Contains(m, "fable"):
		// Fable 5: both xhigh and max.
		return true, true
	case strings.Contains(m, "mythos"):
		// Mythos model ids are inferred from the effort reference (no
		// catalogue entry yet): every variant accepts max, and the full
		// release adds xhigh while the preview tops out at max.
		return !strings.Contains(m, "preview"), true
	}
	// Claude Opus 5+ ships the full effort ladder (xhigh and max), matching Opus 4.7+.
	if strings.Contains(m, "opus") {
		if major, _, ok := claudeOpusSonnetVersion(m); ok && major >= 5 {
			return true, true
		}
	}
	family, minor, ok := claudeFamilyMinor(m)
	if !ok {
		return false, false
	}
	switch family {
	case "opus":
		switch {
		case minor >= 7:
			return true, true
		case minor == 6:
			return false, true
		}
	case "sonnet":
		if minor >= 6 {
			return false, true
		}
	}
	return false, false
}

// claudeFamilyMinor extracts the family ("opus" or "sonnet") and minor version
// from a normalized Claude id of the form "...<family>-4-<minor>" or
// "...<family>-4.<minor>". It reports ok=false for other families and for
// date-stamped 4.0 ids such as "claude-opus-4-20250514", whose long digit run
// is a date, not a minor version.
func claudeFamilyMinor(m string) (family string, minor int, ok bool) {
	for _, fam := range []string{"opus", "sonnet"} {
		_, rest, found := strings.Cut(m, fam+"-4")
		if !found {
			continue
		}
		if rest == "" || (rest[0] != '-' && rest[0] != '.') {
			return "", 0, false
		}
		minor, width := leadingInt(rest[1:])
		if width == 0 || width > 2 {
			return "", 0, false
		}
		return fam, minor, true
	}
	return "", 0, false
}

// gptFiveMinor extracts the minor version from an OpenAI gpt-5.x model id,
// tolerating -sol/-terra/-luna and dated/snapshot suffixes appended after a
// hyphen (e.g. "gpt-5.6-sol" -> 6, "gpt-5.6-2026-07-09" -> 6). It reports
// ok=false for non-gpt-5.x ids and for malformed minors: the minor digit run
// is capped at two digits (a longer run is a date, not a minor version, e.g.
// "gpt-5.20260709"), and whatever follows it must be either the end of the
// string or a '-' — anything glued on directly without that boundary
// ("gpt-5.6foo", "gpt-5.6.20260709") is rejected rather than silently
// truncated.
func gptFiveMinor(modelID string) (minor int, ok bool) {
	m := normalizeOpenAI(modelID)
	const prefix = "gpt-5."
	rest, found := strings.CutPrefix(m, prefix)
	if !found {
		return 0, false
	}
	minor, width := leadingInt(rest)
	if width == 0 || width > 2 {
		return 0, false
	}
	if tail := rest[width:]; tail != "" && tail[0] != '-' {
		return 0, false
	}
	return minor, true
}

// openAITopEfforts returns the explicit-only effort tiers (xhigh and/or max)
// an OpenAI model accepts beyond the universal minimal/low/medium/high
// ladder. OpenAI's ladder is cumulative (unlike Anthropic's independent
// tiers): gpt-5.2+ adds xhigh, gpt-5.6+ adds max on top — and gpt-6 and every
// later generation inherit the full gpt-5.6+ ladder via [atLeastGPT], so a
// new GPT generation needs no code change here. The o-series and gpt-5/5.0/5.1
// top out at high.
func openAITopEfforts(modelID string) []effort.Level {
	switch {
	case atLeastGPT(modelID, 5, 6):
		return []effort.Level{effort.XHigh, effort.Max}
	case atLeastGPT(modelID, 5, 2):
		return []effort.Level{effort.XHigh}
	default:
		return nil
	}
}

// OpenAISupportsNoneEffort reports whether an OpenAI model accepts reasoning
// effort "none" as a real, honored value rather than a config-level "turn
// thinking off" sentinel. gpt-5.6 (Sol/Terra/Luna) is the first OpenAI
// family to do so. Exported: the defaults pipeline (pkg/model/provider) and
// the OpenAI client need the same gate to decide whether "none" survives
// normalization and is sent on the wire.
//
// Deliberately scoped to the gpt-5.x line via [gptFiveMinor] rather than
// [atLeastGPT]: gpt-6 (verified live against api.openai.com) rejects
// reasoning.effort "none" with a 400 ("Supported values are: 'low',
// 'medium', 'high', 'xhigh', and 'max'"), so this predicate must NOT widen
// to "gpt-5.6 or later" the way [openAIDropsMinimalEffort] and
// [openAITopEfforts] do — those two effects diverged when gpt-6 shipped.
func OpenAISupportsNoneEffort(modelID string) bool {
	minor, ok := gptFiveMinor(modelID)
	return ok && minor >= 6
}

// openAIDropsMinimalEffort reports whether an OpenAI model rejects reasoning
// effort "minimal", unlike [OpenAISupportsNoneEffort] this holds for every
// gpt-5.6-or-later generation including gpt-6. Verified live against
// api.openai.com (2026-09): gpt-5.6/-sol/-terra AND gpt-6-astra all 400 on
// reasoning.effort "minimal" ("Supported values are: 'none', 'low',
// 'medium', 'high', 'xhigh', and 'max'" for the gpt-5.6 line — "none" isn't
// in gpt-6-astra's list, see [OpenAISupportsNoneEffort]) — only the "none"
// value itself stops being honored past gpt-5.6.
func openAIDropsMinimalEffort(modelID string) bool {
	return atLeastGPT(modelID, 5, 6)
}

// OpenAIRejectsToolsWithReasoningEffort reports whether an OpenAI model
// rejects an explicit `reasoning_effort` sent alongside function tools on the
// Chat Completions endpoint (HTTP 400: "Function tools with reasoning_effort
// are not supported for <model> in /v1/chat/completions. To use function
// tools, use /v1/responses or set reasoning_effort to 'none'."). Verified
// live against api.openai.com (2026-09):
//
//   - gpt-5, gpt-5.1, gpt-5.2, and the o-series still accept an explicit
//     effort together with tools on this endpoint.
//   - gpt-5.4, gpt-5.4-mini, and gpt-5.5 reject an *explicit* effort with
//     tools, but accept the combination when the field is omitted entirely
//     (the model falls back to its implicit default effort).
//   - gpt-5.6 and every later generation (including gpt-6+, via
//     [atLeastGPT]) reject tools+reasoning on this endpoint outright, even
//     with the field omitted; only the Responses API supports the
//     combination for them.
//
// The OpenAI client uses this to drop reasoning_effort rather than send a
// value guaranteed to 400 when a Responses-capable model is forced onto
// Chat Completions (an explicit api_type or custom-provider override) with
// tools in the request.
func OpenAIRejectsToolsWithReasoningEffort(modelID string) bool {
	return atLeastGPT(modelID, 5, 4)
}

// OpenAISupportsExplicitPromptCache reports whether an OpenAI model accepts
// explicit prompt-cache breakpoints (`prompt_cache_breakpoint` content
// markers). gpt-5.6 (Sol/Terra/Luna) is the first OpenAI family to support
// them; gpt-6 and every later generation inherit the capability via
// [atLeastGPT]. Older models reject the field with HTTP 400, so the OpenAI
// client gates on this before putting breakpoints on the wire.
func OpenAISupportsExplicitPromptCache(modelID string) bool {
	return atLeastGPT(modelID, 5, 6)
}

// leadingInt parses the run of decimal digits at the start of s, returning
// its value and width. A zero width means s does not start with a digit.
func leadingInt(s string) (value, width int) {
	for width < len(s) && s[width] >= '0' && s[width] <= '9' {
		width++
	}
	if width == 0 {
		return 0, 0
	}
	n, err := strconv.Atoi(s[:width])
	if err != nil {
		return 0, 0
	}
	return n, width
}
