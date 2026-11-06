package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/tools"
)

const nativeCompactionBeta anthropic.AnthropicBeta = "compact-2026-09-04"

// nativeCompactionEnabled reports whether this client may send compaction
// blocks. Vertex rejects the option, so it never replays a block recorded on
// the Claude API and sends the summary text instead.
func nativeCompactionEnabled(opts map[string]any) bool {
	enabled, _ := providerutil.GetProviderOptBool(opts, "native_compaction")
	return enabled
}

func (c *Client) hasNativeCompaction(messages []chat.Message) bool {
	return nativeCompactionEnabled(c.ModelConfig.ProviderOpts) &&
		slices.ContainsFunc(messages, func(m chat.Message) bool { return m.ReplayableCompaction(providerStateName) != nil })
}

func (c *Client) compactionMessage(msg *chat.Message) (anthropic.BetaMessageParam, bool, error) {
	if !nativeCompactionEnabled(c.ModelConfig.ProviderOpts) {
		return anthropic.BetaMessageParam{}, false, nil
	}
	compact := msg.ReplayableCompaction(providerStateName)
	if compact == nil {
		return anthropic.BetaMessageParam{}, false, nil
	}
	var block anthropic.BetaCompactionBlock
	if err := json.Unmarshal(compact.Block, &block); err != nil {
		return anthropic.BetaMessageParam{}, false, fmt.Errorf("anthropic: invalid compaction block: %w", err)
	}
	if block.Type != "compaction" || strings.TrimSpace(block.Content) == "" || block.Signature == "" {
		return anthropic.BetaMessageParam{}, false, errors.New("anthropic: compaction block requires content and signature")
	}
	// The beta accepts either role; user also permits a summary-only prompt.
	return anthropic.BetaMessageParam{
		Role:    anthropic.BetaMessageParamRoleUser,
		Content: []anthropic.BetaContentBlockParamUnion{param.Override[anthropic.BetaContentBlockParamUnion](compact.Block)},
	}, true, nil
}

// CompactConversation summarizes the entire supplied conversation. The runtime
// persists the signed block before replacing history; failures never discard it.
func (c *Client) CompactConversation(ctx context.Context, messages []chat.Message, requestTools []tools.Tool, instructions string) (*chat.CompactionResult, error) {
	if c.ModelConfig.Provider != "anthropic" {
		return nil, errors.New("anthropic: native compaction requires the Claude API")
	}
	client, err := c.clientFn(ctx)
	if err != nil {
		return nil, err
	}
	requestTools = c.toolsWithSupportedDeferral(ctx, requestTools, messages)
	allTools, err := convertBetaTools(requestTools)
	if err != nil {
		return nil, err
	}
	if selection, ok := strictToolsOpt(c.ModelConfig.ProviderOpts); ok {
		restoreBetaStrictToolSchemas(allTools, requestTools)
		if err := selection.applyBeta(allTools); err != nil {
			return nil, err
		}
	}
	converted, err := c.convertBetaMessagesWithDeferred(ctx, messages, requestTools)
	if err != nil {
		return nil, err
	}
	if len(converted) == 0 {
		return nil, nil
	}
	params := anthropic.BetaMessageNewParams{
		Model: c.ModelConfig.Model, MaxTokens: 16000,
		Messages: converted, System: extractBetaSystemBlocks(messages), Tools: allTools,
		Betas:      []anthropic.AnthropicBeta{nativeCompactionBeta},
		Compaction: anthropic.BetaCompactionConfigUnionParam{OfSummarize: &anthropic.BetaSummarizeCompactionParam{}},
	}
	if strings.TrimSpace(instructions) != "" {
		params.Compaction.OfSummarize.Instructions = anthropic.String(instructions)
	}
	adjusted, err := c.adjustMaxTokensForThinking(params.MaxTokens)
	if err != nil {
		return nil, err
	}
	params.MaxTokens = adjusted
	c.applyBetaThinkingConfig(&params, params.MaxTokens)
	c.applyThinkingBinding(&params)
	var requestContext json.RawMessage
	if cachePreservingUpdatesEnabled(c.ModelConfig.ProviderOpts) {
		updates, err := applyConversationUpdates(ctx, c.ModelConfig.Model, &params, messages, nil)
		if err != nil {
			return nil, err
		}
		requestContext = updates.CompactionContext
	}
	stream := client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var response anthropic.BetaMessage
	complete := false
	for stream.Next() {
		event := stream.Current()
		if err := response.Accumulate(event); err != nil {
			return nil, fmt.Errorf("anthropic: accumulate compaction: %w", err)
		}
		complete = event.Type == "message_stop"
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if !complete {
		return nil, errors.New("anthropic: incomplete compaction response")
	}
	if response.StopReason != "compaction" {
		return nil, fmt.Errorf("anthropic: unexpected compaction stop reason %q", response.StopReason)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "compaction" {
		return nil, errors.New("anthropic: expected one compaction block")
	}
	block := response.Content[0].AsCompaction()
	if strings.TrimSpace(block.Content) == "" {
		return nil, nil
	}
	if block.Signature == "" {
		return nil, errors.New("anthropic: unsigned compaction block")
	}
	var usage chat.Usage
	var iterations []chat.Usage
	if c.TrackUsageEnabled() {
		// On-demand compaction bills iterations, not the zero top-level totals.
		for _, iteration := range response.Usage.Iterations {
			u := chat.Usage{
				InputTokens: iteration.InputTokens, OutputTokens: iteration.OutputTokens,
				CacheWriteTokens: iteration.CacheCreationInputTokens, CachedInputTokens: iteration.CacheReadInputTokens,
			}
			usage.Add(&u)
			iterations = append(iterations, u)
		}
	}
	return &chat.CompactionResult{
		Summary: block.Content, Block: json.RawMessage(block.RawJSON()), Provider: providerStateName,
		Model: response.Model, Usage: usage, Iterations: iterations, RequestContext: requestContext,
	}, nil
}
