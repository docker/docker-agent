package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCacheControlSupportCoverage(t *testing.T) {
	t.Parallel()

	cacheable := map[string]ContentBlockParam{
		"text":        newTextBlock("x"),
		"tool_use":    {Type: "tool_use", ID: "1", Name: "t"},
		"tool_result": newToolResultBlock("1", "ok", false),
		"image":       {Type: "image", Source: &MediaSourceParam{Type: "base64", Data: "d"}},
		"document":    {Type: "document"},
	}
	for name, block := range cacheable {
		assert.True(t, block.SupportsCacheControl(), "%s blocks must accept cache control", name)
	}

	uncacheable := map[string]ContentBlockParam{
		"thinking":          newThinkingBlock("sig", "why"),
		"redacted_thinking": {Type: "redacted_thinking", Data: "sig"},
	}
	for name, block := range uncacheable {
		assert.False(t, block.SupportsCacheControl(), "%s blocks must not accept cache control", name)
	}
}
