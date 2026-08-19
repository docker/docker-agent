// Package attachment provides MIME-aware routing for document attachments.
//
// It defines how a chat.Document should be sent to a model: either dropped
// (unsupported), wrapped in a plain-text envelope (StrategyTXT), or encoded
// as inline base64 data (StrategyB64).
package attachment

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// Strategy describes how an attachment should be handled before sending to the
// provider.
type Strategy int

const (
	// StrategyDrop means the attachment is not supported by the model or has no
	// inline content, and should be silently skipped (with a log warning).
	StrategyDrop Strategy = iota

	// StrategyTXT means the attachment should be wrapped in a TXTEnvelope and
	// sent as plain text.  Used for text/* MIME types whose content is already
	// in Source.InlineText.
	StrategyTXT

	// StrategyB64 means the attachment content (Source.InlineData) should be
	// base64-encoded and sent as a native provider image/document block.
	StrategyB64
)

// Decide returns the routing Strategy for a document given the current model's
// capabilities.
//
// Algorithm:
//  1. If the model does not support the document's MIME type → (Drop, reason).
//  2. If Source.InlineData is non-empty → (B64, "").
//  3. If Source.InlineText is non-empty → (TXT, "").
//  4. Otherwise → (Drop, "no inline content").
func Decide(doc chat.Document, mc modelinfo.ModelCapabilities) (Strategy, string) {
	if !mc.Supports(doc.MimeType) {
		return StrategyDrop, fmt.Sprintf("model does not support MIME type %q", doc.MimeType)
	}
	if len(doc.Source.InlineData) > 0 {
		return StrategyB64, ""
	}
	if doc.Source.InlineText != "" {
		return StrategyTXT, ""
	}
	return StrategyDrop, "no inline content"
}

// TXTEnvelope wraps text content in an XML-like tag derived from the document
// name and MIME type.
//
// Example: a document named "report.md" with MIME "text/markdown" produces:
//
//	<document-report-md-text-markdown>
//	…body…
//	</document-report-md-text-markdown>
//
// # Delimiter safety
//
// The tag is a deterministic slug of the name and MIME type, so it is NOT a
// secret: both inputs are routinely attacker-influenced (a downloaded file, a
// fetched page), which means the tag is predictable to whoever supplied the
// content. The body is therefore defused — any occurrence of this envelope's own
// delimiter inside it is replaced — so content cannot close the region early and
// make injected text appear to come from outside it.
//
// The tag is deliberately kept deterministic rather than randomised per call: a
// per-call nonce would change the prompt prefix on every request and defeat
// provider prompt caching for the attachment.
func TXTEnvelope(name, mimeType, body string) string {
	slug := slugify(name + "-" + mimeType)
	tag := "document-" + slug
	return fmt.Sprintf("<%s>\n%s\n</%s>", tag, defuseDelimiters(body, tag), tag)
}

// delimiterPlaceholder replaces an envelope delimiter found inside a body. It is
// visible on purpose: silently dropping the text would hide the attempt, and an
// invisible substitution (a zero-width character) would be worse — it would look
// like a working delimiter to a human reading the transcript.
const delimiterPlaceholder = "[docker-agent: envelope delimiter removed]"

// envelopeTagRe matches anything shaped like an envelope delimiter — any leading
// mix of slashes and whitespace, an envelope-style tag name, then arbitrary
// junk up to the closing bracket.
//
// Deliberately loose about what follows the tag name. Requiring only whitespace
// or a slash there let `</document-x foo="1">` and `</document-x!>` through, and
// an HTML parser (like a model reading the transcript) ignores trailing
// attributes on an end tag, so those closed the region just as effectively as
// the exact byte sequence.
//
// The tag name is captured rather than baked in so the pattern can be compiled
// once instead of per attachment; [defuseDelimiters] decides whether a given
// match belongs to the envelope being built.
var envelopeTagRe = regexp.MustCompile(`(?i)<[\s/]*(document-[a-z0-9-]+)\b[^>]*>`)

// defuseDelimiters replaces every occurrence of this envelope's own delimiter
// inside body, in any spelling: closing or opening, upper or lower case,
// whitespace-padded, self-closing, or carrying trailing attributes.
//
// Neutralization stays scoped to this envelope's tag, so unrelated markup in an
// HTML or Markdown attachment (`</div>`, `</script>`, another document's tag) is
// preserved verbatim. A tag that merely *extends* this one
// (`</document-x-extra>` inside the `document-x` envelope) is defused too: it
// cannot be a delimiter this envelope opened, but the cost of neutralising it is
// a placeholder in someone else's markup, while the cost of missing it is a
// break-out — so the check errs toward defusing.
//
// Replacement repeats until the output is stable, because one pass can leave a
// delimiter-shaped residue behind: `</TAG</TAG>>` collapses to `[…removed]>` only
// after the second pass.
func defuseDelimiters(body, tag string) string {
	if body == "" {
		return body
	}

	lowerTag := strings.ToLower(tag)
	for range maxDefusePasses {
		defused := envelopeTagRe.ReplaceAllStringFunc(body, func(match string) string {
			groups := envelopeTagRe.FindStringSubmatch(match)
			if len(groups) < 2 || !strings.HasPrefix(strings.ToLower(groups[1]), lowerTag) {
				return match
			}
			return delimiterPlaceholder
		})
		if defused == body {
			return body
		}
		body = defused
	}
	return body
}

// maxDefusePasses bounds the replace-until-stable loop. Each pass strictly
// shortens the body (a match is always longer than nothing and is replaced by a
// constant), so this converges quickly; the bound only exists so a pathological
// input cannot spin.
const maxDefusePasses = 8

// slugify converts s to a lowercase, alphanumeric-and-hyphens-only string.
// Non-alphanumeric runes are replaced with hyphens; consecutive hyphens are
// collapsed to one; leading and trailing hyphens are trimmed.
// If the result is empty, "doc" is returned as a safe fallback.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen {
			b.WriteRune('-')
			prevHyphen = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "doc"
	}
	return result
}
