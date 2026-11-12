//go:build !js && !docker_agent_no_openai

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestCloneOptionMatrixOnWire(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"literal", "environment", "inherited", "custom", "routed"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")
			env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret", "CUSTOM_TOKEN": "secret", "ENDPOINT": server.URL, "MODEL": "gpt-5.6"})
			cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", BaseURL: server.URL, ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}, ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}}
			providers := map[string]latest.ProviderConfig{}
			models := map[string]latest.ModelConfig{}
			switch shape {
			case "environment":
				cfg.BaseURL = "${ENDPOINT}"
				cfg.Model = "${MODEL}"
			case "inherited", "custom", "routed":
				cfg.Provider = "custom"
				cfg.BaseURL = ""
				cfg.ProviderOpts = nil
				cfg.ThinkingBudget = nil
				providers["custom"] = latest.ProviderConfig{Provider: "openai", BaseURL: server.URL, TokenKey: "CUSTOM_TOKEN", APIType: "openai_chatcompletions", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}
				if shape == "routed" {
					models["target"] = latest.ModelConfig{Provider: "custom", Model: "gpt-5.6"}
					cfg.Routing = []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}
				}
			}
			original, err := fullTestRegistry().NewWithModels(t.Context(), cfg, models, env, options.WithProviders(providers), options.WithStructuredOutput(&latest.StructuredOutput{Name: "test", Schema: map[string]any{"type": "object", "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1}}}}), options.WithGateway("https://must-bypass.example"))
			require.NoError(t, err)
			clone, err := Clone(t.Context(), original, options.WithNoThinking(), options.WithMaxTokens(128), options.WithStructuredOutput(nil))
			require.NoError(t, err)
			clone, err = Clone(t.Context(), clone, options.WithGeneratingTitle())
			require.NoError(t, err)
			stream, err := clone.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
			require.NoError(t, err)
			drainStream(t, stream)
			stream.Close()
			var payload map[string]any
			require.NoError(t, json.Unmarshal(body(), &payload))
			assert.Equal(t, "none", payload["reasoning_effort"])
			assert.NotContains(t, payload, "response_format")
			assert.InDelta(t, float64(256), payload["max_completion_tokens"], 0)
			cfgClone := clone.BaseConfig()
			assert.True(t, cfgClone.ModelOptions.NoThinking())
			assert.Nil(t, cfgClone.ModelOptions.StructuredOutput())
			if shape == "inherited" || shape == "custom" || shape == "routed" {
				assert.NotEmpty(t, cfgClone.ModelOptions.Providers())
			}
		})
	}
}

func TestCloneRejectsUncopyableConfigWithoutPanic(t *testing.T) {
	t.Parallel()
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, value := range []any{func() {}, make(chan int), cycle} {
		for _, field := range []string{"provider_opts", "structured_output", "providers"} {
			cfg := base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "base"}, RebuildProvider: func(context.Context, *latest.ModelConfig, ...options.Opt) (Provider, error) {
				t.Error("must reject before rebuilding")
				return nil, nil
			}}
			switch field {
			case "provider_opts":
				cfg.ModelConfig.ProviderOpts = map[string]any{"bad": value}
			case "structured_output":
				cfg.ModelOptions = options.Apply(options.WithStructuredOutput(&latest.StructuredOutput{Schema: map[string]any{"bad": value}}))
			case "providers":
				cfg.ModelOptions = options.Apply(options.WithProviders(map[string]latest.ProviderConfig{"bad": {ProviderOpts: map[string]any{"bad": value}}}))
			}
			p := &fakeProvider{config: cfg}
			clone, err := Clone(t.Context(), p)
			assert.Nil(t, clone)
			require.ErrorIs(t, err, ErrUncopyableConfig)
			legacy := CloneWithOptions(t.Context(), p)
			_, err = legacy.CreateChatCompletionStream(t.Context(), nil, nil)
			require.ErrorIs(t, err, ErrUncopyableConfig)
		}
	}
}

func TestCloneCopiesYAMLAndHiddenFields(t *testing.T) {
	t.Parallel()
	var cfg latest.ModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(`provider: test
model: base
max_tokens: 1024
track_usage: false
parallel_tool_calls: false
provider_opts:
  extra_body:
    chat_template_kwargs:
      enable_thinking: false
  strict_tools: [one, two]
`), &cfg))
	cfg.Name = "named"
	cfg.DisplayModel = "display"
	copied, _, err := mergeCloneOptions(base.Config{ModelConfig: cfg}, nil)
	require.NoError(t, err)
	assert.Equal(t, "named", copied.Name)
	assert.Equal(t, "display", copied.DisplayModel)
	require.NotNil(t, copied.TrackUsage)
	assert.False(t, *copied.TrackUsage)
	require.NotNil(t, copied.ParallelToolCalls)
	assert.False(t, *copied.ParallelToolCalls)
	assert.Equal(t, int64(1024), *copied.MaxTokens)
	assert.Equal(t, []any{"one", "two"}, copied.ProviderOpts["strict_tools"])
	assert.Equal(t, false, copied.ProviderOpts["extra_body"].(map[string]any)["chat_template_kwargs"].(map[string]any)["enable_thinking"])
	var hidden []string
	typ := reflect.TypeFor[latest.ModelConfig]()
	for field := range typ.Fields() {
		if field.Tag.Get("json") == "-" {
			hidden = append(hidden, field.Name)
		}
	}
	assert.ElementsMatch(t, []string{"Name", "DisplayModel"}, hidden, "checked copy must preserve every JSON-excluded field")
}

func TestClonePreservesStructuredOutputOnWire(t *testing.T) {
	t.Parallel()
	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")
	original, err := fullTestRegistry().New(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}}, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret"}), options.WithStructuredOutput(&latest.StructuredOutput{Name: "counter", Schema: map[string]any{"type": "object", "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1}}}}))
	require.NoError(t, err)
	clone, err := Clone(t.Context(), original, options.WithMaxTokens(512))
	require.NoError(t, err)
	stream, err := clone.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
	require.NoError(t, err)
	drainStream(t, stream)
	stream.Close()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(body(), &payload))
	require.Contains(t, payload, "response_format")
	format := payload["response_format"].(map[string]any)["json_schema"].(map[string]any)
	assert.Equal(t, "counter", format["name"])
	assert.InDelta(t, float64(1), format["schema"].(map[string]any)["properties"].(map[string]any)["count"].(map[string]any)["minimum"], 0)
}

func TestCloneConfiguredOpenAIThinkingOffOnWire(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"o3", "gpt-4o"} {
		for _, shape := range []string{"literal", "custom", "routed"} {
			for _, budget := range []*latest.ThinkingBudget{{Effort: "none"}, {}} {
				t.Run(model+"/"+shape+"/"+budget.Effort, func(t *testing.T) {
					t.Parallel()
					server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")
					cfg := &latest.ModelConfig{Provider: "openai", Model: model, BaseURL: server.URL, ThinkingBudget: budget, ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}}
					providers := map[string]latest.ProviderConfig{"openai": {ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}}
					models := map[string]latest.ModelConfig{}
					if shape != "literal" {
						cfg.Provider = "custom"
						cfg.BaseURL = ""
						providers["custom"] = latest.ProviderConfig{BaseURL: server.URL, TokenKey: "OPENAI_API_KEY", APIType: "openai_chatcompletions", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}
					}
					if shape == "routed" {
						models["target"] = *cfg
						cfg.Routing = []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}
					}
					p, err := fullTestRegistry().NewWithModels(t.Context(), cfg, models, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fake"}), options.WithProviders(providers))
					require.NoError(t, err)
					for range 3 {
						stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
						require.NoError(t, err)
						drainStream(t, stream)
						stream.Close()
						var payload map[string]any
						require.NoError(t, json.Unmarshal(body(), &payload))
						if model == "o3" {
							assert.Equal(t, "low", payload["reasoning_effort"])
						} else {
							assert.NotContains(t, payload, "reasoning_effort")
						}
						p, err = Clone(t.Context(), p, options.WithCompacting(), options.WithStructuredOutput(nil), options.WithMaxTokens(8192))
						require.NoError(t, err)
					}
				})
			}
		}
	}
}

func TestCloneNumericPrecisionOnWire(t *testing.T) {
	t.Parallel()
	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")
	schema := map[string]any{"type": "object", "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": int64(9007199254740993)}}}
	original, err := fullTestRegistry().New(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_chatcompletions", "extra_body": map[string]any{"exact_limit": int64(9007199254740993)}}}, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret"}), options.WithStructuredOutput(&latest.StructuredOutput{Name: "counter", Schema: schema}))
	require.NoError(t, err)
	for range 3 {
		original, err = Clone(t.Context(), original)
		require.NoError(t, err)
		stream, err := original.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
		require.NoError(t, err)
		drainStream(t, stream)
		stream.Close()
		var payload map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body()))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&payload))
		assert.Equal(t, json.Number("9007199254740993"), payload["exact_limit"])
		bound := payload["response_format"].(map[string]any)["json_schema"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["count"].(map[string]any)["minimum"]
		assert.Equal(t, json.Number("9007199254740993"), bound)
	}
}

func TestCloneRoutedTokenCapsPreserveConstructionDefaults(t *testing.T) {
	t.Parallel()
	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")
	cfg := &latest.ModelConfig{Provider: "custom", Model: "gpt-4o", MaxTokens: new(int64(4096)), Routing: []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}}
	models := map[string]latest.ModelConfig{"target": {Provider: "custom", Model: "gpt-5.6", MaxTokens: new(int64(8192))}}
	original, err := fullTestRegistry().NewWithModels(t.Context(), cfg, models, environment.NewMapEnvProvider(map[string]string{"KEY": "fake"}), options.WithMaxTokens(4096), options.WithProviders(map[string]latest.ProviderConfig{"custom": {BaseURL: server.URL, TokenKey: "KEY", APIType: "openai_chatcompletions", MaxTokens: new(int64(16384))}}))
	require.NoError(t, err)
	request := func(p Provider, tokenCap int64) {
		t.Helper()
		for _, message := range []string{"hello", "unrelated"} {
			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: message}}, nil)
			require.NoError(t, err)
			drainStream(t, stream)
			stream.Close()
			var payload map[string]any
			require.NoError(t, json.Unmarshal(body(), &payload))
			wantModel, wantCap := "gpt-5.6", tokenCap
			if message == "unrelated" {
				wantModel = "gpt-4o"
			}
			if tokenCap == 0 {
				wantCap = 8192
				if message == "unrelated" {
					wantCap = 16384
				}
			} else if message == "hello" && tokenCap < 256 {
				wantCap = 256
			}
			assert.Equal(t, wantModel, payload["model"])
			field := "max_completion_tokens"
			if message == "unrelated" {
				field = "max_tokens"
			}
			assert.InDelta(t, float64(wantCap), payload[field], 0)
		}
	}
	request(original, 0)
	for _, tc := range []struct {
		opts        []options.Opt
		tokenCap    int64
		inheritable bool
	}{
		{[]options.Opt{options.WithAuxiliaryMaxTokens(128), options.WithMaxTokens(64)}, 64, false},
		{[]options.Opt{options.WithMaxTokens(64), options.WithAuxiliaryMaxTokens(128)}, 128, true},
	} {
		p, err := fullTestRegistry().NewWithModels(t.Context(), cfg, models, environment.NewMapEnvProvider(map[string]string{"KEY": "fake"}), append(options.FromModelOptions(original.BaseConfig().ModelOptions), append(tc.opts, options.WithNoThinking())...)...)
		require.NoError(t, err)
		baseCfg := p.BaseConfig()
		assert.Equal(t, tc.tokenCap, baseCfg.ModelOptions.MaxTokens())
		assert.Equal(t, tc.inheritable, baseCfg.ModelOptions.MaxTokensInheritable())
		wireCap := tc.tokenCap
		if !tc.inheritable {
			wireCap = 0
		}
		request(p, wireCap)
	}
	plain, err := Clone(t.Context(), original)
	require.NoError(t, err)
	request(plain, 0)
	plain, err = Clone(t.Context(), original, options.WithNoThinking(), options.WithGeneratingTitle())
	require.NoError(t, err)
	request(plain, 0)
	for _, opts := range [][]options.Opt{
		{options.WithMaxTokens(128)},
		{options.WithAuxiliaryMaxTokens(1024), options.WithMaxTokens(128)},
		{options.WithMaxTokens(1024), options.WithAuxiliaryMaxTokens(128)},
	} {
		clone, err := Clone(t.Context(), original, append(opts, options.WithNoThinking())...)
		require.NoError(t, err)
		request(clone, 128)
		clone, err = Clone(t.Context(), clone, options.WithMaxTokens(0))
		require.NoError(t, err)
		request(clone, 128)
	}
	request(original, 0)
}
