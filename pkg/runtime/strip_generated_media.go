package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
)

// BuiltinStripGeneratedMedia is the name of the runtime-shipped
// before_llm_call message transform that removes assistant-authored,
// model-generated media (materialized as a session artifact — see
// [chat.DocumentSource.ArtifactPath]) from outgoing provider history,
// keeping the surrounding text.
//
// This is the default no-resend policy for generated media (plan step 4,
// "Context bloat decision"): without it, every subsequent turn would
// re-encode and resend the same generated image bytes to the provider on
// every follow-up request, burning context budget for content the model
// already produced and the user already has. It runs unconditionally,
// independent of the model's capabilities — unlike
// [BuiltinStripUnsupportedModalities], which only strips media the
// current model cannot accept.
//
// User-attached documents (InlineData/InlineText) are never touched: only
// parts carrying an ArtifactPath — which is exclusively set for
// runtime-materialized, provider-generated media — are removed.
//
// It is registered to run BEFORE [BuiltinStripUnsupportedModalities] (see
// the registration order in runtime.go's New) so that a capability-less
// or unknown model never gets a chance to strip the same media part
// first, bypassing the placeholder logic below and leaving a
// media-only assistant turn with nothing at all.
const BuiltinStripGeneratedMedia = "strip_generated_media"

// generatedMediaPlaceholderPrefix is the stable, greppable marker at the
// start of every placeholder produced by [generatedMediaPlaceholderTexts].
// Kept as a distinguishable constant (rather than inlined into the format
// string) so tests and any future log-scraping can recognize a placeholder
// without depending on its exact wording.
const generatedMediaPlaceholderPrefix = "[Generated media omitted from history"

// fallbackDisplayName and fallbackMimeType are the canonical, deterministic
// substitutes used whenever a provider supplies an empty, unnamed, or
// syntactically invalid display name or MIME type. Every caller that
// surfaces this metadata (placeholders, warnings) must use exactly these
// values rather than inventing its own fallback text.
const (
	fallbackDisplayName = "generated media"
	fallbackMimeType    = "application/octet-stream"
)

// maxPlaceholderOrWarningBytes bounds every fully formatted placeholder or
// warning line after interpolation, independent of the smaller
// [chat.MaxSanitizedFieldBytes] bound already applied to each individual
// name/MIME field: it is a defense-in-depth backstop against amplification
// through combined/duplicated fields, not something normal (already
// field-bounded) input is expected to hit.
const maxPlaceholderOrWarningBytes = 512

// stripGeneratedMediaTransform is the [MessageTransform] registered under
// [BuiltinStripGeneratedMedia]. Unlike
// [LocalRuntime.stripUnsupportedModalitiesTransform], it needs no resolved
// capability set: the policy is unconditional, so it is a plain function
// rather than a method capturing runtime state.
//
// For every stripped artifact it appends one placeholder [chat.MessagePart]
// (Type text) naming the count, sanitized display name, and MIME type of
// the item that was removed — never just clearing MultiContent and setting
// Content alone, which would strand any provider converter that treats a
// non-empty MultiContent as authoritative (see recordAssistantMessage's own
// text-duplication comment in loop.go for why that matters here too). The
// same placeholder text is also mirrored into Content: Anthropic's
// message converters (client.go and beta_converter.go) build the assistant
// turn purely from Content and ignore MultiContent's text/document parts
// entirely, so a placeholder that only existed as a MultiContent part would
// be silently invisible to Anthropic specifically.
func stripGeneratedMediaTransform(ctx context.Context, _ *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
	result := make([]chat.Message, len(msgs))
	for i, msg := range msgs {
		result[i] = msg

		if msg.Role != chat.MessageRoleAssistant || len(msg.MultiContent) == 0 {
			continue
		}

		var filtered []chat.MessagePart
		var stripped []chat.MessagePart
		for _, part := range msg.MultiContent {
			if isGeneratedMediaPart(part) {
				stripped = append(stripped, part)
				continue
			}
			filtered = append(filtered, part)
		}

		if len(stripped) == 0 {
			continue
		}

		for _, part := range stripped {
			slog.DebugContext(ctx, "strip_generated_media: stripped generated artifact from outgoing history",
				"name", part.Document.Name, "mime_type", part.Document.MimeType)
		}

		texts := generatedMediaPlaceholderTexts(stripped)
		placeholders := make([]chat.MessagePart, len(texts))
		for j, text := range texts {
			placeholders[j] = chat.MessagePart{Type: chat.MessagePartTypeText, Text: text}
		}

		result[i].MultiContent = append(filtered, placeholders...)
		result[i].Content = mergeWithPlaceholder(msg.Content, texts)
	}
	return result, nil
}

// generatedMediaPlaceholderTexts builds one placeholder string per stripped
// artifact, each carrying its position/count and safe display metadata
// (sanitized name and MIME type). [materializeGeneratedMedia] already
// sanitizes Document.Name/MimeType before either is ever stored, but a
// persisted message loaded from an older session (or written by any
// future code path that forgets to) could still carry unsafe or raw
// values — this is the second, defense-in-depth sanitization pass the
// review calls for: never trust that upstream storage was sanitized,
// sanitize again at the point a value becomes user-visible. An empty or
// unnamed display name deterministically falls back to
// [fallbackDisplayName]; an empty or invalid MIME type deterministically
// falls back to [fallbackMimeType] — both are always shown, never omitted.
// The formatted result is capped at [maxPlaceholderOrWarningBytes] as a
// final backstop, independent of the smaller per-field bound already
// applied by the sanitizers themselves.
func generatedMediaPlaceholderTexts(stripped []chat.MessagePart) []string {
	total := len(stripped)
	texts := make([]string, total)
	for i, part := range stripped {
		name := fallbackDisplayName
		mimeType := fallbackMimeType
		if part.Document != nil {
			if safeName := chat.SanitizeDisplayName(part.Document.Name); safeName != "" {
				name = safeName
			}
			mimeType = sanitizeMimeType(part.Document.MimeType)
		}
		text := fmt.Sprintf("%s %d/%d: %s (%s)]", generatedMediaPlaceholderPrefix, i+1, total, name, mimeType)
		texts[i] = chat.TruncateUTF8Bytes(text, maxPlaceholderOrWarningBytes)
	}
	return texts
}

// mimeTypePattern is the conservative MIME syntax [sanitizeMimeType]
// requires: a bare type/subtype pair (no parameters like "; charset=...",
// which generated-media MIME types never carry) built only from the
// characters RFC 6838 permits in a token, so nothing that could read as a
// delimiter, whitespace, or markup can ever survive sanitization.
var mimeTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*$`)

// sanitizeMimeType is [chat.SanitizeDisplayName]'s narrower counterpart
// for a MIME type value: control characters and newlines are neutralized
// first, the result is capped at [chat.MaxSanitizedFieldBytes], and it
// must then match [mimeTypePattern] — a bare, conservative type/subtype
// pair — or the entire value is discarded in favor of [fallbackMimeType].
// This is stricter than [chat.SanitizeDisplayName] (which rewrites
// individual bad characters and keeps the rest): a MIME type has no
// legitimate free-text content, so anything that fails the conservative
// syntax check is untrustworthy as a whole, not just in the specific
// characters it used to smuggle a fake log line or escape sequence.
func sanitizeMimeType(mimeType string) string {
	var b strings.Builder
	b.Grow(len(mimeType))
	for _, r := range mimeType {
		if r < 0x20 || r == 0x7f {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	sanitized := chat.TruncateUTF8Bytes(strings.TrimSpace(b.String()), chat.MaxSanitizedFieldBytes)
	if !mimeTypePattern.MatchString(sanitized) {
		return fallbackMimeType
	}
	return sanitized
}

// mergeWithPlaceholder appends placeholder texts to the assistant's
// original text, mirroring what [stripGeneratedMediaTransform] appends to
// MultiContent so that a Content-only reader (Anthropic — see this file's
// package doc) sees exactly the same information. When original is empty
// (a media-only turn), the joined placeholders become the entire Content,
// which is guaranteed non-empty since there is always at least one
// stripped item by the time this is called.
func mergeWithPlaceholder(original string, placeholderTexts []string) string {
	joined := strings.Join(placeholderTexts, "\n")
	trimmed := strings.TrimSpace(original)
	if trimmed == "" {
		return joined
	}
	return trimmed + "\n" + joined
}

// isGeneratedMediaPart reports whether part is a runtime-materialized,
// model-generated artifact rather than a user attachment. ArtifactPath is
// only ever set by [materializeGeneratedMedia], so its presence is a
// sufficient marker.
func isGeneratedMediaPart(part chat.MessagePart) bool {
	return part.Type == chat.MessagePartTypeDocument &&
		part.Document != nil &&
		part.Document.Source.ArtifactPath != ""
}
