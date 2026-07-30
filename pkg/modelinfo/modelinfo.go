// Package modelinfo centralizes every model-specific behavior decision and
// capability query used by docker-agent's provider clients.
//
// Some providers must specialize their behavior depending on the underlying
// model: pick OpenAI's Responses API for o-series and gpt-5, switch Claude
// Opus 4.6/4.7/4.8/5 to adaptive thinking, use level-based thinking for Gemini 3+,
// auto-enable interleaved thinking for any Claude model regardless of host
// (Anthropic, Bedrock, Vertex AI Model Garden), decide which attachment MIME
// types can be forwarded natively, and so on.
//
// Rather than scattering name-pattern checks across the codebase, every such
// predicate lives here, with a name that describes the *capability* (not the
// version) and a doc comment that explains *why* the behavior is needed.
//
// # Two layers
//
//   - "Is*" predicates take a bare model identifier and use stable name
//     patterns. They are zero-allocation and safe to call on the request hot
//     path.
//   - Lookup helpers (LookupFamily, IsClaudeFamily, [LoadCaps], ...) use
//     the models.dev database via a [modelsdev.Store] when richer information
//     is needed (e.g. detecting Claude across providers, determining
//     attachment MIME-type support). They are intended for config-resolution
//     paths, not per-request paths.
//
// # Adding a new model
//
// New members of an existing family inherit behavior automatically as long as
// their model identifiers follow the family's naming convention; in that case
// no code change is needed. New behavior categories belong in this package,
// expressed as a capability-named predicate with a comment that explains the
// underlying API rule.
package modelinfo

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// SupportsResponsesAPI reports whether an OpenAI model should be served via
// the Responses API rather than the legacy Chat Completions API.
//
// The Responses API is the forward path for newer OpenAI models: gpt-4.1,
// the o-series (o1/o3/o4), gpt-5 and later generations (gpt-6+, matched via
// [gptGeneration] so a naming convention rather than a per-id list picks up
// new generations), and Codex variants. Older models stay on Chat
// Completions for compatibility.
func SupportsResponsesAPI(modelID string) bool {
	m := normalizeOpenAI(modelID)
	switch {
	case strings.HasPrefix(m, "gpt-4.1"),
		strings.HasPrefix(m, "gpt-5"),
		strings.HasPrefix(m, "codex"),
		strings.Contains(m, "-codex"):
		return true
	}
	if maj, _, ok := gptGeneration(m); ok && maj >= 6 {
		return true
	}
	return isOSeries(m)
}

// SupportsDeferredTools reports whether a first-party model supports loading
// tool definitions at a point in the transcript without invalidating its cache.
func SupportsDeferredTools(provider, modelID string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "openai", "chatgpt":
		m := normalizeOpenAI(modelID)
		// gpt-6 is the next generation of the gpt-5.6 trio, which already
		// supports deferred tool loading; every future generation inherits
		// the capability without a per-id allow-list entry.
		if maj, _, ok := gptGeneration(m); ok && maj >= 6 {
			return true
		}
		switch m {
		case "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra":
			return true
		case "gpt-5.4-pro":
			return provider == "openai"
		default:
			return false
		}
	case "anthropic":
		m := normalize(modelID)
		if strings.Contains(m, "haiku") {
			return false
		}
		if strings.Contains(m, "fable") {
			return true
		}
		major, minor, ok := claudeOpusSonnetVersion(m)
		return ok && (major > 4 || major == 4 && minor >= 5)
	default:
		return false
	}
}

// UsesReasoningEffort reports whether an OpenAI model accepts the
// `reasoning.effort` API parameter.
//
// All reasoning-capable OpenAI models do (gpt-5, gpt-6+, the o-series),
// except the gpt-5-chat variants which are non-reasoning chat models at the
// API level. This exclusion is NOT (yet) generation-agnostic: a future
// "gpt-6-chat"-style non-reasoning id would need its own prefix check added
// here, since the maj >= 6 branch below returns true unconditionally.
func UsesReasoningEffort(modelID string) bool {
	m := normalizeOpenAI(modelID)
	if strings.HasPrefix(m, "gpt-5-chat") {
		return false
	}
	if maj, _, ok := gptGeneration(m); ok && maj >= 6 {
		return true
	}
	return isOSeries(m) || strings.HasPrefix(m, "gpt-5")
}

// AlwaysReasons reports whether an OpenAI model always reasons internally
// and therefore needs a default thinking_budget when none is configured.
//
// The o1/o3/o4 reasoning families cannot operate without thinking; they are
// seeded with reasoning_effort=medium when no thinking_budget is supplied.
// gpt-5 is excluded: it can produce visible output without reasoning, so the
// default depends on the user's intent.
func AlwaysReasons(modelID string) bool {
	return isOSeries(normalizeOpenAI(modelID))
}

// adaptiveOnlyOpusPrefixes lists the bare Claude Opus model families that
// reject token-based thinking ([RejectsTokenThinking]) and instead require
// adaptive thinking. This is an API behavior quirk that models.dev does not
// describe, so it stays hard-coded here (unlike context windows, which come
// from the catalogue).
var adaptiveOnlyOpusPrefixes = []string{"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-5"}

// isAdaptiveOnlyOpus reports whether modelID names a Claude Opus model that
// rejects token-based thinking (e.g. claude-opus-4-6, claude-opus-5, or a
// dated variant like claude-opus-4-7-20251101). Bedrock-style identifiers
// such as "global.anthropic.claude-opus-5" are recognised by stripping the
// inference-profile prefix first.
func isAdaptiveOnlyOpus(modelID string) bool {
	m := normalize(modelID)
	if bare, ok := bedrockClaudeModelName(m); ok {
		m = bare
	}
	for _, prefix := range adaptiveOnlyOpusPrefixes {
		if m == prefix || strings.HasPrefix(m, prefix+"-") {
			return true
		}
	}
	return false
}

// RejectsTokenThinking reports whether an Anthropic Claude model rejects
// `thinking.type=enabled` (token-based extended thinking) and instead requires
// `thinking.type=adaptive`.
//
// Applies to Claude Opus 4.6, 4.7, 4.8, 5, and their dated variants (e.g.
// claude-opus-4-7-20251101, claude-opus-5-20260724). Bedrock-style identifiers
// such as "global.anthropic.claude-opus-5" are recognised too.
// For these models the agent transparently switches a token-based budget to
// adaptive thinking.
//
// See https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking
func RejectsTokenThinking(modelID string) bool {
	return isAdaptiveOnlyOpus(modelID)
}

// SupportsAdaptiveThinking reports whether an Anthropic Claude model accepts
// adaptive extended thinking (`thinking.type=adaptive`) together with the
// `output_config.effort` parameter.
//
// Adaptive thinking and effort levels arrived with the Claude 4.6 generation.
// Earlier models (Sonnet 4.5 and older, Haiku 4.5, Opus 4.5 and older, and all
// Claude 3.x) reject `thinking.type=adaptive`/`output_config.effort` with a 400
// and must use token-based extended thinking (`thinking.type=enabled`) instead.
//
// Supported: Opus 4.6/4.7/4.8/5 (which additionally reject token budgets, see
// [RejectsTokenThinking]), Sonnet 4.6, the Claude 5 families (e.g. Sonnet 5,
// Opus 5), and the codenamed frontier models (Fable, Mythos). Bedrock-style
// identifiers such as "global.anthropic.claude-sonnet-4-6" are recognised too.
//
// The set is a superset of [RejectsTokenThinking]: a model that rejects token
// budgets must accept adaptive thinking.
//
// See https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking
func SupportsAdaptiveThinking(modelID string) bool {
	m := normalize(modelID)
	if bare, ok := bedrockClaudeModelName(m); ok {
		m = bare
	}
	// Codenamed frontier models ship with adaptive thinking.
	if strings.Contains(m, "fable") || strings.Contains(m, "mythos") {
		return true
	}
	// Only Opus and Sonnet gained adaptive thinking; Haiku, Claude 3.x, and
	// non-Claude models do not parse and fall through to false.
	major, minor, ok := claudeOpusSonnetVersion(m)
	if !ok {
		return false
	}
	// Claude 5+ (Sonnet 5, ...) always support it; within the 4.x line only
	// 4.6 and later do.
	return major >= 5 || (major == 4 && minor >= 6)
}

// SupportsFullThinkingDisplay reports whether an Anthropic Claude model
// accepts `thinking.display: "display"` (full thinking blocks).
//
// The restriction arrived with the adaptive-thinking generation: models from
// Claude 4.6 onward (Opus/Sonnet 4.6+, the Claude 5 families, Fable, Mythos)
// only accept "summarized" and "omitted" and reject "display" with a 400.
// Older token-thinking models (Sonnet 4.5, Haiku 4.5, ...) still accept it.
func SupportsFullThinkingDisplay(modelID string) bool {
	return !SupportsAdaptiveThinking(modelID)
}

// claudeOpusSonnetVersion extracts the major and minor version of a normalized
// bare Opus/Sonnet id such as "claude-opus-4-6", "claude-sonnet-4.5", or
// "claude-sonnet-5". It reports ok=false for other families (Haiku, Claude 3.x
// like "claude-3-opus-20240229", non-Claude), so only Opus/Sonnet parse.
//
// The major must be one or two digits; a longer run is a date stamp, not a
// version, which excludes Claude 3.x ids where the family precedes the number
// ("claude-3-opus-..."). A minor after '-' or '.' is likewise capped at two
// digits so date-stamped 4.0 ids ("claude-opus-4-20250514") yield minor 0.
func claudeOpusSonnetVersion(m string) (major, minor int, ok bool) {
	for _, fam := range []string{"opus", "sonnet"} {
		_, rest, found := strings.Cut(m, fam+"-")
		if !found {
			continue
		}
		maj, w := leadingInt(rest)
		if w == 0 || w > 2 {
			return 0, 0, false
		}
		rest = rest[w:]
		if rest != "" && (rest[0] == '-' || rest[0] == '.') {
			if n, mw := leadingInt(rest[1:]); mw > 0 && mw <= 2 {
				minor = n
			}
		}
		return maj, minor, true
	}
	return 0, 0, false
}

// gptGeneration extracts the major and minor version of a normalized OpenAI
// GPT model id of the form "gpt-<major>[.<minor>][-suffix]", such as
// "gpt-6-astra", "gpt-6.1-foo", or "gpt-5.6-terra". A bare major with no dot
// ("gpt-6") yields minor 0. It reports ok=false for anything that isn't a
// GPT-5-or-later id: pre-gpt-5 families ("gpt-4.1"), non-GPT ids, and
// malformed/date-shaped runs (a major or minor wider than two digits, or a
// suffix glued on without a '-' boundary, e.g. "gpt-5.20260709" or
// "gpt-6foo").
//
// This is the family-agnostic sibling of [gptFiveMinor] in
// thinking_levels.go: callers that need to compare across GPT generations
// ("is this gpt-5.6 or later, including gpt-6+") use [atLeastGPT] built on
// top of this, while gptFiveMinor stays the narrower gpt-5.x-only gate whose
// callers and tests predate the gpt-6 family and must keep rejecting a bare
// "gpt-5" (no minor) exactly as before.
func gptGeneration(modelID string) (major, minor int, ok bool) {
	m := normalizeOpenAI(modelID)
	rest, found := strings.CutPrefix(m, "gpt-")
	if !found {
		return 0, 0, false
	}
	maj, w := leadingInt(rest)
	if w == 0 || w > 2 || maj < 5 {
		return 0, 0, false
	}
	rest = rest[w:]
	switch {
	case rest == "", rest[0] == '-':
		return maj, 0, true
	case rest[0] != '.':
		return 0, 0, false
	}
	mn, mw := leadingInt(rest[1:])
	if mw == 0 || mw > 2 {
		return 0, 0, false
	}
	if tail := rest[1+mw:]; tail != "" && tail[0] != '-' {
		return 0, 0, false
	}
	return maj, mn, true
}

// atLeastGPT reports whether modelID names an OpenAI GPT model at or above
// the given generation (major, minor), e.g. atLeastGPT(id, 5, 6) is the
// gpt-5.6-or-later threshold shared by several capability checks. It uses
// [gptGeneration], so every later GPT generation (gpt-6, gpt-7, ...)
// automatically satisfies any gpt-5.x-or-later threshold without a code
// change; ok=false (non-GPT-5+ ids) reports false.
func atLeastGPT(modelID string, major, minor int) bool {
	maj, mn, ok := gptGeneration(modelID)
	if !ok {
		return false
	}
	return maj > major || (maj == major && mn >= minor)
}

// UsesThinkingLevel reports whether a Google Gemini model uses level-based
// thinking configuration (`thinkingLevel`) rather than token-based budgets.
//
// Gemini 3+ models always reason and only accept ThinkingLevel; older
// Gemini 2.5 models accept the legacy ThinkingBudget tokens.
//
// Matches both "gemini-3-<family>" and "gemini-3.X-<family>" patterns.
func UsesThinkingLevel(modelID string) bool {
	m := normalize(modelID)
	if !strings.HasPrefix(m, "gemini-3") {
		return false
	}
	rest := m[len("gemini-3"):]
	if rest == "" {
		return false
	}
	switch rest[0] {
	case '-':
		return true
	case '.':
		return strings.Contains(rest, "-")
	}
	return false
}

// IsBedrockClaudeID reports whether a model identifier looks like an Anthropic
// Claude model on AWS Bedrock.
//
// Bedrock model IDs are prefixed with "anthropic." or with a regional
// inference profile such as "global.anthropic." or "us.anthropic.".
//
// Prefer [IsClaude] for cross-provider checks: this helper exists so callers
// in the Bedrock path can avoid touching the models.dev store.
func IsBedrockClaudeID(modelID string) bool {
	_, ok := bedrockClaudeModelName(normalize(modelID))
	return ok
}

// bedrockClaudeModelName returns the bare Claude model name for a Bedrock-style
// identifier ("anthropic.claude-...", optionally preceded by a single regional
// inference profile such as "global." or "us."). The input must already be
// normalized. Returns ("", false) for non-Bedrock IDs; ARN-style identifiers
// are not handled.
func bedrockClaudeModelName(m string) (string, bool) {
	if bare, ok := strings.CutPrefix(m, "anthropic."); ok && strings.HasPrefix(bare, "claude-") {
		return bare, true
	}
	// Strip a single regional prefix (us., eu., apac., global., ...).
	if i := strings.IndexByte(m, '.'); i > 0 {
		if bare, ok := strings.CutPrefix(m[i+1:], "anthropic."); ok && strings.HasPrefix(bare, "claude-") {
			return bare, true
		}
	}
	return "", false
}

// IsClaude reports whether a model belongs to the Claude family, regardless
// of provider (Anthropic, AWS Bedrock, GCP Vertex AI Model Garden, ...).
//
// Resolution order:
//  1. The models.dev database, when [store] is non-nil and the model is
//     registered: the family is checked against [IsClaudeFamily].
//  2. Provider-specific name patterns (Bedrock-style IDs).
//  3. A bare "claude-" prefix on the model name.
//
// Pass a nil store to skip the models.dev lookup entirely (the name-pattern
// fallback still works, which is fine for the common case).
func IsClaude(ctx context.Context, store *modelsdev.Store, id modelsdev.ID) bool {
	if family := LookupFamily(ctx, store, id); family != "" {
		return IsClaudeFamily(family)
	}
	if IsBedrockClaudeID(id.Model) {
		return true
	}
	return strings.HasPrefix(normalize(id.Model), "claude-")
}

// LookupFamily returns the canonical model family identifier from models.dev
// (e.g. "claude-opus", "claude-sonnet", "gemini-pro", "o", "o-mini", "gpt").
//
// Returns "" when the store is nil, the id is incomplete, or the
// model is not registered in the database. Callers that want a non-empty
// answer for unknown models should fall back to a name-pattern heuristic.
func LookupFamily(ctx context.Context, store *modelsdev.Store, id modelsdev.ID) string {
	if store == nil || !id.IsValid() {
		return ""
	}
	m, err := store.GetModel(ctx, id)
	if err != nil || m == nil {
		return ""
	}
	return m.Family
}

// IsClaudeFamily reports whether a models.dev family identifier corresponds
// to one of the Claude families (claude-opus, claude-sonnet, claude-haiku,
// claude-instant, ...). Returns false for the empty string.
func IsClaudeFamily(family string) bool {
	return strings.HasPrefix(family, "claude-")
}

// openAIQualifierPrefix is the gateway convention (Vercel AI Gateway,
// OpenRouter, ...) for vendor-qualifying a model id, e.g.
// "openai/gpt-5.6-sol". [normalizeOpenAI] strips it so OpenAI-specific
// name-pattern predicates recognize the underlying OpenAI model regardless of
// which gateway fronts it. Other vendor qualifiers (DMR's "ai/qwen3",
// OpenRouter's "meta-llama/...") are left untouched: they name the model
// itself, not a wrapper around an OpenAI id.
const openAIQualifierPrefix = "openai/"

// normalize returns the lowercased, whitespace-trimmed model identifier used
// by every name-pattern predicate in this package. Provider-qualified ids as
// used by gateways and aggregators ("openai/gpt-5-nano" on OpenRouter,
// "openrouter/anthropic/claude-sonnet-4-6" in custom configs) are reduced to
// their last path segment: the predicates match on the bare model name, and
// without this a prefixed id would silently disable model-specific behavior
// (e.g. a thinking_budget on "anthropic/claude-opus-4.6" was never recognized
// as adaptive-thinking-capable).
//
// The one exception is a trailing "openai/<model>" pair: it is an explicit
// vendor qualifier (see [openAIQualifierPrefix]), so it is preserved rather
// than stripped down to <model>. Stripping it unconditionally would let a
// qualified id naming a *different* vendor's model behind an OpenAI-labeled
// gateway slug — "openai/claude-opus-4-7", "vercel/openai/gemini-3-pro" — fall
// through to the bare "claude-"/"gemini-" prefix checks and be misclassified.
// OpenAI-specific predicates still get the bare name: they call
// [normalizeOpenAI], which strips that preserved "openai/" pair afterwards.
func normalize(modelID string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	prefix, last, ok := strings.CutLast(m, "/")
	if !ok {
		return m
	}
	prevSeg := prefix
	if _, seg, found := strings.CutLast(prefix, "/"); found {
		prevSeg = seg
	}
	if prevSeg == "openai" {
		return openAIQualifierPrefix + last
	}
	return last
}

// normalizeOpenAI is [normalize] plus stripping of a leading "openai/" gateway
// qualifier (see [openAIQualifierPrefix]). OpenAI-specific name-pattern
// predicates (Responses API routing, reasoning-effort detection, gpt-5.x
// minor-version parsing) call this so a gateway-qualified id — Vercel AI
// Gateway's "openai/gpt-5.6-sol", OpenRouter's equivalent, ... — is recognized
// as the underlying OpenAI model regardless of which gateway fronts it.
// Non-OpenAI predicates (Claude, Gemini, ...) must keep calling [normalize]
// instead, or a qualified id naming a *different* vendor's model would be
// misclassified once the prefix is removed.
func normalizeOpenAI(modelID string) string {
	return stripOpenAIQualifier(normalize(modelID))
}

// stripOpenAIQualifier removes a leading "openai/" gateway qualifier from an
// already-[normalize]d model id, if present.
func stripOpenAIQualifier(m string) string {
	if rest, ok := strings.CutPrefix(m, openAIQualifierPrefix); ok {
		return rest
	}
	return m
}

// HasOpenAIQualifier reports whether modelID is explicitly vendor-qualified
// with an "openai/" prefix, the convention gateways (Vercel AI Gateway,
// OpenRouter, ...) use to route a specific vendor's model through a shared
// endpoint (e.g. "openai/gpt-5.6-sol"). It is the provider-agnostic signal
// that a model is genuinely OpenAI's, independent of which alias/gateway
// provider fronts it; [github.com/docker/docker-agent/pkg/model/provider]
// uses it to gate OpenAI-only behavior (like gpt-5.6's real "none" reasoning
// effort) onto the right providers.
func HasOpenAIQualifier(modelID string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelID)), openAIQualifierPrefix)
}

// openAIVendorProviders are the provider names whose models are genuinely
// OpenAI's own, as opposed to a third-party vendor that merely speaks the
// OpenAI-compatible wire protocol through a built-in alias. See
// [IsOpenAIVendor].
var openAIVendorProviders = map[string]bool{
	"openai":  true,
	"chatgpt": true,
	"azure":   true,
}

// IsOpenAIVendor reports whether a (provider, modelID) pair genuinely names an
// OpenAI vendor endpoint, as opposed to a third-party provider that merely
// speaks the OpenAI-compatible wire protocol (xai, mistral, and other
// OpenAI-compatible aliases whose models are NOT OpenAI's own).
//
// True for:
//   - the direct "openai" provider, "chatgpt" (the ChatGPT/Codex backend), and
//     "azure" (Azure OpenAI Service hosts OpenAI's own models)
//   - any provider when modelID is explicitly vendor-qualified with "openai/"
//     (the convention gateways such as Vercel AI Gateway or OpenRouter use to
//     route a specific vendor's model through a shared endpoint, e.g.
//     "openai/gpt-5.6-sol"), regardless of which provider/alias fronts it
//
// This is the shared, import-cycle-free core of the vendor check, used to
// gate OpenAI-only behavior (gpt-5.6's real "none" reasoning effort, its
// xhigh/max effort tiers routed through a gateway, ...) consistently across
// pkg/model/provider's defaults pipeline, the OpenAI client, and this
// package's own effort-family selection ([SupportedThinkingLevels]).
// pkg/model/provider layers one more exclusion on top for custom providers
// (providers: section) whose registered name collides with its built-in
// alias registry — that registry lives above modelinfo in the import graph,
// so it cannot be folded in here.
func IsOpenAIVendor(provider, modelID string) bool {
	if openAIVendorProviders[strings.ToLower(strings.TrimSpace(provider))] {
		return true
	}
	return HasOpenAIQualifier(modelID)
}

// isOSeries reports whether the (already-normalized) identifier names an
// OpenAI o-series reasoning model (o1/o3/o4 and their variants).
func isOSeries(m string) bool {
	return strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4")
}

// ---------------------------------------------------------------------------
// Attachment MIME-type capabilities
// ---------------------------------------------------------------------------

// ModelCapabilities describes what MIME types a given model can accept as
// document attachments.
type ModelCapabilities struct {
	supportsImage bool
	supportsPDF   bool
	supportsAudio bool
	supportsVideo bool
}

// SupportsImage reports whether the model accepts image attachments.
func (mc ModelCapabilities) SupportsImage() bool {
	return mc.supportsImage
}

// SupportsPDF reports whether the model accepts application/pdf attachments.
func (mc ModelCapabilities) SupportsPDF() bool {
	return mc.supportsPDF
}

// SupportsAudio reports whether the model accepts audio attachments.
func (mc ModelCapabilities) SupportsAudio() bool {
	return mc.supportsAudio
}

// SupportsVideo reports whether the model accepts video attachments.
func (mc ModelCapabilities) SupportsVideo() bool {
	return mc.supportsVideo
}

// Supports reports whether the model can accept an attachment with the given
// MIME type.
//
// Only five content families are recognised:
//   - image/* → requires the models.dev "image" input modality
//   - application/pdf → requires the models.dev "pdf" input modality
//   - audio/* → requires the models.dev "audio" input modality
//   - video/* → requires the models.dev "video" input modality
//   - text/* → always accepted (TXT envelope is universally safe)
//
// Everything else (Office binaries, …) returns false.
func (mc ModelCapabilities) Supports(mimeType string) bool {
	mt := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(mt, "image/"):
		return mc.supportsImage
	case mt == "application/pdf":
		return mc.supportsPDF
	case strings.HasPrefix(mt, "audio/"):
		return mc.supportsAudio
	case strings.HasPrefix(mt, "video/"):
		return mc.supportsVideo
	case strings.HasPrefix(mt, "text/"):
		return true
	default:
		return false
	}
}

// loadCapsTimeout is the maximum time allowed for a models.dev capability lookup.
const loadCapsTimeout = 10 * time.Second

// DefaultAnthropicContextLimit is the context window assumed for a Claude
// model only when models.dev has no entry for it AND no store is available —
// a degenerate, last-resort case. Claude 3.5 through 4.x all expose at least a
// 200k-token window, so it is a safe conservative floor for clamping retries.
//
// Model-specific windows (e.g. the 1M window of Claude Fable and Opus 4.6+)
// are NOT special-cased here: they come from the embedded models.dev snapshot,
// which is always available (even offline) and refreshed at build time. See
// [ContextLimit].
const DefaultAnthropicContextLimit = 200000

// ContextLimit returns the context-window size (in tokens) for a model.
//
// It prefers the models.dev catalogue entry for id; when the store is nil,
// the model is unknown, or the catalogue reports no context limit, it falls
// back to fallback. Callers that have no sensible fallback should pass 0 and
// treat a 0 result as "unknown".
//
// The supplied ctx is wrapped with loadCapsTimeout so the lookup stays
// cancellable with the caller and the underlying models.dev load is bounded.
// Note that the first lookup may serialize behind a shared store load: the
// timeout bounds the load itself, not time spent waiting for the store lock.
func ContextLimit(ctx context.Context, store *modelsdev.Store, id modelsdev.ID, fallback int64) int64 {
	if store == nil {
		return fallback
	}

	ctx, cancel := context.WithTimeout(ctx, loadCapsTimeout)
	defer cancel()

	model, err := store.GetModel(ctx, id)
	if err != nil || model == nil || model.Limit.Context <= 0 {
		return fallback
	}
	return int64(model.Limit.Context)
}

// CapsOverride is an explicit, user-declared attachment capability set that
// takes precedence over the models.dev catalogue. It is the modelinfo-level
// representation of a config capability override, deliberately free of any
// config-package dependency so modelinfo stays at the bottom of the import
// graph (it must not import pkg/config).
type CapsOverride struct {
	Image bool
	PDF   bool
	Audio bool
	Video bool
}

// ResolveCaps returns the model's attachment capabilities, preferring an
// explicit override when one is supplied and otherwise consulting models.dev
// via [LoadCaps]. A nil override reproduces plain [LoadCaps] behaviour, so it
// is safe to thread a nil override through every call site.
//
// The override is the escape hatch for models the catalogue does not describe
// correctly (custom OpenAI-compatible providers, local models, dropped model
// versions); see [github.com/docker/docker-agent/pkg/config/latest.CapabilitiesConfig].
func ResolveCaps(ctx context.Context, store *modelsdev.Store, id modelsdev.ID, override *CapsOverride) ModelCapabilities {
	if override != nil {
		return CapsWith(override.Image, override.PDF, override.Audio, override.Video)
	}
	return LoadCaps(ctx, store, id)
}

// capsMissLogged dedupes the "model not in models.dev" diagnostic so a given
// model is reported at most once per process rather than on every request.
var capsMissLogged sync.Map

// warnCapsLookupMiss emits a one-shot diagnostic when a model is absent from
// models.dev, distinguishing this recoverable misconfiguration from the silent
// text-only fallback it used to cause. It points the user at the config escape
// hatch so attachments can be restored. See issue #2741.
func warnCapsLookupMiss(ctx context.Context, id modelsdev.ID, cause error) {
	if _, dup := capsMissLogged.LoadOrStore(id.String(), struct{}{}); dup {
		return
	}
	slog.WarnContext(ctx,
		"modelinfo: model not found in models.dev; assuming text-only, so image and PDF "+
			"attachments will be dropped. If this model does accept attachments, declare them "+
			"in the agent config (models.<name>.capabilities: {image: true, pdf: true}) to "+
			"override capability detection.",
		"model", id.String(), "cause", cause)
}

// LoadCaps fetches (or returns from cache) the capability record for the given
// model ID using the provided store.
//
// When the store is nil or the model is not found, LoadCaps returns a
// conservative capability set that only allows text MIME types. A models.dev
// miss is logged once per model via [warnCapsLookupMiss] so the degraded
// behaviour is diagnosable rather than silent.
//
// The supplied ctx is wrapped with loadCapsTimeout so the lookup stays
// cancellable with the caller and the underlying models.dev load is bounded.
// Note that the first lookup may serialize behind a shared store load: the
// timeout bounds the load itself, not time spent waiting for the store lock.
func LoadCaps(ctx context.Context, store *modelsdev.Store, id modelsdev.ID) ModelCapabilities {
	if store == nil {
		return ModelCapabilities{}
	}

	ctx, cancel := context.WithTimeout(ctx, loadCapsTimeout)
	defer cancel()

	model, err := store.GetModel(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "modelinfo: models.dev lookup timed out, using conservative caps",
				"model", id.String(), "timeout", loadCapsTimeout)
		} else {
			warnCapsLookupMiss(ctx, id, err)
		}
		return ModelCapabilities{}
	}

	return capsFromModalities(model.Modalities.Input)
}

// ToolCallSupport is the models.dev catalogue's tri-state tool-call capability.
type ToolCallSupport uint8

const (
	ToolCallSupportUnknown ToolCallSupport = iota
	ToolCallUnsupported
	ToolCallSupported
)

// ResolveToolCallSupport reports whether models.dev says a model supports tool
// calls. Missing catalogue data remains unknown rather than being treated as a
// negative capability claim.
func ResolveToolCallSupport(ctx context.Context, store *modelsdev.Store, id modelsdev.ID) ToolCallSupport {
	if store == nil {
		return ToolCallSupportUnknown
	}

	ctx, cancel := context.WithTimeout(ctx, loadCapsTimeout)
	defer cancel()

	model, err := store.GetModel(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "modelinfo: models.dev tool-call lookup timed out, leaving support unknown",
				"model", id.String(), "timeout", loadCapsTimeout)
		} else {
			warnCapsLookupMiss(ctx, id, err)
		}
		return ToolCallSupportUnknown
	}
	if model.ToolCall {
		return ToolCallSupported
	}
	return ToolCallUnsupported
}

// ResolveOutputImage applies an explicit image-output override when present;
// otherwise it derives support from the models.dev output modalities. Missing
// catalogue data conservatively disables image output.
func ResolveOutputImage(ctx context.Context, store *modelsdev.Store, id modelsdev.ID, override *bool) bool {
	if override != nil {
		return *override
	}
	if store == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, loadCapsTimeout)
	defer cancel()

	model, err := store.GetModel(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "modelinfo: models.dev output lookup timed out, disabling image output",
				"model", id.String(), "timeout", loadCapsTimeout)
		} else {
			warnCapsLookupMiss(ctx, id, err)
		}
		return false
	}
	return hasOutputModality(model.Modalities.Output, "image")
}

func hasOutputModality(modalities []string, expected string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(modality, expected) {
			return true
		}
	}
	return false
}

// booleans it grants. Unknown modality names are ignored.
func capsFromModalities(input []string) ModelCapabilities {
	var mc ModelCapabilities
	for _, modality := range input {
		switch strings.ToLower(modality) {
		case "image":
			mc.supportsImage = true
		case "pdf":
			mc.supportsPDF = true
		case "audio":
			mc.supportsAudio = true
		case "video":
			mc.supportsVideo = true
		}
	}
	return mc
}

// ResolveCapsFromModel applies the same precedence contract as [ResolveCaps]
// — an explicit override wins, otherwise capabilities derive from the
// models.dev record — for callers that fetch models through their own store
// abstraction (e.g. the runtime's ModelStore interface) instead of a concrete
// [*modelsdev.Store]. A nil model yields the same conservative text-only
// default as a store miss in [LoadCaps].
func ResolveCapsFromModel(model *modelsdev.Model, override *CapsOverride) ModelCapabilities {
	if override != nil {
		return CapsWith(override.Image, override.PDF, override.Audio, override.Video)
	}
	if model == nil {
		return ModelCapabilities{}
	}
	return capsFromModalities(model.Modalities.Input)
}

// CapsWith constructs a ModelCapabilities value directly from booleans. This is
// intended for use in tests and provider implementations that need to create a
// capabilities value without hitting the network.
func CapsWith(supportsImage, supportsPDF, supportsAudio, supportsVideo bool) ModelCapabilities {
	return ModelCapabilities{
		supportsImage: supportsImage,
		supportsPDF:   supportsPDF,
		supportsAudio: supportsAudio,
		supportsVideo: supportsVideo,
	}
}
