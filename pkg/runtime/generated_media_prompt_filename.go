package runtime

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker-agent/pkg/chat"
)

// Deterministic user-prompt filename fallback: when a turn returns exactly
// one media blob and no [media-file:] marker named it (a model may ignore
// the marker instruction entirely), the triggering user message — never
// model text or history — is scanned for a single unambiguous explicit
// output filename ("Generate an image as sunshine.jpg", "Generate an image
// of a red panda as assets/red-panda.jpg", "save it as `pics/cat.png`",
// "filename: x.webp", ...). The naming precedence is
// therefore: marker →
// user-prompt explicit filename → provider display name → generic
// generated-N. An extracted path is untrusted exactly like a marker path:
// it only fills [chat.MediaDelta.RequestedPath] and flows through the same
// workspacemedia classification, MIME/extension correction, collision
// suffixing, and escape-confirmation pipeline. This is a strict grammar,
// deliberately not generic NLP: zero or multiple candidates mean no
// extraction.

// maxExplicitOutputFilenameBytes bounds an extracted filename; anything
// longer is not treated as a candidate.
const maxExplicitOutputFilenameBytes = 256

// explicitOutputFilenameRE matches an explicit output-naming cue followed by
// a quoted, backticked, or unquoted filename. Bare "to" is deliberately not
// a cue so input-file mentions ("add a border to photo.jpg") never match,
// and bare "called" is not a cue because it usually references an existing
// input file ("similar to the one called old-render.png"). Bare "as" only
// counts inside a narrow imperative output context — a generation verb
// directly followed by an optionally-articled media noun ("Generate an
// image as sunshine.jpg"; explicitOutputFilenameOfPhraseRE below adds the
// "of <subject>" variant of the same context) — because RE2 has no
// lookbehind to exclude the comparative form ("in the same style as
// sunshine.jpg", "the same background as assets/bg.png") any other way.
// The unquoted form
// additionally anchors on a known image extension so trailing punctuation
// is excluded. Quoted/backticked contents may include spaces and are
// validated separately by isExplicitImageFilename.
var explicitOutputFilenameRE = regexp.MustCompile(
	`(?i)\b(?:(?:save\s+it\s+as|save\s+as|save\s+to|write\s+to|output\s+to|name\s+it|call\s+it)\s+|filename\s*[:=]\s*|` +
		`(?:re)?(?:generate|create|make|draw|render|produce)\s+(?:an?\s+|the\s+)?` +
		`(?:image|picture|photo|banner|logo|icon|graphic|drawing|illustration|thumbnail|sticker|avatar|gif)\s+as\s+)` +
		`(?:"([^"]+)"|'([^']+)'|` + "`([^`]+)`" + `|([^\s"'` + "`" + `]+\.(?:png|jpe?g|webp|gif))\b)`,
)

// explicitOutputFilenameOfPhraseRE extends the imperative form above to a
// media noun carrying an "of <subject>" description before the bare "as"
// cue ("Generate an image of a red panda coding at a terminal as
// assets/red-panda-terminal.jpg"). The subject cannot cross quotes,
// clause punctuation, or CR/LF — so "Generate an image of a cat. Save it
// as x.png" and the newline-separated equivalent stay a single
// save-it-as candidate — and it is captured so extraction can reject
// comparative phrasings inside it ("of a cat in the same style as
// old.png", "of a beach with the exact palette as ref.png"; see
// comparativeCueRE): RE2 has no lookbehind to express that exclusion in
// the pattern itself. The subject MAY swallow a conjunction ahead of an
// explicit cue ("of a wolf and save it as logo.png"); extraction
// deduplicates that overlap with explicitOutputFilenameRE by capture
// position.
var explicitOutputFilenameOfPhraseRE = regexp.MustCompile(
	`(?i)\b(?:re)?(?:generate|create|make|draw|render|produce)\s+(?:an?\s+|the\s+)?` +
		`(?:image|picture|photo|banner|logo|icon|graphic|drawing|illustration|thumbnail|sticker|avatar|gif)\s+` +
		`(of\s+[^"'` + "`" + `.,;:!?\r\n]+?)\s+as\s+` +
		`(?:"([^"]+)"|'([^']+)'|` + "`([^`]+)`" + `|([^\s"'` + "`" + `]+\.(?:png|jpe?g|webp|gif))\b)`,
)

// comparativeCueRE flags an of-phrase subject whose trailing "as" compares
// against an existing file instead of naming the output ("of a cat in the
// same style as old.png", "of something like ref.png"). Beyond explicit
// comparative words, it conservatively rejects any subject containing
// "as", "with", or "in": those introduce attribute phrases whose trailing
// "as" compares ("with the exact palette as ref.png", "with identical
// colors as ref.png", "as tall as tree.png") and comparative adjectives
// are open-ended. The asymmetry justifies over-rejecting: a false reject
// only costs the generic generated-N name, while a false capture can
// engage the escape-confirmation policy for a merely referenced file.
var comparativeCueRE = regexp.MustCompile(
	`(?i)\b(?:same|style|similar|like|such|as|with|in|exact|identical|matching|equivalent)\b`,
)

// extractExplicitOutputFilename returns the single unambiguous explicit
// output filename in the triggering user prompt, if there is exactly one.
// Candidates that fail validation (unknown extension, control characters,
// invalid UTF-8, over-long, extension-only, comparative of-phrase) are
// ignored rather than counted. The two grammars overlap: an of-phrase
// subject may swallow a conjunction ahead of an explicit cue ("make a
// logo of a wolf and save it as logo.png"), so candidates are keyed by
// the filename capture's position — the same occurrence matched by both
// grammars counts once, while distinct occurrences (even of the same
// name) stay ambiguous and yield no extraction.
func extractExplicitOutputFilename(prompt string) (string, bool) {
	candidates := make(map[int]string)
	for _, m := range explicitOutputFilenameRE.FindAllStringSubmatchIndex(prompt, -1) {
		if offset, name := firstMatchedGroup(prompt, m, 1); isExplicitImageFilename(name) {
			candidates[offset] = name
		}
	}
	for _, m := range explicitOutputFilenameOfPhraseRE.FindAllStringSubmatchIndex(prompt, -1) {
		if comparativeCueRE.MatchString(prompt[m[2]:m[3]]) {
			continue
		}
		if offset, name := firstMatchedGroup(prompt, m, 2); isExplicitImageFilename(name) {
			candidates[offset] = name
		}
	}
	if len(candidates) != 1 {
		return "", false
	}
	for _, name := range candidates {
		return name, true
	}
	return "", false
}

// firstMatchedGroup returns the start offset and text of the one capture
// group the quoting alternation filled, scanning submatch index pairs from
// firstGroup on; every group requires at least one character, so a filled
// group has a non-negative start.
func firstMatchedGroup(prompt string, m []int, firstGroup int) (int, string) {
	for g := firstGroup; 2*g+1 < len(m); g++ {
		if start, end := m[2*g], m[2*g+1]; start >= 0 {
			return start, prompt[start:end]
		}
	}
	return -1, ""
}

// isExplicitImageFilename applies the same field constraints as the marker
// grammar (valid UTF-8, no control characters, no edge whitespace, bounded)
// plus a known image extension and a non-empty stem. Traversing or absolute
// paths are valid candidates on purpose: the untrusted-path pipeline owns
// containment and escape confirmation.
func isExplicitImageFilename(name string) bool {
	if name == "" || len(name) > maxExplicitOutputFilenameBytes || !utf8.ValidString(name) {
		return false
	}
	if strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
	default:
		return false
	}
	return len(path.Base(name)) > len(path.Ext(name))
}

// applyUserPromptRequestedPath fills RequestedPath from the triggering user
// prompt for a turn that returned exactly ONE media blob that marker pairing
// left unnamed. Markers keep precedence (a non-empty RequestedPath is never
// overwritten) and multi-blob turns are skipped entirely — a single prompt
// filename cannot unambiguously name one blob among several.
func applyUserPromptRequestedPath(media []chat.MediaDelta, prompt string) {
	if len(media) != 1 || media[0].RequestedPath != "" {
		return
	}
	if name, ok := extractExplicitOutputFilename(prompt); ok {
		media[0].RequestedPath = name
	}
}
