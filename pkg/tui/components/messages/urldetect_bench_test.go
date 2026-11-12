package messages

import (
	"strings"
	"testing"
)

// urlScanBatch is a representative batch used for allocation benchmarks.
// Labels refer to the whole batch, not individual inputs.
var urlScanBatch = []string{
	"hello world, nothing to see here at all in this plain ascii line of text",
	"see https://example.com/path?q=1 and http://foo.bar/baz. Also (https://x.y/z).",
	strings.Repeat("word ", 40) + "https://example.com/very/long/path/with/segments" + strings.Repeat(" tail", 40),
	"日本語テキスト https://例え.jp/パス と 絵文字 🎉 http://emoji.test/🚀 done",
	"nourlhere " + strings.Repeat("x", 300),
	"\x1b]8;;https://osc8.example.com\x1b\\clickable\x1b]8;;\x1b\\ then visible https://visible.example.com/",
	"broken \xff\xfe utf8 https://still.works/ok \xc3 tail",
}

func BenchmarkFindURLSpansBatch(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for _, line := range urlScanBatch {
			_ = findURLSpans(line)
		}
	}
}

func BenchmarkFindAllURLSpansBatch(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for _, line := range urlScanBatch {
			_ = findAllURLSpans(line)
		}
	}
}
