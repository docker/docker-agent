package anthropic

import (
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestPromptCacheControlHonorsBreakpointLimit(t *testing.T) {
	t.Parallel()

	source := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "invariant", PromptSection: chat.PromptSectionInvariant},
		{Role: chat.MessageRoleSystem, Content: "context", PromptSection: chat.PromptSectionContext},
	}
	requestTools := []tools.Tool{
		{Name: "immediate", Parameters: map[string]any{"type": "object"}},
		{Name: "deferred", Parameters: map[string]any{"type": "object"}, Deferred: true},
	}
	convertedTools, err := convertTools(requestTools)
	require.NoError(t, err)
	params := anthropic.MessageNewParams{
		System: []anthropic.TextBlockParam{{Text: "invariant"}, {Text: "context"}},
		Tools:  convertedTools,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("older")),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock("latest")),
		},
	}

	applyPromptCacheControl(source, requestTools, &params)

	assert.Equal(t, maxCacheControlBreakpoints, countCacheControlBreakpoints(params))
	assert.Empty(t, string(params.Messages[0].Content[0].OfText.CacheControl.Type))
	assert.Equal(t, "ephemeral", string(params.Messages[1].Content[0].OfText.CacheControl.Type))

	convertedBetaTools, err := convertBetaTools(requestTools)
	require.NoError(t, err)
	betaParams := anthropic.BetaMessageNewParams{
		System: []anthropic.BetaTextBlockParam{{Text: "invariant"}, {Text: "context"}},
		Tools:  convertedBetaTools,
		Messages: []anthropic.BetaMessageParam{
			betaTextMessage(anthropic.BetaMessageParamRoleUser, "older"),
			betaTextMessage(anthropic.BetaMessageParamRoleAssistant, "latest"),
		},
	}

	applyBetaPromptCacheControl(source, requestTools, &betaParams)

	assert.Equal(t, maxCacheControlBreakpoints, countBetaCacheControlBreakpoints(betaParams))
	assert.Empty(t, string(betaParams.Messages[0].Content[0].OfText.CacheControl.Type))
	assert.Equal(t, "ephemeral", string(betaParams.Messages[1].Content[0].OfText.CacheControl.Type))
}

func TestPromptCacheControlMarksTwoRecentMessagesWhenBudgetAllows(t *testing.T) {
	t.Parallel()

	source := []chat.Message{{
		Role:          chat.MessageRoleSystem,
		Content:       "invariant",
		PromptSection: chat.PromptSectionInvariant,
	}}
	params := anthropic.MessageNewParams{
		System: []anthropic.TextBlockParam{{Text: "invariant"}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("oldest")),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock("older")),
			anthropic.NewUserMessage(anthropic.NewTextBlock("latest")),
		},
	}

	applyPromptCacheControl(source, nil, &params)

	assert.Equal(t, 3, countCacheControlBreakpoints(params))
	assert.Empty(t, string(params.Messages[0].Content[0].OfText.CacheControl.Type))
	assert.Equal(t, "ephemeral", string(params.Messages[1].Content[0].OfText.CacheControl.Type))
	assert.Equal(t, "ephemeral", string(params.Messages[2].Content[0].OfText.CacheControl.Type))
}

func betaTextMessage(role anthropic.BetaMessageParamRole, text string) anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{
		Role: role,
		Content: []anthropic.BetaContentBlockParamUnion{{
			OfText: &anthropic.BetaTextBlockParam{Text: text},
		}},
	}
}

func countCacheControlBreakpoints(params anthropic.MessageNewParams) int {
	count := 0
	for _, block := range params.System {
		if block.CacheControl.Type != "" {
			count++
		}
	}
	for _, tool := range params.Tools {
		if tool.OfTool != nil && tool.OfTool.CacheControl.Type != "" {
			count++
		}
	}
	for _, message := range params.Messages {
		for _, block := range message.Content {
			if block.OfText != nil && block.OfText.CacheControl.Type != "" {
				count++
			}
		}
	}
	return count
}

func countBetaCacheControlBreakpoints(params anthropic.BetaMessageNewParams) int {
	count := 0
	for _, block := range params.System {
		if block.CacheControl.Type != "" {
			count++
		}
	}
	for _, tool := range params.Tools {
		if tool.OfTool != nil && tool.OfTool.CacheControl.Type != "" {
			count++
		}
	}
	for _, message := range params.Messages {
		for _, block := range message.Content {
			if block.OfText != nil && block.OfText.CacheControl.Type != "" {
				count++
			}
		}
	}
	return count
}
