package message

import (
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/tui/types"
)

// streamingMarkdownContent is a representative assistant reply: a few
// paragraphs of prose, bullets, inline code and a fenced block. Long enough
// that re-parsing it on every chunk is measurable.
const streamingMarkdownContent = `# Streaming response

This is a representative assistant reply that uses a mix of **bold**,
*italic* and ` + "`inline code`" + ` so the markdown parser actually has to do
some work. We also throw in a [link](https://example.com) and an autolink
https://docker.com to exercise the URL detection paths.

Here is a short list:

- First bullet with some prose attached.
- Second bullet with ` + "`code`" + ` inside.
- Third bullet that mentions another https://example.org URL.

And a fenced code block:

` + "```go" + `
package main

import "fmt"

func main() {
    fmt.Println("hello, world")
}
` + "```" + `

Another paragraph after the code block to make sure the parser does not
short-circuit. We want this benchmark to reflect real assistant traffic.
`

// BenchmarkRenderRepeated measures the cost of calling Render twice in a row
// on the same content (View() + Height() pattern). With the cache this should
// be roughly half the cost of two uncached renders.
func BenchmarkRenderRepeated(b *testing.B) {
	msg := types.Agent(types.MessageTypeAssistant, "agent", streamingMarkdownContent)
	mv := New(msg, nil)
	mv.SetSize(100, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = mv.Render(100)
		_ = mv.Render(100)
	}
}

// BenchmarkRenderRepeatedUncached is the baseline that bypasses the cache by
// calling the unmemoized core. Comparing it with BenchmarkRenderRepeated shows
// how much work the cache eliminates when View()/Height() pairs render the
// same content.
func BenchmarkRenderRepeatedUncached(b *testing.B) {
	msg := types.Agent(types.MessageTypeAssistant, "agent", streamingMarkdownContent)
	mv := New(msg, nil)
	mv.SetSize(100, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = mv.render(100)
		_ = mv.render(100)
	}
}

// BenchmarkStreamingChunks simulates a streaming reply where each new token
// triggers a Render of the entire accumulated content. With chunkCount chunks
// the work is O(N^2) in chunkCount because the markdown parse processes the
// full prefix every time. The cache here only helps when the same content is
// rendered twice (e.g. View() then Height()), which is the common case.
func BenchmarkStreamingChunks(b *testing.B) {
	msg := types.Agent(types.MessageTypeAssistant, "agent", "")
	mv := New(msg, nil)
	mv.SetSize(100, 0)

	const chunkCount = 64
	chunks := splitIntoChunks(streamingMarkdownContent, chunkCount)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var accumulated strings.Builder
		msg.Content = ""
		mv.SetMessage(msg)
		for _, c := range chunks {
			accumulated.WriteString(c)
			msg.Content = accumulated.String()
			mv.SetMessage(msg)
			// Each chunk typically triggers View() and (transitively) Height().
			_ = mv.Render(100)
			_ = mv.Render(100)
		}
	}
}

func splitIntoChunks(s string, n int) []string {
	if n <= 0 {
		return []string{s}
	}
	chunkSize := max(1, len(s)/n)
	chunks := make([]string, 0, n)
	for i := 0; i < len(s); i += chunkSize {
		end := min(i+chunkSize, len(s))
		chunks = append(chunks, s[i:end])
	}
	return chunks
}
