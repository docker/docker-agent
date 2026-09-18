//go:build js && wasm && docker_agent_wasm_cloud

package main

import (
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

func TestCloudProviderRegistry(t *testing.T) {
	for _, name := range []string{"amazon-bedrock", "google", "dmr"} {
		assert.True(t, demoProviders.Has(name), name)
	}
}

// Cloud credentials come from the session env only, and a session that has
// none is rejected when it is created, before any request.
func TestCloudSessionsUseSessionEnvOnly(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "bedrock without token",
			model:   "provider: amazon-bedrock\n    model: anthropic.claude-sonnet-4-6",
			wantErr: "requires a bearer token",
		},
		{
			name:  "bedrock with token",
			model: "provider: amazon-bedrock\n    model: anthropic.claude-sonnet-4-6",
			env:   map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "key"},
		},
		{
			name:  "bedrock with token_key",
			model: "provider: amazon-bedrock\n    model: anthropic.claude-sonnet-4-6\n    token_key: BEDROCK_KEY",
			env:   map[string]string{"BEDROCK_KEY": "key"},
		},
		{
			name:    "bedrock rejects profile",
			model:   "provider: amazon-bedrock\n    model: anthropic.claude-sonnet-4-6\n    provider_opts:\n      profile: dev",
			env:     map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "key"},
			wantErr: "provider_opts.profile is not supported in the browser",
		},
		{
			name:    "vertex gemini without token",
			model:   "provider: google\n    model: gemini-2.5-flash\n    provider_opts:\n      project: my-project\n      location: us-central1",
			wantErr: "requires an OAuth access token in the session env (GOOGLE_OAUTH_ACCESS_TOKEN)",
		},
		{
			name:  "vertex gemini with token",
			model: "provider: google\n    model: gemini-2.5-flash\n    provider_opts:\n      project: my-project\n      location: us-central1",
			env:   map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "tok"},
		},
		{
			name:    "vertex gemini token_key is required",
			model:   "provider: google\n    model: gemini-2.5-flash\n    token_key: MY_TOKEN\n    provider_opts:\n      project: my-project\n      location: us-central1",
			env:     map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "ignored"},
			wantErr: "MY_TOKEN",
		},
		{
			name:  "vertex gemini with token_key",
			model: "provider: google\n    model: gemini-2.5-flash\n    token_key: MY_TOKEN\n    provider_opts:\n      project: my-project\n      location: us-central1",
			env:   map[string]string{"MY_TOKEN": "tok"},
		},
		{
			name:    "vertex gemini via GOOGLE_GENAI_USE_VERTEXAI",
			model:   "provider: google\n    model: gemini-2.5-flash",
			env:     map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1", "GOOGLE_CLOUD_PROJECT": "my-project", "GOOGLE_CLOUD_LOCATION": "us-central1"},
			wantErr: "requires an OAuth access token",
		},
		{
			name:  "model garden anthropic with token",
			model: "provider: google\n    model: claude-sonnet-4-6\n    provider_opts:\n      project: my-project\n      location: us-east5\n      publisher: anthropic",
			env:   map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "tok"},
		},
		{
			name:    "model garden meta without token",
			model:   "provider: google\n    model: meta/llama-4-maverick-17b-128e-instruct-maas\n    provider_opts:\n      project: my-project\n      location: us-central1\n      publisher: meta",
			wantErr: "requires an OAuth access token",
		},
		{
			name:  "gemini API still takes an API key",
			model: "provider: google\n    model: gemini-2.5-flash",
			env:   map[string]string{"GOOGLE_API_KEY": "key"},
		},
		{
			name:    "dmr without base_url",
			model:   "provider: dmr\n    model: ai/qwen3",
			wantErr: "dmr in the browser requires an explicit http(s) base_url",
		},
		{
			name:    "dmr with unix base_url",
			model:   "provider: dmr\n    model: ai/qwen3\n    base_url: unix:///var/run/docker.sock",
			wantErr: "dmr in the browser requires an explicit http(s) base_url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := "models:\n  primary:\n    " + tt.model + "\nagents:\n  root:\n    model: primary\n"
			s, err := browserHost.openSession(t.Context(), sessionOptions{YAML: yaml, Env: tt.env})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.NoError(t, s.close())
		})
	}
}

// cannedTransport records requests and answers each with body.
type cannedTransport struct {
	status int
	body   string
	got    []*http.Request
}

func (c *cannedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = append(c.got, req)
	return &http.Response{
		StatusCode: c.status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    req,
	}, nil
}

// The session env token reaches every request through the registry as the
// only credential: no SigV4 signature, no API key header.
func TestCloudTokensReachRequests(t *testing.T) {
	tests := []struct {
		name     string
		cfg      latest.ModelConfig
		env      map[string]string
		status   int
		body     string
		wantHost string
		wantPath string
	}{
		{
			name:     "bedrock",
			cfg:      latest.ModelConfig{Provider: "amazon-bedrock", Model: "anthropic.claude-sonnet-4-20250514-v1:0", ProviderOpts: map[string]any{"region": "eu-west-1"}},
			env:      map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "session-token"},
			status:   http.StatusForbidden,
			body:     `{"message":"denied"}`,
			wantHost: "bedrock-runtime.eu-west-1.amazonaws.com",
			wantPath: "/model/anthropic.claude-sonnet-4-20250514-v1:0/converse-stream",
		},
		{
			name:     "vertex gemini",
			cfg:      latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: map[string]any{"project": "my-project", "location": "us-central1"}},
			env:      map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "session-token"},
			status:   http.StatusOK,
			body:     "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}\n\n",
			wantHost: "us-central1-aiplatform.googleapis.com",
			wantPath: "/v1beta1/projects/my-project/locations/us-central1/publishers/google/models/gemini-2.5-flash:streamGenerateContent",
		},
		{
			name:     "model garden anthropic",
			cfg:      latest.ModelConfig{Provider: "google", Model: "claude-sonnet-4-6", TokenKey: "GCP_TOKEN", ProviderOpts: map[string]any{"project": "my-project", "location": "us-east5", "publisher": "anthropic"}},
			env:      map[string]string{"GCP_TOKEN": "session-token"},
			status:   http.StatusOK,
			body:     "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantHost: "us-east5-aiplatform.googleapis.com",
			wantPath: "/v1/projects/my-project/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-6:streamRawPredict",
		},
		{
			name:     "model garden meta",
			cfg:      latest.ModelConfig{Provider: "google", Model: "meta/llama-4-maverick-17b-128e-instruct-maas", ProviderOpts: map[string]any{"project": "my-project", "location": "us-central1", "publisher": "meta"}},
			env:      map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "session-token"},
			status:   http.StatusOK,
			body:     "data: [DONE]\n\n",
			wantHost: "us-central1-aiplatform.googleapis.com",
			wantPath: "/v1beta1/projects/my-project/locations/us-central1/endpoints/openapi/chat/completions",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &cannedTransport{status: tt.status, body: tt.body}
			p, err := demoProviders.New(t.Context(), &tt.cfg, environment.NewMapEnvProvider(tt.env),
				options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper { return transport }))
			require.NoError(t, err)

			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
			if err == nil {
				for {
					if _, err := stream.Recv(); err != nil {
						break
					}
				}
				stream.Close()
			}

			require.Len(t, transport.got, 1)
			req := transport.got[0]
			assert.Equal(t, tt.wantHost, req.URL.Host)
			assert.Equal(t, tt.wantPath, req.URL.Path)
			assert.Equal(t, "Bearer session-token", req.Header.Get("Authorization"))
			assert.Empty(t, req.Header.Get("X-Api-Key"))
			assert.Empty(t, req.Header.Get("X-Amz-Date"))
		})
	}
}
