package messages

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// findURLSpansOracle is a verbatim copy of findURLSpans before the hasRunePrefix
// change, kept as a regression oracle.
func findURLSpansOracle(text string) []urlSpan {
	var spans []urlSpan
	runes := []rune(text)
	n := len(runes)
	for i := 0; i < n; {
		remaining := string(runes[i:])
		var prefixLen int
		switch {
		case strings.HasPrefix(remaining, "https://"):
			prefixLen = len("https://")
		case strings.HasPrefix(remaining, "http://"):
			prefixLen = len("http://")
		default:
			i++
			continue
		}
		if i > 0 && isURLWordChar(runes[i-1]) {
			i++
			continue
		}
		urlStart := i
		j := i + prefixLen
		for j < n && isURLChar(runes[j]) {
			j++
		}
		for j > urlStart+prefixLen && isTrailingPunct(runes[j-1]) {
			j--
		}
		url := string(runes[urlStart:j])
		url = balanceParens(url)
		j = urlStart + len([]rune(url))
		startCol := runeSliceWidth(runes[:urlStart])
		endCol := startCol + runeSliceWidth(runes[urlStart:j])
		spans = append(spans, urlSpan{url: url, startCol: startCol, endCol: endCol})
		i = j
	}
	return spans
}

// corpus exercises Unicode, invalid UTF-8, boundary URLs, http/https, bare
// prefixes, punctuation, OSC 8 sequences, and ANSI colour codes.
var runePrefixCorpus = []string{
	"",
	"h",
	"http",
	"http:/",
	"http://",
	"https://",
	"https:/x",
	// word-char boundary: must not match
	"xhttps://not-a-url.com",
	"0http://digit-prefixed.com",
	// non-word-char boundary: must match
	"_http://underscore-ok.com",
	"see https://example.com/path?q=1 and http://foo.bar/baz.",
	"(https://paren.example.com)",
	"https://wiki.example.com/Foo_(bar)",
	"https://wiki.example.com/Foo_(bar))",
	"https://a.b,https://c.d",
	// CJK and emoji before/after URL
	"日本語 https://例え.jp/パス と絵文字🎉 http://emoji.test/🚀 done",
	"wide前https://after-cjk.com",
	// invalid UTF-8 bytes before and inside URL
	"broken \xff\xfe utf8 https://still.works/ok \xc3 tail",
	"\xffhttps://after-invalid.com",
	"https://\xff\xfe/invalid-in-url",
	// encoded surrogate (Go decodes to replacement rune)
	"\xed\xa0\x80https://after-surrogate.com",
	// OSC 8 sequences — findURLSpans sees the raw string, not stripped
	"\x1b]8;;https://osc8.example.com\x1b\\clickable\x1b]8;;\x1b\\ then https://visible.example.com/",
	// ANSI colour codes
	"\x1b[31mhttps://colored.example.com\x1b[0m trailing",
	// trailing punctuation stripping
	"https://x.y/z!?;:",
	// repeated URLs
	strings.Repeat("https://a.b ", 50),
}

// TestHasRunePrefixEquivalence verifies that hasRunePrefix matches
// strings.HasPrefix(string(runes[i:]), prefix) for all prefixes and inputs.
func TestHasRunePrefixEquivalence(t *testing.T) {
	t.Parallel()
	prefixes := []string{"https://", "http://", "h", "", "https:/x", "http://x"}
	for _, text := range runePrefixCorpus {
		runes := []rune(text)
		for _, pfx := range prefixes {
			for i := range runes {
				want := strings.HasPrefix(string(runes[i:]), pfx)
				got := hasRunePrefix(runes[i:], pfx)
				if want != got {
					t.Errorf("hasRunePrefix(%q[%d:], %q) = %v, want %v", text, i, pfx, got, want)
				}
			}
		}
	}
}

// TestFindURLSpansRunePrefixEquivalence compares findURLSpans against the oracle
// (old string-conversion implementation) using reflect.DeepEqual so nil vs empty
// slice mismatches are also caught.
func TestFindURLSpansRunePrefixEquivalence(t *testing.T) {
	t.Parallel()
	for _, text := range runePrefixCorpus {
		assertRunePrefixSameSpans(t, text)
		// Also exercise the ansi-stripped path (real call via findAllURLSpans).
		assertRunePrefixSameSpans(t, ansi.Strip(text))
	}
}

// TestFindURLSpansRunePrefixEquivalenceRandom stress-tests with 20 000 random strings.
func TestFindURLSpansRunePrefixEquivalenceRandom(t *testing.T) {
	t.Parallel()
	alphabet := []string{
		"h", "t", "p", "s", ":", "/", ".", "(", ")", ",", " ", "a", "Z", "9", "_",
		"日", "🎉", "\xff", "\xc3", "\xed\xa0\x80", "\x1b", "]", "8", ";", "\\",
		"http://", "https://", "https:/",
	}
	r := rand.New(rand.NewPCG(7, 11))
	for range 20000 {
		var b strings.Builder
		for range r.IntN(24) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		assertRunePrefixSameSpans(t, b.String())
	}
}

func assertRunePrefixSameSpans(t *testing.T, text string) {
	t.Helper()
	want := findURLSpansOracle(text)
	got := findURLSpans(text)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("mismatch for %q:\n oracle: %s\n   got:  %s",
			text, fmt.Sprintf("%+v", want), fmt.Sprintf("%+v", got))
	}
}
