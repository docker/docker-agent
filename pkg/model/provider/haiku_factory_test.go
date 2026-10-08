//go:build !js && !docker_agent_no_openai

package provider

import (
	"context"
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
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

type haikuTransport func(*http.Request) (*http.Response, error)

func (f haikuTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHaiku55FactoryAndCloneDisabledWire(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"anthropic", "custom", "google", "amazon-bedrock"} {
		for _, mode := range []string{"zero", "none", "inherited", "NoThinking", "clone", "env-zero", "env-none", "env-inherited-zero", "env-inherited"} {
			t.Run(host+"/"+mode, func(t *testing.T) {
				t.Parallel()
				cfg := &latest.ModelConfig{Provider: host, Model: "claude-haiku-5-5", Temperature: new(0.2), TopP: new(0.8), ProviderOpts: map[string]any{"top_k": 5}}
				inherited := &latest.ThinkingBudget{Effort: "high"}
				custom := map[string]latest.ProviderConfig{"custom": {Provider: "anthropic", ThinkingBudget: inherited}}
				if host == "google" {
					cfg.ProviderOpts["publisher"] = "anthropic"
					cfg.ProviderOpts["project"] = "test-project"
					cfg.ProviderOpts["location"] = "global"
				}
				if host == "amazon-bedrock" {
					cfg.Model = "global.anthropic.claude-haiku-5-5"
					cfg.ProviderOpts["region"] = "us-east-1"
				}
				modelID := cfg.Model
				if strings.HasPrefix(mode, "env-") {
					cfg.Model = "${env.HAIKU_MODEL}"
				}
				switch strings.TrimPrefix(mode, "env-") {
				case "zero":
					cfg.ThinkingBudget = &latest.ThinkingBudget{}
				case "none":
					cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
				case "inherited-zero":
					custom["custom"] = latest.ProviderConfig{Provider: "anthropic", ThinkingBudget: &latest.ThinkingBudget{}}
					if host != "custom" {
						cfg.ThinkingBudget = &latest.ThinkingBudget{}
					}
				case "inherited":
					custom["custom"] = latest.ProviderConfig{Provider: "anthropic", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}
					if host != "custom" {
						cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
					}
				default:
					cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "max"}
				}
				before, err := json.Marshal(cfg)
				require.NoError(t, err)
				customBefore, err := json.Marshal(custom)
				require.NoError(t, err)
				var requests []map[string]any
				capture := options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
					return haikuTransport(func(req *http.Request) (*http.Response, error) {
						var body map[string]any
						require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
						requests = append(requests, body)
						if host == "google" {
							require.Len(t, req.Header.Values("anthropic-beta"), 1)
							assert.Contains(t, req.URL.Path, "claude-haiku-5-5:streamRawPredict")
							assert.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
							assert.Empty(t, req.Header.Get("X-Api-Key"))
						}
						if host == "amazon-bedrock" {
							assert.Contains(t, req.URL.Path, "converse-stream")
						}
						return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"test capture"},"message":"test capture"}`)), Request: req}, nil
					})
				})
				opts := []options.Opt{options.WithProviders(custom), capture}
				if mode == "NoThinking" {
					opts = append(opts, options.WithNoThinking())
				}
				registry := fullTestRegistry()
				// Supply hermetic auth while exercising the same Model Garden rewrite.
				registry.factories["google"] = func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
					return vertexai.NewClientWithTokenSource(ctx, cfg, env, func(context.Context) (string, error) { return "test-token", nil }, opts...)
				}
				p, err := registry.New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test", "AWS_BEARER_TOKEN_BEDROCK": "test", "HAIKU_MODEL": modelID}), opts...)
				require.NoError(t, err)
				if mode == "clone" {
					original := p
					p = CloneWithOptions(t.Context(), p, options.WithNoThinking())
					require.NotSame(t, original, p)
					assert.Equal(t, "max", original.BaseConfig().ModelConfig.ThinkingBudget.Effort)
				}
				stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "test"}}, nil)
				if err == nil {
					_, err = stream.Recv()
					stream.Close()
				}
				require.Error(t, err, "stub refuses every captured request")
				require.Len(t, requests, 1)
				body := requests[0]
				if host == "amazon-bedrock" {
					fields, ok := body["additionalModelRequestFields"].(map[string]any)
					require.True(t, ok, "additional fields missing")
					assert.Equal(t, map[string]any{"type": "disabled"}, fields["thinking"])
					assert.NotContains(t, fields, "top_k")
					assert.NotContains(t, fields, "output_config")
					inference := body["inferenceConfig"].(map[string]any)
					assert.NotContains(t, inference, "temperature")
					assert.NotContains(t, inference, "topP")
				} else {
					assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
					for _, key := range []string{"output_config", "temperature", "top_p", "top_k"} {
						assert.NotContains(t, body, key)
					}
				}
				after, err := json.Marshal(cfg)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				customAfter, err := json.Marshal(custom)
				require.NoError(t, err)
				assert.Equal(t, customBefore, customAfter)
			})
		}
	}
}

func TestHaiku55InheritedBudgetPrecedence(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude-haiku-5-5", "${env.HAIKU_MODEL}"} {
		for _, tc := range []struct {
			modelEffort, providerEffort string
			clone                       bool
			off                         bool
		}{
			{"medium", "none", false, false},
			{"none", "medium", false, true},
			{"medium", "medium", true, true},
		} {
			t.Run(model+"/"+tc.modelEffort+"/"+tc.providerEffort+map[bool]string{true: "/clone"}[tc.clone], func(t *testing.T) {
				t.Parallel()
				cfg := &latest.ModelConfig{Provider: "custom", Model: model, ThinkingBudget: &latest.ThinkingBudget{Effort: tc.modelEffort}}
				providers := map[string]latest.ProviderConfig{"custom": {Provider: "anthropic", ThinkingBudget: &latest.ThinkingBudget{Effort: tc.providerEffort}}}
				var got map[string]any
				capture := options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
					return haikuTransport(func(req *http.Request) (*http.Response, error) {
						require.NoError(t, json.NewDecoder(req.Body).Decode(&got))
						return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"capture"}}`)), Request: req}, nil
					})
				})
				p, err := fullTestRegistry().New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test", "HAIKU_MODEL": "claude-haiku-5-5"}), options.WithProviders(providers), capture)
				require.NoError(t, err)
				if tc.clone {
					p = CloneWithOptions(t.Context(), p, options.WithNoThinking())
				}
				stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "test"}}, nil)
				require.NoError(t, err)
				_, err = stream.Recv()
				stream.Close()
				require.Error(t, err)
				if tc.off {
					assert.Equal(t, map[string]any{"type": "disabled"}, got["thinking"])
					assert.NotContains(t, got, "output_config")
				} else {
					assert.Equal(t, "adaptive", got["thinking"].(map[string]any)["type"])
					assert.Equal(t, "medium", got["output_config"].(map[string]any)["effort"])
				}
				assert.Equal(t, tc.modelEffort, cfg.ThinkingBudget.Effort)
				assert.Equal(t, tc.providerEffort, providers["custom"].ThinkingBudget.Effort)
			})
		}
	}
}
