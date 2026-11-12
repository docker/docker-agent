package chat

import (
	"strings"
	"unicode/utf8"
)

// MaxSanitizedFieldBytes is the canonical UTF-8-safe upper bound applied to
// every sanitized display-name and MIME-type field (see
// [SanitizeDisplayName] and the MIME sanitizer in pkg/runtime), independent
// of the separate, larger bound applied to a fully formatted placeholder or
// warning line. It exists so a single overlong provider-supplied field
// cannot itself balloon persisted metadata, session history, or prompts.
const MaxSanitizedFieldBytes = 128

// TruncateUTF8Bytes returns the longest prefix of s that is at most max
// bytes and still valid UTF-8 — it never splits a multi-byte rune, so the
// result is always safe to display or re-encode. Used by every canonical
// sanitizer (display name, MIME type, and formatted placeholder/warning
// text) to enforce a byte bound without corrupting non-ASCII content.
func TruncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// SanitizeDisplayName neutralizes a provider-supplied display name before
// it is stored in a [Document] or any other session-visible metadata. The
// name is untrusted input (e.g. Gemini's InlineData.DisplayName): it is
// never used verbatim to build a filesystem path (path-bearing consumers
// validate the requested name themselves), but it does end up in
// UI, warnings, placeholder text, and interpolated harness/prompt text
// (see pkg/runtime/harness.go's "<role>...</role>"-delimited blocks), so
// it must not carry control characters, path separators, traversal-like
// sequences, or angle brackets that could confuse a terminal, log line,
// forge a fake XML/tag boundary, or trick a human copy-pasting it into a
// shell.
//
// Control characters, path separators, and '<'/'>' are rewritten to '_';
// any residual ".." sequence (which could still read as a traversal hint
// even without separators, e.g. "..name") is rewritten too. The result is
// capped at [MaxSanitizedFieldBytes] (UTF-8-safe) and trimmed of
// surrounding whitespace. An empty or all-whitespace input returns "" —
// callers are responsible for substituting their own fallback name.
func SanitizeDisplayName(name string) string {
	name = strings.TrimSpace(name)

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('_')
		case r == '<' || r == '>':
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	sanitized := b.String()
	for strings.Contains(sanitized, "..") {
		sanitized = strings.ReplaceAll(sanitized, "..", "_")
	}
	sanitized = TruncateUTF8Bytes(strings.TrimSpace(sanitized), MaxSanitizedFieldBytes)
	return strings.TrimSpace(sanitized)
}
