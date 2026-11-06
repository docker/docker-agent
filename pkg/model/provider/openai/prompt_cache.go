package openai

/*
Explicit prompt-cache breakpoints (gpt-5.6+).

Session assembly marks chat.Message.CacheControl at stable prompt boundaries
(invariant system prompt, instruction context, per-session extras). On models
that support it, those markers are translated into OpenAI's explicit
`prompt_cache_breakpoint: {"mode":"explicit"}` content markers so the service
pins reusable prefixes at exactly those boundaries. The request keeps the
default implicit prompt_cache_options mode (no request-level field is sent),
so the service still maintains its latest-message implicit checkpoint on top
of the explicit ones.
*/

import (
	"context"
	"slices"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// sendsExplicitCacheBreakpoints reports whether chat.Message.CacheControl
// markers should be translated into explicit prompt_cache_breakpoint content
// markers on the wire.
//
// gpt-5.6+ is the first OpenAI family to accept the field (older models
// reject it with HTTP 400), but that alone is not sufficient: OpenAI-only
// wire fields must not leak onto OpenAI-compatible aliases fronting a
// different vendor's models (xai, mistral, ...). The vendor check mirrors
// [sendsRealNoneEffort]: a genuine OpenAI vendor per
// [modelinfo.IsOpenAIVendor] (direct openai/chatgpt/azure providers, or an
// explicitly "openai/"-qualified model id behind a gateway), or the trusted
// options.WithOpenAIVendor bit resolved by pkg/model/provider's factory for
// named custom OpenAI providers. Nothing under cfg.ProviderOpts feeds this
// decision.
//
// One exclusion overrides both positive paths: the openai provider pointed
// at a custom base_url reaches a self-hosted or third-party
// OpenAI-compatible server (vLLM, SGLang, ...), not api.openai.com — the
// same assumption [shouldMergeConsecutiveMessages] makes on the Chat
// Completions path. Such servers reject the OpenAI-only field, and the
// endpoint stays custom no matter how vendor identity was resolved, so the
// openAIVendor bit cannot bypass the exclusion.
func sendsExplicitCacheBreakpoints(cfg *latest.ModelConfig, openAIVendor bool) bool {
	if !modelinfo.OpenAISupportsExplicitPromptCache(cfg.Model) {
		return false
	}
	if cfg.Provider == "openai" && !catalog.IsDefaultEndpoint(cfg.Provider, cfg.BaseURL) {
		return false
	}
	return modelinfo.IsOpenAIVendor(cfg.Provider, cfg.Model) || openAIVendor
}

// convertMessagesWithCacheBreakpoints converts messages for the Chat
// Completions API like [oaistream.ConvertMessages], additionally translating
// each source message's CacheControl marker into an explicit breakpoint on
// the last supported content block converted from that message.
//
// Decorate before tool attachments are deferred, so their markers retain the
// originating source boundary without interrupting a parallel tool-result batch.
func (c *Client) convertMessagesWithCacheBreakpoints(ctx context.Context, messages []chat.Message) []openai.ChatCompletionMessageParamUnion {
	mc := modelinfo.ResolveCaps(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), c.CapsOverride())
	return oaistream.ConvertMessagesWithTransform(ctx, messages, mc, func(i int, converted []openai.ChatCompletionMessageParamUnion) {
		if messages[i].CacheControl {
			markChatCacheBreakpoint(converted)
		}
	})
}

// markChatCacheBreakpoint places an explicit breakpoint on the last supported
// content block among the given Chat Completions messages (all converted from
// one marked source message), scanning backwards. It reports whether a block
// was marked; an empty or content-less message set is left untouched.
func markChatCacheBreakpoint(msgs []openai.ChatCompletionMessageParamUnion) bool {
	for i := range slices.Backward(msgs) {
		if markChatMessageCacheBreakpoint(&msgs[i]) {
			return true
		}
	}
	return false
}

// markChatMessageCacheBreakpoint marks the last supported content block of a
// single Chat Completions message. Simple string content is first converted
// to the role-appropriate one-element content-part array, because breakpoints
// can only be carried by content-block objects.
func markChatMessageCacheBreakpoint(msg *openai.ChatCompletionMessageParamUnion) bool {
	switch {
	case msg.OfSystem != nil:
		content := &msg.OfSystem.Content
		if content.OfString.Valid() {
			content.OfArrayOfContentParts = []openai.ChatCompletionContentPartTextParam{{Text: content.OfString.Value}}
			content.OfString = param.Opt[string]{}
		}
		return markLastChatTextPart(content.OfArrayOfContentParts)

	case msg.OfUser != nil:
		content := &msg.OfUser.Content
		if content.OfString.Valid() {
			content.OfArrayOfContentParts = []openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(content.OfString.Value)}
			content.OfString = param.Opt[string]{}
		}
		for i := range slices.Backward(content.OfArrayOfContentParts) {
			if markChatContentPart(&content.OfArrayOfContentParts[i]) {
				return true
			}
		}

	case msg.OfAssistant != nil:
		content := &msg.OfAssistant.Content
		if content.OfString.Valid() {
			content.OfArrayOfContentParts = []openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion{
				{OfText: &openai.ChatCompletionContentPartTextParam{Text: content.OfString.Value}},
			}
			content.OfString = param.Opt[string]{}
		}
		// Assistant blocks converted by this package are text-only (no
		// refusal blocks); a tool-call-only assistant message has no content
		// block that could carry a breakpoint.
		for _, block := range slices.Backward(content.OfArrayOfContentParts) {
			if block.OfText != nil {
				block.OfText.PromptCacheBreakpoint = openai.NewChatCompletionContentPartTextPromptCacheBreakpointParam()
				return true
			}
		}

	case msg.OfTool != nil:
		content := &msg.OfTool.Content
		if content.OfString.Valid() {
			content.OfArrayOfContentParts = []openai.ChatCompletionContentPartTextParam{{Text: content.OfString.Value}}
			content.OfString = param.Opt[string]{}
		}
		return markLastChatTextPart(content.OfArrayOfContentParts)
	}
	return false
}

// markLastChatTextPart marks the last text content part, if any.
func markLastChatTextPart(parts []openai.ChatCompletionContentPartTextParam) bool {
	if len(parts) == 0 {
		return false
	}
	parts[len(parts)-1].PromptCacheBreakpoint = openai.NewChatCompletionContentPartTextPromptCacheBreakpointParam()
	return true
}

// markChatContentPart marks a user-message content part. All Chat Completions
// part types (text, image_url, input_audio, file) support breakpoints.
func markChatContentPart(part *openai.ChatCompletionContentPartUnionParam) bool {
	switch {
	case part.OfText != nil:
		part.OfText.PromptCacheBreakpoint = openai.NewChatCompletionContentPartTextPromptCacheBreakpointParam()
	case part.OfImageURL != nil:
		part.OfImageURL.PromptCacheBreakpoint = openai.NewChatCompletionContentPartImagePromptCacheBreakpointParam()
	case part.OfInputAudio != nil:
		part.OfInputAudio.PromptCacheBreakpoint = openai.NewChatCompletionContentPartInputAudioPromptCacheBreakpointParam()
	case part.OfFile != nil:
		part.OfFile.PromptCacheBreakpoint = openai.NewChatCompletionContentPartFilePromptCacheBreakpointParam()
	default:
		return false
	}
	return true
}

// markResponseCacheBreakpoint places an explicit breakpoint on the last
// supported content block among the given Responses input items (all
// converted from one marked source message), scanning backwards. Top-level
// function_call items cannot carry breakpoints and are skipped; it reports
// whether a block was marked.
func markResponseCacheBreakpoint(items []responses.ResponseInputItemUnionParam) bool {
	for i := range slices.Backward(items) {
		if markResponseInputItem(&items[i]) {
			return true
		}
	}
	return false
}

// markResponseInputItem marks the last supported content block of a single
// Responses input item. Simple string content — EasyInputMessage content and
// function_call_output output — is first converted to a one-element
// input_text content list so it can carry the typed breakpoint.
func markResponseInputItem(item *responses.ResponseInputItemUnionParam) bool {
	switch {
	case item.OfMessage != nil:
		if item.OfMessage.Role == responses.EasyInputMessageRoleAssistant {
			// Assistant prompt content uses output_text, which cannot carry a
			// breakpoint (only input_text/input_image/input_file can).
			return false
		}
		content := &item.OfMessage.Content
		if content.OfString.Valid() {
			content.OfInputItemContentList = responses.ResponseInputMessageContentListParam{
				{OfInputText: &responses.ResponseInputTextParam{Text: content.OfString.Value}},
			}
			content.OfString = param.Opt[string]{}
		}
		return markLastResponseContentPart(content.OfInputItemContentList)

	case item.OfInputMessage != nil:
		return markLastResponseContentPart(item.OfInputMessage.Content)

	case item.OfFunctionCallOutput != nil:
		output := &item.OfFunctionCallOutput.Output
		if output.OfString.Valid() {
			output.OfResponseFunctionCallOutputItemArray = responses.ResponseFunctionCallOutputItemListParam{
				{OfInputText: &responses.ResponseInputTextContentParam{Text: output.OfString.Value}},
			}
			output.OfString = param.Opt[string]{}
		}
		return markLastFunctionCallOutputItem(output.OfResponseFunctionCallOutputItemArray)
	}
	return false
}

// markLastResponseContentPart marks the last supported (input_text,
// input_image or input_file) content part, if any.
func markLastResponseContentPart(parts responses.ResponseInputMessageContentListParam) bool {
	for _, part := range slices.Backward(parts) {
		switch {
		case part.OfInputText != nil:
			part.OfInputText.PromptCacheBreakpoint = responses.NewResponseInputTextPromptCacheBreakpointParam()
		case part.OfInputImage != nil:
			part.OfInputImage.PromptCacheBreakpoint = responses.NewResponseInputImagePromptCacheBreakpointParam()
		case part.OfInputFile != nil:
			part.OfInputFile.PromptCacheBreakpoint = responses.NewResponseInputFilePromptCacheBreakpointParam()
		default:
			continue
		}
		return true
	}
	return false
}

// markLastFunctionCallOutputItem marks the last supported (input_text,
// input_image or input_file) function_call_output content item, if any.
// These Content-typed blocks carry their own breakpoint params, distinct
// from the message-level input part types above.
func markLastFunctionCallOutputItem(items responses.ResponseFunctionCallOutputItemListParam) bool {
	for _, item := range slices.Backward(items) {
		switch {
		case item.OfInputText != nil:
			item.OfInputText.PromptCacheBreakpoint = responses.NewResponseInputTextContentPromptCacheBreakpointParam()
		case item.OfInputImage != nil:
			item.OfInputImage.PromptCacheBreakpoint = responses.NewResponseInputImageContentPromptCacheBreakpointParam()
		case item.OfInputFile != nil:
			item.OfInputFile.PromptCacheBreakpoint = responses.NewResponseInputFileContentPromptCacheBreakpointParam()
		default:
			continue
		}
		return true
	}
	return false
}
