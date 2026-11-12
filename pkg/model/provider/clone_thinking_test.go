//go:build !js

package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestCloneRoutedClaudeStrictOffOnWire(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"anthropic", "amazon-bedrock"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			var body map[string]any
			calls := 0
			wrapper := options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
				return thinkingRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					body = nil
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"offline capture"}}`)), Request: req}, nil
				})
			})
			model := "claude-haiku-4-5"
			if provider == "amazon-bedrock" {
				model = "anthropic.claude-haiku-4-5-20251001-v1:0"
			}
			models := map[string]latest.ModelConfig{"target": {Provider: provider, Model: model, MaxTokens: new(int64(16384)), ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"region": "us-east-1"}}}
			cfg := &latest.ModelConfig{Provider: provider, Model: model, Routing: []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}}
			env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "fake", "AWS_BEARER_TOKEN_BEDROCK": "fake"})
			original, err := fullTestRegistry().NewWithModels(t.Context(), cfg, models, env, wrapper, options.WithMaxTokens(4096))
			require.NoError(t, err)
			request := func(p Provider, message string, thinking bool, tokenCap int64) {
				t.Helper()
				before := calls
				stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: message}}, nil)
				if err == nil {
					_, err = stream.Recv()
					stream.Close()
				}
				require.Error(t, err)
				require.Equal(t, before+1, calls)
				fields := body
				if provider == "amazon-bedrock" {
					if additional, exists := body["additionalModelRequestFields"]; exists {
						fields = additional.(map[string]any)
					}
					assert.InDelta(t, float64(tokenCap), body["inferenceConfig"].(map[string]any)["maxTokens"], 0)
				} else {
					assert.InDelta(t, float64(tokenCap), body["max_tokens"], 0)
				}
				if thinking {
					require.Contains(t, fields, "thinking")
					assert.InDelta(t, float64(2048), fields["thinking"].(map[string]any)["budget_tokens"], 0)
				} else {
					assert.NotContains(t, fields, "thinking")
					assert.NotContains(t, fields, "reasoning_config")
				}
			}
			defaultCap := int64(8192)
			if provider == "amazon-bedrock" {
				defaultCap = 16384
			}
			request(original, "hello", true, defaultCap)
			for _, purpose := range []options.Opt{options.WithGeneratingTitle(), options.WithStructuredOutput(nil), options.WithCompacting()} {
				clone, err := Clone(t.Context(), original, options.WithNoThinking(), options.WithMaxTokens(128), purpose)
				require.NoError(t, err)
				for _, tokenCap := range []int64{128, 0, 64, 1024} {
					clone, err = Clone(t.Context(), clone, options.WithMaxTokens(tokenCap))
					require.NoError(t, err)
					requested := tokenCap
					if tokenCap == 0 {
						requested = 128
					}
					assert.Equal(t, requested, *clone.BaseConfig().ModelConfig.MaxTokens)
					baseCfg := clone.BaseConfig()
					assert.Equal(t, requested, baseCfg.ModelOptions.MaxTokens())
					wireCap := requested
					if provider == "anthropic" && wireCap < 256 {
						wireCap = 256
					}
					request(clone, "hello", false, wireCap)
					request(clone, "unrelated", false, wireCap)
					clone, err = Clone(t.Context(), clone)
					require.NoError(t, err)
					request(clone, "hello", false, wireCap)
					request(clone, "unrelated", false, wireCap)
				}
			}
			for _, opts := range [][]options.Opt{
				{options.WithMaxTokens(1024), nil, options.WithMaxTokens(0)},
				{options.WithAuxiliaryMaxTokens(1024), options.WithMaxTokens(0)},
				{options.WithMaxTokens(1024), options.WithAuxiliaryMaxTokens(0)},
			} {
				clone, err := Clone(t.Context(), original, options.WithNoThinking(), options.WithMaxTokens(128))
				require.NoError(t, err)
				clone, err = Clone(t.Context(), clone, opts...)
				require.NoError(t, err)
				for range 2 {
					cfg := clone.BaseConfig()
					assert.Equal(t, int64(1024), *cfg.ModelConfig.MaxTokens)
					assert.Equal(t, int64(1024), cfg.ModelOptions.MaxTokens())
					request(clone, "hello", false, 1024)
					request(clone, "unrelated", false, 1024)
					clone, err = Clone(t.Context(), clone)
					require.NoError(t, err)
				}
			}
			request(original, "hello", true, defaultCap)
		})
	}
}

type thinkingRoundTripper func(*http.Request) (*http.Response, error)

func (f thinkingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCloneConfiguredThinkingOffOnWire(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"literal-none", "literal-zero", "environment", "provider-none", "provider-zero", "routed-none", "routed-zero", "model-wins"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			var body map[string]any
			wrapper := options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
				return thinkingRoundTripper(func(req *http.Request) (*http.Response, error) {
					body = nil
					require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
					return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"offline capture"}}`)), Request: req}, nil
				})
			})
			cfg := &latest.ModelConfig{Provider: "custom", Model: "claude-haiku-4-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}
			pc := latest.ProviderConfig{Provider: "anthropic", BaseURL: "https://offline.invalid", TokenKey: "KEY", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}}
			switch shape {
			case "literal-zero", "routed-zero":
				cfg.ThinkingBudget = &latest.ThinkingBudget{}
			case "environment":
				cfg.Model = "${MODEL}"
				pc.BaseURL = "${ENDPOINT}"
			case "provider-none", "provider-zero":
				cfg.ThinkingBudget = nil
				pc.ThinkingBudget = &latest.ThinkingBudget{}
				if shape == "provider-none" {
					pc.ThinkingBudget.Effort = "none"
				}
			case "model-wins":
				cfg.ThinkingBudget = &latest.ThinkingBudget{Tokens: 8192}
				pc.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
			}
			models := map[string]latest.ModelConfig{}
			if strings.HasPrefix(shape, "routed-") {
				models["target"] = *cfg
				cfg.Routing = []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}
			}
			original, err := fullTestRegistry().NewWithModels(t.Context(), cfg, models, environment.NewMapEnvProvider(map[string]string{"KEY": "fake", "MODEL": "claude-haiku-4-5", "ENDPOINT": "https://offline.invalid"}), wrapper, options.WithProviders(map[string]latest.ProviderConfig{"custom": pc}))
			require.NoError(t, err)
			p := original
			for range 3 {
				baseCfg := p.BaseConfig()
				if len(baseCfg.ModelConfig.Routing) == 0 {
					assert.Equal(t, shape != "model-wins", baseCfg.ModelOptions.NoThinking())
				}
				stream, requestErr := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
				if requestErr == nil {
					_, requestErr = stream.Recv()
					stream.Close()
				}
				require.Error(t, requestErr)
				if shape == "model-wins" {
					require.Contains(t, body, "thinking")
					assert.InDelta(t, float64(8192), body["thinking"].(map[string]any)["budget_tokens"], 0)
				} else {
					assert.NotContains(t, body, "thinking")
				}
				p, err = Clone(t.Context(), p, options.WithStructuredOutput(nil), options.WithMaxTokens(16384), options.WithCompacting())
				require.NoError(t, err)
			}
		})
	}
}

func TestCloneDirectClaudeRetainsExplicitCapOnZero(t *testing.T) {
	t.Parallel()
	var body map[string]any
	wrapper := options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
		return thinkingRoundTripper(func(req *http.Request) (*http.Response, error) {
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"offline capture"}}`)), Request: req}, nil
		})
	})
	original, err := fullTestRegistry().New(t.Context(), &latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5"}, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "fake"}), wrapper)
	require.NoError(t, err)
	request := func(p Provider, tokenCap int64) {
		t.Helper()
		stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
		if err == nil {
			_, err = stream.Recv()
			stream.Close()
		}
		require.Error(t, err)
		assert.InDelta(t, float64(tokenCap), body["max_tokens"], 0)
	}
	request(original, 8192)
	p, err := Clone(t.Context(), original, options.WithMaxTokens(128), options.WithNoThinking())
	require.NoError(t, err)
	for _, opts := range [][]options.Opt{nil, {options.WithMaxTokens(0)}, nil} {
		p, err = Clone(t.Context(), p, opts...)
		require.NoError(t, err)
		cfg := p.BaseConfig()
		assert.Equal(t, int64(128), cfg.ModelOptions.MaxTokens())
		assert.Equal(t, int64(128), *cfg.ModelConfig.MaxTokens)
		request(p, 256)
	}
	for _, opts := range [][]options.Opt{
		{options.WithMaxTokens(1024), options.WithMaxTokens(0)},
		{options.WithAuxiliaryMaxTokens(1024), options.WithMaxTokens(0)},
		{options.WithMaxTokens(1024), options.WithAuxiliaryMaxTokens(0)},
	} {
		clone, err := Clone(t.Context(), p, opts...)
		require.NoError(t, err)
		for range 2 {
			cfg := clone.BaseConfig()
			assert.Equal(t, int64(1024), cfg.ModelOptions.MaxTokens())
			assert.Equal(t, int64(1024), *cfg.ModelConfig.MaxTokens)
			request(clone, 1024)
			clone, err = Clone(t.Context(), clone)
			require.NoError(t, err)
		}
	}
	request(original, 8192)
}
