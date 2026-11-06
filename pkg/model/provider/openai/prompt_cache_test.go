package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// explicitBreakpoint is the exact wire shape of an explicit prompt-cache
// breakpoint marker.
var explicitBreakpoint = map[string]any{"mode": "explicit"}

// promptCacheMessages is the canonical marked conversation used across the
// wire tests: a CacheControl-marked system prompt (the stable boundary
// session assembly pins) followed by an unmarked user message.
func promptCacheMessages() []chat.Message {
	return []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are helpful.", CacheControl: true},
		{Role: chat.MessageRoleUser, Content: "hi"},
	}
}

// promptCacheChatRequest drives a Chat Completions request against a mock
// server and returns the decoded request body.
func promptCacheChatRequest(t *testing.T, cfg *latest.ModelConfig, messages []chat.Message, opts ...options.Opt) map[string]any {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg.BaseURL = server.URL
	cfg.TokenKey = "MY_TOKEN"
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

// promptCacheResponsesRequest is promptCacheChatRequest's Responses-API
// sibling.
func promptCacheResponsesRequest(t *testing.T, cfg *latest.ModelConfig, messages []chat.Message, opts ...options.Opt) map[string]any {
	t.Helper()

	server, body := captureResponsesRequestBody(t)
	cfg.BaseURL = server.URL
	cfg.TokenKey = "MY_TOKEN"
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), messages, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

// assertNoPromptCacheFields asserts the request body carries neither an
// explicit breakpoint marker nor a request-wide prompt_cache_options field.
func assertNoPromptCacheFields(t *testing.T, req map[string]any) {
	t.Helper()
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "prompt_cache_breakpoint")
	_, ok := req["prompt_cache_options"]
	assert.False(t, ok, "prompt_cache_options must not be sent (implicit mode is the default)")
}

// TestChatCompletions_PromptCacheBreakpoint_GPT56 verifies that on a genuine
// OpenAI vendor endpoint (azure hosts OpenAI's own models and defaults to the
// Chat Completions API) a CacheControl-marked system message is sent as a
// one-element text content array whose block carries the explicit breakpoint,
// while unmarked messages keep their plain string content and no request-wide
// prompt_cache_options field is added.
func TestChatCompletions_PromptCacheBreakpoint_GPT56(t *testing.T) {
	t.Parallel()

	req := promptCacheChatRequest(t, &latest.ModelConfig{
		Provider: "azure",
		Model:    "gpt-5.6",
	}, promptCacheMessages())

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 2)

	assert.Equal(t, map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{
				"type":                    "text",
				"text":                    "You are helpful.",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, msgs[0])
	assert.Equal(t, map[string]any{"role": "user", "content": "hi"}, msgs[1],
		"unmarked messages must not get breakpoints or change shape")

	_, ok = req["prompt_cache_options"]
	assert.False(t, ok, "prompt_cache_options must not be sent (implicit mode is the default)")
}

// TestChatCompletions_PromptCacheBreakpoint_MultipartMessage verifies that a
// marked multipart message gets the breakpoint on its last supported content
// block only (here the trailing image), leaving earlier blocks untouched.
func TestChatCompletions_PromptCacheBreakpoint_MultipartMessage(t *testing.T) {
	t.Parallel()

	req := promptCacheChatRequest(t, &latest.ModelConfig{
		Provider: "azure",
		Model:    "gpt-5.6",
	}, []chat.Message{
		{
			Role: chat.MessageRoleUser,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "look at this"},
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/x.png", Detail: chat.ImageURLDetailHigh}},
			},
			CacheControl: true,
		},
	})

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 1)

	assert.Equal(t, map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "look at this"},
			map[string]any{
				"type":                    "image_url",
				"image_url":               map[string]any{"url": "https://example.com/x.png", "detail": "high"},
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, msgs[0])
}

// TestChatCompletions_NoPromptCacheBreakpoint_OlderModel verifies that models
// before gpt-5.6 (which reject the field with HTTP 400) get neither
// breakpoints nor prompt_cache_options, and marked message content keeps its
// plain string shape.
func TestChatCompletions_NoPromptCacheBreakpoint_OlderModel(t *testing.T) {
	t.Parallel()

	req := promptCacheChatRequest(t, &latest.ModelConfig{
		Provider: "azure",
		Model:    "gpt-5.5",
	}, promptCacheMessages())

	assertNoPromptCacheFields(t, req)
	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"role": "system", "content": "You are helpful."}, msgs[0])
}

// TestChatCompletions_NoPromptCacheBreakpoint_UnrelatedAlias verifies the
// vendor gate: an OpenAI-compatible alias fronting a different vendor's
// models must not receive the OpenAI-only field even when its configured
// model name matches gpt-5.6's naming pattern exactly.
func TestChatCompletions_NoPromptCacheBreakpoint_UnrelatedAlias(t *testing.T) {
	t.Parallel()

	req := promptCacheChatRequest(t, &latest.ModelConfig{
		Provider: "xai",
		Model:    "gpt-5.6",
	}, promptCacheMessages())

	assertNoPromptCacheFields(t, req)
	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"role": "system", "content": "You are helpful."}, msgs[0])
}

// TestChatCompletions_NoPromptCacheBreakpoint_SelfHostedMergePath verifies
// that the consecutive-message merge path (third-party/self-hosted
// OpenAI-compatible backends, see shouldMergeConsecutiveMessages) never
// carries explicit breakpoints: such backends don't accept the OpenAI-only
// field and the merge rewrites the boundaries the markers would pin.
// promptCacheChatRequest points cfg.BaseURL at the test server, so this is
// the openai-provider-plus-custom-base_url configuration that
// sendsExplicitCacheBreakpoints excludes before the merge decision runs.
func TestChatCompletions_NoPromptCacheBreakpoint_SelfHostedMergePath(t *testing.T) {
	t.Parallel()

	req := promptCacheChatRequest(t, &latest.ModelConfig{
		Provider:     "openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}, promptCacheMessages())

	assertNoPromptCacheFields(t, req)
}

// TestChatCompletions_PromptCacheBreakpoint_NamedCustomProvider verifies that
// a trusted named custom OpenAI provider (providers: section, resolved by
// pkg/model/provider's factory to the trusted options.WithOpenAIVendor bit)
// gets explicit breakpoints on the Chat Completions API. Its pinned
// api_type=openai_chatcompletions would also select the consecutive-message
// merge used for generic custom providers, but breakpoint eligibility is
// decided first (see convertMessages), so the marked stable boundary
// survives and unmarked content keeps its plain string shape.
func TestChatCompletions_PromptCacheBreakpoint_NamedCustomProvider(t *testing.T) {
	t.Parallel()

	req := promptCacheChatRequest(t, &latest.ModelConfig{
		Provider:     "my_openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}, promptCacheMessages(), options.WithOpenAIVendor(true))

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 2)

	assert.Equal(t, map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{
				"type":                    "text",
				"text":                    "You are helpful.",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, msgs[0])
	assert.Equal(t, map[string]any{"role": "user", "content": "hi"}, msgs[1],
		"unmarked messages must not get breakpoints or change shape")

	_, ok = req["prompt_cache_options"]
	assert.False(t, ok, "prompt_cache_options must not be sent (implicit mode is the default)")
}

// TestResponses_PromptCacheBreakpoint_GPT56 verifies that on the Responses
// API a marked system message gets the breakpoint on its last input_text
// block, a marked user message's string content becomes a one-element
// input_text list carrying the breakpoint, and unmarked messages keep plain
// string content. No request-wide prompt_cache_options field is added.
//
// The client is configured as a trusted named custom OpenAI provider: wire
// tests must point base_url at the test server, and the direct openai
// provider with a custom base_url is the self-hosted case that legitimately
// gets no breakpoints (see
// TestResponses_NoPromptCacheBreakpoint_SelfHostedBaseURL).
func TestResponses_PromptCacheBreakpoint_GPT56(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "my_openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are helpful.", CacheControl: true},
		{Role: chat.MessageRoleUser, Content: "remember this", CacheControl: true},
		{Role: chat.MessageRoleUser, Content: "hi"},
	}, options.WithOpenAIVendor(true))

	input, ok := req["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 3)

	assert.Equal(t, map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{
				"type":                    "input_text",
				"text":                    "You are helpful.",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, input[0])
	assert.Equal(t, map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":                    "input_text",
				"text":                    "remember this",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, input[1])
	assert.Equal(t, map[string]any{"role": "user", "content": "hi"}, input[2],
		"unmarked messages must not get breakpoints or change shape")

	_, ok = req["prompt_cache_options"]
	assert.False(t, ok, "prompt_cache_options must not be sent (implicit mode is the default)")
}

// TestResponses_PromptCacheBreakpoint_MultipartMessage verifies that a marked
// multipart message gets the breakpoint on its last supported content block
// only (here the trailing input_image).
func TestResponses_PromptCacheBreakpoint_MultipartMessage(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "my_openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, []chat.Message{
		{
			Role: chat.MessageRoleUser,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "look at this"},
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/x.png", Detail: chat.ImageURLDetailHigh}},
			},
			CacheControl: true,
		},
	}, options.WithOpenAIVendor(true))

	input, ok := req["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)

	assert.Equal(t, map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "look at this"},
			map[string]any{
				"type":                    "input_image",
				"detail":                  "high",
				"image_url":               "https://example.com/x.png",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, input[0])
}

// TestResponses_NoPromptCacheBreakpoint_OlderModel is the Responses-API
// counterpart of TestChatCompletions_NoPromptCacheBreakpoint_OlderModel. The
// trusted named-custom vendor setup would get breakpoints on gpt-5.6+ (see
// TestResponses_PromptCacheBreakpoint_GPT56), so the older model is the only
// reason none are sent.
func TestResponses_NoPromptCacheBreakpoint_OlderModel(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "my_openai",
		Model:        "gpt-5.5",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, promptCacheMessages(), options.WithOpenAIVendor(true))

	assertNoPromptCacheFields(t, req)
	input, ok := req["input"].([]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"role":    "system",
		"content": []any{map[string]any{"type": "input_text", "text": "You are helpful."}},
	}, input[0])
}

// TestResponses_NoPromptCacheBreakpoint_UnrelatedAlias is the Responses-API
// counterpart of TestChatCompletions_NoPromptCacheBreakpoint_UnrelatedAlias.
func TestResponses_NoPromptCacheBreakpoint_UnrelatedAlias(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "xai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, promptCacheMessages())

	assertNoPromptCacheFields(t, req)
}

// TestResponses_NoPromptCacheBreakpoint_SelfHostedBaseURL verifies that the
// openai provider pointed at a custom base_url — a self-hosted or
// third-party OpenAI-compatible server (vLLM, SGLang, ...), the same
// assumption shouldMergeConsecutiveMessages makes on the Chat Completions
// path — never receives the OpenAI-only explicit breakpoint field, even for
// gpt-5.6+ and even with the trusted OpenAI-vendor bit set: the endpoint is
// still custom.
func TestResponses_NoPromptCacheBreakpoint_SelfHostedBaseURL(t *testing.T) {
	t.Parallel()

	// promptCacheResponsesRequest points cfg.BaseURL at the test server,
	// which is exactly the self-hosted configuration under test.
	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, promptCacheMessages(), options.WithOpenAIVendor(true))

	assertNoPromptCacheFields(t, req)
}

// TestResponses_PromptCacheBreakpoint_ProviderQualifiedModel verifies that an
// explicitly "openai/"-qualified model id behind a gateway provider is
// recognized as a genuine OpenAI vendor and gets breakpoints.
func TestResponses_PromptCacheBreakpoint_ProviderQualifiedModel(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "vercel",
		Model:        "openai/gpt-5.6-sol",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, promptCacheMessages())

	raw, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "prompt_cache_breakpoint")
}

// TestResponses_PromptCacheBreakpoint_NamedCustomProvider verifies that a
// named custom OpenAI provider (providers: section, resolved by
// pkg/model/provider's factory to the trusted options.WithOpenAIVendor bit)
// gets breakpoints like the direct openai provider.
func TestResponses_PromptCacheBreakpoint_NamedCustomProvider(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "my_openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, promptCacheMessages(), options.WithOpenAIVendor(true))

	raw, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "prompt_cache_breakpoint")
}

// TestSendsExplicitCacheBreakpoints_Gating pins the gate itself, in
// particular the self-hosted exclusion that wire tests cannot isolate: the
// openai provider with a custom base_url stays excluded even with the
// trusted vendor bit, while azure, provider-qualified gateway models and
// trusted named custom providers keep breakpoints despite their (always
// custom) base_url.
func TestSendsExplicitCacheBreakpoints_Gating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cfg          latest.ModelConfig
		openAIVendor bool
		want         bool
	}{
		{name: "direct openai, no base_url", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.6"}, want: true},
		{name: "azure custom endpoint", cfg: latest.ModelConfig{Provider: "azure", Model: "gpt-5.6", BaseURL: "https://res.openai.azure.com"}, want: true},
		{name: "provider-qualified model behind gateway", cfg: latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol", BaseURL: "https://gw.example.com/v1"}, want: true},
		{name: "trusted named custom provider", cfg: latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6", BaseURL: "https://proxy.example.com/v1"}, openAIVendor: true, want: true},
		{name: "self-hosted openai base_url (vLLM)", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", BaseURL: "http://box:8000/v1"}, want: false},
		{name: "self-hosted openai base_url ignores vendor bit", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", BaseURL: "http://box:8000/v1"}, openAIVendor: true, want: false},
		{name: "older model", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.5"}, want: false},
		{name: "unrelated alias", cfg: latest.ModelConfig{Provider: "xai", Model: "gpt-5.6"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, sendsExplicitCacheBreakpoints(&tt.cfg, tt.openAIVendor))
		})
	}
}

// newBreakpointTestClient builds a minimal client for direct conversion-level
// tests, mirroring how client_test.go constructs zero-value clients.
func newBreakpointTestClient(provider, model string) *Client {
	return &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: provider, Model: model}}}
}

// TestConvertMessages_CacheControlToolAttachment verifies marker mapping when
// generic conversion expands a source message: a marked tool result with a
// text document attachment injects a follow-up user message, and the breakpoint lands
// on that injected message's last block (the true end of the source message),
// not on the tool message text.
func TestConvertMessages_CacheControlToolAttachment(t *testing.T) {
	t.Parallel()

	c := newBreakpointTestClient("azure", "gpt-5.6")
	converted := c.convertMessages(t.Context(), []chat.Message{
		{
			Role:       chat.MessageRoleTool,
			ToolCallID: "call_9",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "saved"},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.txt", MimeType: "text/plain", Source: chat.DocumentSource{InlineText: "report"}}},
			},
			CacheControl: true,
		},
	})

	require.Len(t, converted, 2)
	raw, err := json.Marshal(converted)
	require.NoError(t, err)

	var msgs []map[string]any
	require.NoError(t, json.Unmarshal(raw, &msgs))

	toolContent, ok := msgs[0]["content"].([]any)
	require.True(t, ok)
	for _, part := range toolContent {
		assert.NotContains(t, part, "prompt_cache_breakpoint")
	}

	userContent, ok := msgs[1]["content"].([]any)
	require.True(t, ok)
	require.Len(t, userContent, 2)
	assert.NotContains(t, userContent[0], "prompt_cache_breakpoint")
	last, ok := userContent[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", last["type"])
	assert.Equal(t, explicitBreakpoint, last["prompt_cache_breakpoint"])
}

func TestChatCompletions_ToolImagesKeepBatchAndCacheOrigins(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		name := "generic"
		if cached {
			name = "explicit cache"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: "azure", Model: "gpt-4o", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}}
			if cached {
				cfg.Model = "gpt-5.6"
			}
			result := func(id, url string, marked bool) chat.Message {
				return chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, CacheControl: marked, MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "result " + id},
					{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: url}},
				}}
			}
			req := promptCacheChatRequest(t, cfg, []chat.Message{
				{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{
					{ID: "A", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
					{ID: "B", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
				}},
				result("B", "data:image/png;base64,Ag==", true),
				{Role: chat.MessageRoleAssistant},
				result("A", "data:image/png;base64,AQ==", false),
				{Role: chat.MessageRoleUser, Content: "continue"},
			})
			messages := req["messages"].([]any)
			if !cached {
				require.Len(t, messages, 4)
				assert.Equal(t, "tool", messages[1].(map[string]any)["role"])
				assert.Equal(t, "tool", messages[2].(map[string]any)["role"])
				parts := messages[3].(map[string]any)["content"].([]any)
				require.Len(t, parts, 5)
				assert.Contains(t, parts[0].(map[string]any)["text"], "tool_call_id=B")
				assert.Contains(t, parts[2].(map[string]any)["text"], "tool_call_id=A")
				assert.Equal(t, "continue", parts[4].(map[string]any)["text"])
				assertNoPromptCacheFields(t, req)
				return
			}
			require.Len(t, messages, 6)
			var roles []any
			for _, msg := range messages {
				roles = append(roles, msg.(map[string]any)["role"])
			}
			assert.Equal(t, []any{"assistant", "tool", "tool", "user", "user", "user"}, roles)
			assert.Equal(t, "B", messages[1].(map[string]any)["tool_call_id"])
			assert.Equal(t, "A", messages[2].(map[string]any)["tool_call_id"])
			for i, id := range []string{"B", "A"} {
				parts := messages[3+i].(map[string]any)["content"].([]any)
				assert.Contains(t, parts[0].(map[string]any)["text"], "tool_call_id="+id)
				image := parts[1].(map[string]any)
				assert.Equal(t, "image_url", image["type"])
				_, marked := image["prompt_cache_breakpoint"]
				assert.Equal(t, cached && id == "B", marked)
			}
		})
	}
}

func TestResponses_ToolImagesKeepCallAndCacheOrigins(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		name := "generic"
		if cached {
			name = "explicit cache"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			model := "gpt-4o"
			if cached {
				model = "gpt-5.6"
			}
			cfg := &latest.ModelConfig{Provider: "azure", Model: model, ProviderOpts: map[string]any{"api_type": "openai_responses"}, Capabilities: &latest.CapabilitiesConfig{Image: true, PDF: true}}
			messages := []chat.Message{
				{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{
					{ID: "A", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
					{ID: "B", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
				}},
				{Role: chat.MessageRoleTool, ToolCallID: "B", Content: "B", CacheControl: true, MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "B"},
					{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,Ag==", Detail: chat.ImageURLDetailHigh}},
				}},
				{Role: chat.MessageRoleTool, ToolCallID: "A", Content: "A", MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "A"},
					{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "a.png", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1}}}},
					{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.pdf", MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: []byte("%PDF")}}},
					{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.txt", MimeType: "text/plain", Source: chat.DocumentSource{InlineText: "report body"}}},
				}},
			}
			req := promptCacheResponsesRequest(t, cfg, messages)
			input := req["input"].([]any)
			require.Len(t, input, 4, "attachments must stay inside their function_call_output")
			for i, id := range []string{"B", "A"} {
				result := input[2+i].(map[string]any)
				assert.Equal(t, "function_call_output", result["type"])
				assert.Equal(t, id, result["call_id"])
				output := result["output"].([]any)
				assert.Equal(t, id, output[0].(map[string]any)["text"])
				image := output[1].(map[string]any)
				assert.Equal(t, "input_image", image["type"])
				_, marked := image["prompt_cache_breakpoint"]
				assert.Equal(t, cached && id == "B", marked)
				if id == "B" {
					require.Len(t, output, 2)
					assert.Equal(t, "data:image/png;base64,Ag==", image["image_url"])
					assert.Equal(t, "high", image["detail"])
				} else {
					require.Len(t, output, 4)
					assert.Equal(t, "data:image/png;base64,AQ==", image["image_url"])
					file := output[2].(map[string]any)
					assert.Equal(t, "input_file", file["type"])
					assert.Equal(t, "report.pdf", file["filename"])
					assert.Equal(t, "data:application/pdf;base64,JVBERg==", file["file_data"])
					assert.Contains(t, output[3].(map[string]any)["text"], "report body")
				}
			}
			if !cached {
				assertNoPromptCacheFields(t, req)
			}
		})
	}
}

// TestConvertMessages_CacheControlEmptyMessages verifies the edge behavior of
// content-less and filtered marked messages: no panic, no marker, and no
// accidental marking of a neighboring message.
func TestConvertMessages_CacheControlEmptyMessages(t *testing.T) {
	t.Parallel()

	c := newBreakpointTestClient("azure", "gpt-5.6")
	converted := c.convertMessages(t.Context(), []chat.Message{
		// Filtered out entirely by the generic converter.
		{Role: chat.MessageRoleAssistant, Content: "", CacheControl: true},
		// Kept, but has no content block that could carry a breakpoint.
		{
			Role:         chat.MessageRoleAssistant,
			ToolCalls:    []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "t", Arguments: "{}"}}},
			CacheControl: true,
		},
		{Role: chat.MessageRoleUser, Content: "hi"},
	})

	raw, err := json.Marshal(converted)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "prompt_cache_breakpoint")

	var msgs []map[string]any
	require.NoError(t, json.Unmarshal(raw, &msgs))
	require.Len(t, msgs, 2)
	assert.Equal(t, "hi", msgs[1]["content"], "unmarked neighbor must stay untouched")
}

// TestConvertMessagesToResponseInput_CacheControlToolItems verifies marker
// mapping for messages that expand into top-level tool items. A marked
// assistant message with tool calls yields output-side text and
// function_call items, neither of which can carry a breakpoint, so it stays
// unmarked without bleeding onto a neighbor. A marked tool result's string
// output is converted to a one-element input_text content array carrying the
// breakpoint, preserving call_id and text exactly.
func TestConvertMessagesToResponseInput_CacheControlToolItems(t *testing.T) {
	t.Parallel()

	c := newBreakpointTestClient("openai", "gpt-5.6")
	input := c.convertMessagesToResponseInput(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "q"},
		{
			Role:         chat.MessageRoleAssistant,
			Content:      "Let me check.",
			ToolCalls:    []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "search", Arguments: "{}"}}},
			CacheControl: true,
		},
		{Role: chat.MessageRoleTool, Content: "result", ToolCallID: "call_1", CacheControl: true},
	})

	raw, err := json.Marshal(input)
	require.NoError(t, err)

	var items []map[string]any
	require.NoError(t, json.Unmarshal(raw, &items))
	require.Len(t, items, 4)

	// The marked assistant message expands into assistant text plus a
	// function_call; neither may be marked.
	for i, item := range items[:3] {
		itemJSON, err := json.Marshal(item)
		require.NoError(t, err)
		assert.NotContains(t, string(itemJSON), "prompt_cache_breakpoint", "item %d must stay unmarked", i)
	}
	assert.Equal(t, "function_call", items[2]["type"])

	assert.Equal(t, map[string]any{
		"type":    "function_call_output",
		"call_id": "call_1",
		"output": []any{
			map[string]any{
				"type":                    "input_text",
				"text":                    "result",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, items[3])
}

// TestResponses_PromptCacheBreakpoint_ToolResult verifies the wire shape of
// a marked tool result end to end: the request's function_call_output
// carries a one-element input_text output array with the breakpoint, while
// the assistant function_call item stays unmarked.
func TestResponses_PromptCacheBreakpoint_ToolResult(t *testing.T) {
	t.Parallel()

	req := promptCacheResponsesRequest(t, &latest.ModelConfig{
		Provider:     "my_openai",
		Model:        "gpt-5.6",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}, []chat.Message{
		{Role: chat.MessageRoleUser, Content: "q"},
		{
			Role:      chat.MessageRoleAssistant,
			Content:   "Let me check.",
			ToolCalls: []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "search", Arguments: "{}"}}},
		},
		{Role: chat.MessageRoleTool, Content: "result", ToolCallID: "call_1", CacheControl: true},
	}, options.WithOpenAIVendor(true))

	input, ok := req["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 4)

	fnCall, ok := input[2].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "function_call", fnCall["type"])
	assert.NotContains(t, fnCall, "prompt_cache_breakpoint")

	assert.Equal(t, map[string]any{
		"type":    "function_call_output",
		"call_id": "call_1",
		"output": []any{
			map[string]any{
				"type":                    "input_text",
				"text":                    "result",
				"prompt_cache_breakpoint": explicitBreakpoint,
			},
		},
	}, input[3])
}

// TestResponses_UsageMapsCacheWriteTokens verifies that the Responses stream
// adapter surfaces input_tokens_details.cache_write_tokens as
// chat.Usage.CacheWriteTokens and keeps the three input buckets mutually
// exclusive: fresh InputTokens excludes both cached and cache-write tokens,
// so the buckets sum back to the provider's input_tokens.
func TestResponses_UsageMapsCacheWriteTokens(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7,"total_tokens":107,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30},"output_tokens_details":{"reasoning_tokens":3}}}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)

	cfg := &latest.ModelConfig{
		Provider:     "openai",
		Model:        "gpt-5.6",
		BaseURL:      server.URL,
		TokenKey:     "MY_TOKEN",
		ProviderOpts: map[string]any{"api_type": "openai_responses"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()

	var usage *chat.Usage
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		if resp.Usage != nil {
			usage = resp.Usage
		}
	}

	require.NotNil(t, usage)
	assert.Equal(t, int64(50), usage.InputTokens, "fresh input must exclude both cached and cache-write tokens")
	assert.Equal(t, int64(7), usage.OutputTokens)
	assert.Equal(t, int64(20), usage.CachedInputTokens)
	assert.Equal(t, int64(30), usage.CacheWriteTokens)
	assert.Equal(t, int64(3), usage.ReasoningTokens)
	assert.Equal(t, int64(100), usage.InputTokens+usage.CachedInputTokens+usage.CacheWriteTokens,
		"the three input buckets must sum back to the provider's input_tokens")
}
