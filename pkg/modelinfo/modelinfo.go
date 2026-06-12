// Package modelinfo centralizes every model-specific behavior decision and
// capability query used by docker-agent's provider clients.
//
// Some providers must specialize their behavior depending on the underlying
// model: pick OpenAI's Responses API for o-series and gpt-5, switch Claude
// Opus 4.6+ to adaptive thinking, use level-based thinking for Gemini 3+,
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
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// SupportsResponsesAPI reports whether an OpenAI model should be served via
// the Responses API rather than the legacy Chat Completions API.
//
// The Responses API is the forward path for newer OpenAI models: gpt-4.1,
// the o-series (o1/o3/o4), gpt-5 and Codex variants. Older models stay on
// Chat Completions for compatibility.
func SupportsResponsesAPI(modelID string) bool {
	m := normalize(modelID)
	switch {
	case strings.HasPrefix(m, "gpt-4.1"),
		strings.HasPrefix(m, "gpt-5"),
		strings.HasPrefix(m, "codex"),
		strings.Contains(m, "-codex"):
		return true
	}
	return isOSeries(m)
}

// UsesReasoningEffort reports whether an OpenAI model accepts the
// `reasoning.effort` API parameter.
//
// All reasoning-capable OpenAI models do, except the gpt-5-chat variants
// which are non-reasoning chat models at the API level.
func UsesReasoningEffort(modelID string) bool {
	m := normalize(modelID)
	if strings.HasPrefix(m, "gpt-5-chat") {
		return false
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
	return isOSeries(normalize(modelID))
}

// RejectsTokenThinking reports whether an Anthropic Claude model rejects
// `thinking.type=enabled` (token-based extended thinking) and instead requires
// `thinking.type=adaptive`.
//
// Claude Opus 4.7+ only supports adaptive thinking. Opus 4.6 also accepts
// adaptive thinking, but it still accepts token budgets, so it is not coerced.
//
// See https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking
func RejectsTokenThinking(modelID string) bool {
	m := normalizeClaudeName(modelID)
	return matchesClaudeFamilyAtLeast(m, "opus", 4, 7)
}

func matchesClaudeFamilyAtLeast(modelID, family string, major, minor int) bool {
	prefix := "claude-" + family + "-"
	if !strings.HasPrefix(modelID, prefix) {
		return false
	}
	rest := strings.TrimPrefix(modelID, prefix)
	parts := strings.Split(rest, "-")
	if len(parts) < 2 || len(parts[1]) != 1 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	if gotMajor != major {
		return gotMajor > major
	}
	return gotMinor >= minor
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
	m := normalize(modelID)
	if strings.HasPrefix(m, "anthropic.claude-") {
		return true
	}
	// Strip a single regional prefix (us., eu., apac., global., ...).
	if i := strings.IndexByte(m, '.'); i > 0 {
		return strings.HasPrefix(m[i+1:], "anthropic.claude-")
	}
	return false
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

// normalize returns the lowercased, whitespace-trimmed model identifier used
// by every name-pattern predicate in this package.
func normalize(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

func normalizeClaudeName(modelID string) string {
	m := normalize(modelID)
	if i := strings.Index(m, "claude-"); i >= 0 {
		return m[i:]
	}
	return m
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
}

// Supports reports whether the model can accept an attachment with the given
// MIME type.
//
// Only three content families are recognised:
//   - image/* → requires the models.dev "image" input modality
//   - application/pdf → requires the models.dev "pdf" input modality
//   - text/* → always accepted (TXT envelope is universally safe)
//
// Everything else (audio, video, Office binaries, …) returns false.
func (mc ModelCapabilities) Supports(mimeType string) bool {
	mt := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(mt, "image/"):
		return mc.supportsImage
	case mt == "application/pdf":
		return mc.supportsPDF
	case strings.HasPrefix(mt, "text/"):
		return true
	default:
		return false
	}
}

// loadCapsTimeout is the maximum time allowed for a models.dev capability lookup.
const loadCapsTimeout = 10 * time.Second

// DefaultAnthropicContextLimit is the context window assumed for most Claude
// models when models.dev has no entry for them. Claude 3.5 through 4.x all
// expose a 200k-token window, so it is a safe floor for clamping retries.
// Prefer [DefaultClaudeContextLimit], which special-cases newer families with
// larger windows.
const DefaultAnthropicContextLimit = 200000

// DefaultClaudeContextLimit returns the context window assumed for a Claude
// model when models.dev has no entry for it — typically a model released
// before the catalogue caught up. Claude Fable models expose a 1M-token
// window; everything else falls back to the standard 200k floor.
func DefaultClaudeContextLimit(modelID string) int64 {
	m := normalize(modelID)
	if m == "claude-fable" || strings.HasPrefix(m, "claude-fable-") {
		return 1_000_000
	}
	return DefaultAnthropicContextLimit
}

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

// LoadCaps fetches (or returns from cache) the capability record for the given
// model ID using the provided store.
//
// When the store is nil or the model is not found, LoadCaps returns a
// conservative capability set that only allows text MIME types.
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
		}
		return ModelCapabilities{}
	}

	var mc ModelCapabilities
	for _, input := range model.Modalities.Input {
		switch strings.ToLower(input) {
		case "image":
			mc.supportsImage = true
		case "pdf":
			mc.supportsPDF = true
		}
	}
	return mc
}

// CapsWith constructs a ModelCapabilities value directly from booleans. This is
// intended for use in tests and provider implementations that need to create a
// capabilities value without hitting the network.
func CapsWith(supportsImage, supportsPDF bool) ModelCapabilities {
	return ModelCapabilities{
		supportsImage: supportsImage,
		supportsPDF:   supportsPDF,
	}
}
