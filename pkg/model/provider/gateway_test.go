package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

type gatewayEnvFunc func(context.Context, string) (string, bool)

func (f gatewayEnvFunc) Get(ctx context.Context, name string) (string, bool) {
	return f(ctx, name)
}

func TestGatewayProviderRequests(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		provider   string
		model      string
		factory    Factory
		path       string
		forward    string
		authHeader string
		response   string
	}{
		{
			provider: "openai", model: "gpt-4o", factory: openAITestFactory,
			path: "/gateway/v1/chat/completions", forward: "https://api.openai.com/v1",
			authHeader: "Authorization",
			response:   "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
		{
			provider: "anthropic", model: "claude-sonnet-4-5", factory: anthropicTestFactory,
			path: "/gateway/v1/messages", forward: "https://api.anthropic.com/",
			authHeader: "X-Api-Key",
			response:   "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			provider: "google", model: "gemini-2.5-flash", factory: googleTestFactory,
			path: "/gateway/v1beta/models/gemini-2.5-flash:streamGenerateContent", forward: "https://generativelanguage.googleapis.com/",
			authHeader: "X-Goog-Api-Key",
			response:   "event: keepalive\ndata: {}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}\n\n",
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()

			type request struct {
				path   string
				query  url.Values
				header http.Header
				body   map[string]any
			}
			requests := make(chan request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				select {
				case requests <- request{r.URL.Path, r.URL.Query(), r.Header.Clone(), body}:
				default:
					t.Error("unexpected extra gateway request")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.response)
			}))
			t.Cleanup(server.Close)

			var events []string
			tokenCalls := 0
			env := gatewayEnvFunc(func(_ context.Context, name string) (string, bool) {
				require.Equal(t, environment.DockerDesktopTokenEnv, name)
				events = append(events, "token")
				tokenCalls++
				return fmt.Sprintf("token-%d", tokenCalls), true
			})
			cfg := &latest.ModelConfig{Provider: tc.provider, Model: tc.model, Name: "test-model"}
			p, err := tc.factory(t.Context(), cfg, env,
				options.WithGateway(server.URL+"/gateway?tier=pro&tier=team"),
				options.WithGeneratingTitle(), options.WithCompacting(),
				options.WithEncryptedConfig("encrypted-config"),
				options.WithHTTPTransportWrapper(func(rt http.RoundTripper) http.RoundTripper {
					events = append(events, "wrap")
					require.NotNil(t, rt)
					return rt
				}),
			)
			require.NoError(t, err)
			assert.Empty(t, events, "loopback gateway setup must remain lazy")

			for i := range 2 {
				stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
				require.NoError(t, err)
				for {
					_, err = stream.Recv()
					if err != nil {
						break
					}
				}
				stream.Close()
				require.ErrorIs(t, err, io.EOF)

				require.Len(t, requests, 1)
				got := <-requests
				token := fmt.Sprintf("token-%d", i+1)
				assert.Equal(t, tc.path, got.path)
				assert.Equal(t, []string{"pro", "team"}, got.query["tier"])
				assert.Equal(t, "Bearer "+token, got.header.Get("Authorization"))
				if tc.authHeader != "Authorization" {
					assert.Equal(t, token, got.header.Get(tc.authHeader))
				}
				assert.Equal(t, tc.forward, got.header.Get("X-Cagent-Forward"))
				assert.Equal(t, tc.provider, got.header.Get("X-Cagent-Provider"))
				assert.Equal(t, tc.model, got.header.Get("X-Cagent-Model"))
				assert.Equal(t, "test-model", got.header.Get("X-Cagent-Model-Name"))
				assert.Equal(t, "1", got.header.Get("X-Cagent-GeneratingTitle"))
				assert.Equal(t, "1", got.header.Get("X-Cagent-Compacting"))
				assert.Equal(t, "encrypted-config", got.body[httpclient.EncryptedConfigBodyField])
			}
			assert.Equal(t, []string{"token", "wrap", "token", "wrap"}, events)
		})
	}
}

func TestGatewayProviderErrorTiming(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"openai", "anthropic", "google"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			factory := fullTestRegistry().factories[provider]
			for _, tc := range []struct {
				name         string
				gateway      string
				token        string
				construction bool
			}{
				{name: "missing token fails at construction", gateway: "https://api.docker.com/models", construction: true},
				{name: "lost token fails at request", gateway: "https://api.docker.com/models", token: "initial-token"},
				{name: "invalid URL fails at request", gateway: "http://gateway.invalid/%zz"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					token := tc.token
					env := gatewayEnvFunc(func(_ context.Context, _ string) (string, bool) { return token, token != "" })
					wrapped := false
					p, err := factory(t.Context(), &latest.ModelConfig{Provider: provider, Model: "test-model"}, env,
						options.WithGateway(tc.gateway),
						options.WithHTTPTransportWrapper(func(rt http.RoundTripper) http.RoundTripper {
							wrapped = true
							return rt
						}),
					)
					if tc.construction {
						require.EqualError(t, err, "sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
					} else {
						require.NoError(t, err)
						token = ""
						_, err = p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
						if tc.token != "" {
							require.EqualError(t, err, base.NoDesktopTokenErrorMessage)
						} else {
							var urlErr *url.Error
							require.ErrorAs(t, err, &urlErr)
							require.EqualError(t, err, "invalid gateway URL: "+urlErr.Error())
						}
					}
					assert.False(t, wrapped, "auth and URL errors must precede transport construction")
				})
			}
		})
	}
}
