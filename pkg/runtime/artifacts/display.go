package artifacts

import (
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

// FallbackDisplayName and FallbackMimeType are the canonical, deterministic
// substitutes used whenever a provider supplies an empty, unnamed, or
// syntactically invalid display name or MIME type. Every caller that
// surfaces this metadata (placeholders, warnings) must use exactly these
// values rather than inventing its own fallback text.
const (
	FallbackDisplayName = "generated media"
	FallbackMimeType    = "application/octet-stream"
)

// MaxWarningBytes bounds every fully formatted placeholder or
// warning line after interpolation, independent of the smaller
// [chat.MaxSanitizedFieldBytes] bound already applied to each individual
// name/MIME field: it is a defense-in-depth backstop against amplification
// through combined/duplicated fields, not something normal (already
// field-bounded) input is expected to hit.
const MaxWarningBytes = 512

// mimeTypePattern is the conservative MIME syntax [SanitizeMimeType]
// requires: a bare type/subtype pair (no parameters like "; charset=...",
// which generated-media MIME types never carry) built only from the
// characters RFC 6838 permits in a token, so nothing that could read as a
// delimiter, whitespace, or markup can ever survive sanitization.
var mimeTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*$`)

// SanitizeMimeType is [chat.SanitizeDisplayName]'s narrower counterpart
// for a MIME type value: control characters and newlines are neutralized
// first, the result is capped at [chat.MaxSanitizedFieldBytes], and it
// must then match [mimeTypePattern] — a bare, conservative type/subtype
// pair — or the entire value is discarded in favor of [FallbackMimeType].
// This is stricter than [chat.SanitizeDisplayName] (which rewrites
// individual bad characters and keeps the rest): a MIME type has no
// legitimate free-text content, so anything that fails the conservative
// syntax check is untrustworthy as a whole, not just in the specific
// characters it used to smuggle a fake log line or escape sequence.
func SanitizeMimeType(mimeType string) string {
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
		return FallbackMimeType
	}
	return sanitized
}
