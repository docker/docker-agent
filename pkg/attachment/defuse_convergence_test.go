package attachment

import (
	"strings"
	"testing"
)

// TestDelimiterPlaceholderCannotFormAMatch pins the property that actually makes
// defuseDelimiters terminate.
//
// The loop does not converge by shrinking the body -- the placeholder is longer
// than the delimiters it replaces, so the body usually grows. It converges
// because the placeholder contains neither "<" nor ">", so it can never become
// part of a new match, and every pass therefore strictly reduces the number of
// delimiter-shaped tokens remaining.
//
// If someone gives delimiterPlaceholder angle brackets, that argument collapses
// and the loop could hit maxDefusePasses with a live delimiter still in the body
// -- which defuseDelimiters would then return as-is. This test is the tripwire.
func TestDelimiterPlaceholderCannotFormAMatch(t *testing.T) {
	if strings.ContainsAny(delimiterPlaceholder, "<>") {
		t.Fatalf("delimiterPlaceholder must contain no angle brackets, or the "+
			"convergence argument for maxDefusePasses no longer holds; got %q",
			delimiterPlaceholder)
	}
	if envelopeTagRe.MatchString(delimiterPlaceholder) {
		t.Fatalf("delimiterPlaceholder must not itself match envelopeTagRe; got %q",
			delimiterPlaceholder)
	}
}

// TestDefuseDelimitersConverges checks that adversarial bodies reach a fixed
// point well inside maxDefusePasses, and that nothing delimiter-shaped for this
// envelope's tag survives.
//
// Convergence is asserted separately from survivor-freedom on purpose: hitting
// the pass bound is silent (defuseDelimiters returns the body it has), so a
// regression there would otherwise only show up as a break-out much later.
func TestDefuseDelimitersConverges(t *testing.T) {
	const tag = "document-x"

	cases := map[string]string{
		"plain close":       `</document-x>`,
		"plain open":        `<document-x>`,
		"self closing":      `<document-x/>`,
		"trailing attrs":    `</document-x foo="1">`,
		"trailing bang":     `</document-x!>`,
		"whitespace padded": `</   document-x   >`,
		"upper case":        `</DOCUMENT-X>`,
		"mixed case":        `</DoCuMeNt-X>`,
		"prefix extending":  `</document-x-extra>`,
		"residue shape":     `</document-x</document-x>>`,
		"residue deeper":    `</document-x</document-x</document-x>>>`,
		"deeply nested":     strings.Repeat(`</document-x`, 20) + strings.Repeat(`>`, 20),
		"split brackets":    strings.Repeat(`<document-x`, 12) + strings.Repeat(`>`, 12),
		"many singles":      strings.Repeat(`</document-x> `, 50),
		"placeholder first": delimiterPlaceholder + `</document-x>`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			passes := passesToFixedPoint(body, tag)
			if passes < 0 {
				t.Fatalf("did not converge within maxDefusePasses (%d)", maxDefusePasses)
			}
			if passes > 1 {
				// Not a failure -- the loop exists precisely to allow this --
				// but the corrected comment claims one pass suffices for every
				// covered case, so surface any case that stops being true.
				t.Logf("converged in %d passes (comment claims 1 for covered cases)", passes)
			}

			out := defuseDelimiters(body, tag)
			for _, m := range envelopeTagRe.FindAllStringSubmatch(out, -1) {
				if len(m) >= 2 && strings.HasPrefix(strings.ToLower(m[1]), tag) {
					t.Fatalf("live delimiter survived defusing: %q in %q", m[0], out)
				}
			}
		})
	}
}

// passesToFixedPoint mirrors the defuseDelimiters loop and reports how many
// passes it takes to stabilise, or -1 if it never does within the bound.
func passesToFixedPoint(body, tag string) int {
	lowerTag := strings.ToLower(tag)
	for i := range maxDefusePasses {
		next := envelopeTagRe.ReplaceAllStringFunc(body, func(match string) string {
			groups := envelopeTagRe.FindStringSubmatch(match)
			if len(groups) < 2 || !strings.HasPrefix(strings.ToLower(groups[1]), lowerTag) {
				return match
			}
			return delimiterPlaceholder
		})
		if next == body {
			return i
		}
		body = next
	}
	// One more comparison: the bound may have been exactly enough.
	next := envelopeTagRe.ReplaceAllStringFunc(body, func(match string) string {
		groups := envelopeTagRe.FindStringSubmatch(match)
		if len(groups) < 2 || !strings.HasPrefix(strings.ToLower(groups[1]), lowerTag) {
			return match
		}
		return delimiterPlaceholder
	})
	if next == body {
		return maxDefusePasses
	}
	return -1
}
