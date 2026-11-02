//go:build !js && !docker_agent_no_openai

package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/testutil/geminiintegration"
)

func TestRegistryUnauthenticatedOpenAIEndpoint(t *testing.T) {
	for _, key := range []string{"", "host-key"} {
		t.Run("host-key="+key, func(t *testing.T) {
			auth := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer server.Close()
			cfg := &latest.ModelConfig{
				Provider: "openai", Model: "local-model", BaseURL: server.URL,
				ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
			}
			p, err := fullTestRegistry().New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": key}))
			require.NoError(t, err)
			for range 2 {
				stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
				require.NoError(t, err)
				for {
					if _, err := stream.Recv(); err != nil {
						break
					}
				}
				stream.Close()
				assert.Empty(t, <-auth)
				p, err = Clone(t.Context(), p)
				require.NoError(t, err)
			}
		})
	}
}

func TestRegistryEndpointEnvironmentPrecedence(t *testing.T) {
	for _, name := range []string{"openai", "anthropic"} {
		t.Run(name, func(t *testing.T) {
			received := make(chan string, 4)
			newServer := func(label string) *httptest.Server {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					received <- label
					w.Header().Set("Content-Type", "text/event-stream")
					if name == "anthropic" {
						_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					} else {
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
					}
				}))
				t.Cleanup(server.Close)
				return server
			}
			envServer, explicitServer := newServer("environment"), newServer("explicit")
			if name == "openai" {
				t.Setenv("OPENAI_BASE_URL", envServer.URL)
			} else {
				t.Setenv("ANTHROPIC_BASE_URL", envServer.URL)
			}
			for _, explicit := range []bool{false, true} {
				cfg := &latest.ModelConfig{Provider: name, Model: "test-model"}
				expected := "environment"
				if explicit {
					cfg.BaseURL = explicitServer.URL
					expected = "explicit"
				}
				p, err := fullTestRegistry().New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test", "ANTHROPIC_API_KEY": "test"}))
				require.NoError(t, err)
				for range 2 {
					stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
					require.NoError(t, err)
					for {
						if _, err := stream.Recv(); err != nil {
							break
						}
					}
					stream.Close()
					assert.Equal(t, expected, <-received)
					p, err = Clone(t.Context(), p)
					require.NoError(t, err)
				}
			}
		})
	}
}

func TestRegistryExplicitCanonicalEndpointBypassesGateway(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: catalog.Lookup("openai").BaseURL, TokenKey: "CUSTOM_KEY"}
	p, err := fullTestRegistry().New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"CUSTOM_KEY": "direct-key"}), options.WithGateway("https://gateway.example"))
	require.NoError(t, err)
	initial := p.BaseConfig()
	assert.Empty(t, initial.ModelOptions.Gateway())
	assert.Equal(t, "CUSTOM_KEY", p.BaseConfig().ModelConfig.TokenKey)
	cloned, err := Clone(t.Context(), p)
	require.NoError(t, err)
	rebuilt := cloned.BaseConfig()
	assert.Empty(t, rebuilt.ModelOptions.Gateway())
	assert.Equal(t, cfg.BaseURL, cloned.BaseConfig().ModelConfig.BaseURL)
}

func TestRegistryGoogleEndpointEnvironmentPrecedence(t *testing.T) {
	geminiintegration.Require(t)

	for _, backend := range []string{"gemini", "vertex"} {
		t.Run(backend, func(t *testing.T) {
			const envURL = "https://environment.example"
			if backend == "vertex" {
				t.Setenv("GOOGLE_VERTEX_BASE_URL", envURL)
			} else {
				t.Setenv("GOOGLE_GEMINI_BASE_URL", envURL)
			}
			for _, tt := range []struct {
				name     string
				baseURL  string
				endpoint string
			}{
				{name: "omitted", endpoint: envURL},
				{name: "explicit", baseURL: "https://explicit.example", endpoint: "https://explicit.example"},
				{name: "explicit catalog endpoint", baseURL: catalog.Lookup("google").BaseURL, endpoint: catalog.Lookup("google").BaseURL},
			} {
				t.Run(tt.name, func(t *testing.T) {
					calls := 0
					opts := []options.Opt{options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
						return cloneRoundTripper(func(req *http.Request) (*http.Response, error) {
							calls++
							assert.Equal(t, tt.endpoint, req.URL.Scheme+"://"+req.URL.Host)
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
								Body:       io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}\n\n")),
								Request:    req,
							}, nil
						})
					})}
					cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.0-flash", BaseURL: tt.baseURL}
					if backend == "vertex" {
						cfg.ProviderOpts = map[string]any{"project": "test-project", "location": "us-central1"}
						opts = append(opts, options.WithTokenSource(func(context.Context) (string, error) { return "test-token", nil }))
					}
					p, err := fullTestRegistry().New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"}), opts...)
					require.NoError(t, err)
					for range 2 {
						stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
						require.NoError(t, err)
						drainStream(t, stream)
						stream.Close()
						p, err = Clone(t.Context(), p)
						require.NoError(t, err)
					}
					assert.Equal(t, 2, calls)
				})
			}
		})
	}
}
