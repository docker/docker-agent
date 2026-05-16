package openai

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestUserHeaders(t *testing.T) {
	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want map[string]string
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: nil,
		},
		{
			name: "nil provider_opts",
			cfg:  &latest.ModelConfig{},
			want: nil,
		},
		{
			name: "no http_headers key",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
			},
			want: nil,
		},
		{
			name: "valid headers",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"http_headers": map[string]any{
						"Copilot-Integration-Id": "vscode-chat",
						"X-Custom":               "value",
					},
				},
			},
			want: map[string]string{
				"Copilot-Integration-Id": "vscode-chat",
				"X-Custom":               "value",
			},
		},
		{
			name: "non-string header value is skipped",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"http_headers": map[string]any{
						"Good": "yes",
						"Bad":  42,
					},
				},
			},
			want: map[string]string{"Good": "yes"},
		},
		{
			name: "http_headers wrong type is ignored",
			cfg: &latest.ModelConfig{
				ProviderOpts: map[string]any{
					"http_headers": "not-a-map",
				},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, userHeaders(tt.cfg))
		})
	}
}

// TestBuildHeaderOptions verifies that the headers configured for a
// model actually reach the wire via a real HTTP server.
func TestBuildHeaderOptions(t *testing.T) {
	tests := []struct {
		name         string
		cfg          latest.ModelConfig
		wantHeaders  map[string]string // headers that MUST be present
		emptyHeaders []string          // headers that MUST be absent
	}{
		{
			name: "github-copilot injects default Copilot-Integration-Id",
			cfg: latest.ModelConfig{
				Provider: "github-copilot",
				Model:    "gpt-4o",
				TokenKey: "GITHUB_TOKEN",
			},
			wantHeaders: map[string]string{
				copilotIntegrationIDHeader: copilotIntegrationIDDefault,
			},
		},
		{
			name: "user override wins, even with different casing",
			cfg: latest.ModelConfig{
				Provider: "github-copilot",
				Model:    "gpt-4o",
				TokenKey: "GITHUB_TOKEN",
				ProviderOpts: map[string]any{
					"http_headers": map[string]any{
						"copilot-integration-id": "my-custom-integration",
					},
				},
			},
			wantHeaders: map[string]string{
				copilotIntegrationIDHeader: "my-custom-integration",
			},
		},
		{
			name: "custom headers are forwarded for non-copilot providers",
			cfg: latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				TokenKey: "OPENAI_API_KEY",
				ProviderOpts: map[string]any{
					"http_headers": map[string]any{
						"X-Custom-Header": "custom-value",
					},
				},
			},
			wantHeaders:  map[string]string{"X-Custom-Header": "custom-value"},
			emptyHeaders: []string{copilotIntegrationIDHeader},
		},
		{
			name: "non-copilot providers never get the default injected",
			cfg: latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				TokenKey: "OPENAI_API_KEY",
			},
			emptyHeaders: []string{copilotIntegrationIDHeader},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every test provides both env vars; NewClient only reads the one
			// referenced by cfg.TokenKey.
			envVars := map[string]string{
				"GITHUB_TOKEN":   "test-token",
				"OPENAI_API_KEY": "test-token",
			}
			got := captureHeaders(t, &tt.cfg, envVars)

			for name, want := range tt.wantHeaders {
				assert.Equal(t, want, got.Get(name), "header %q", name)
			}
			for _, name := range tt.emptyHeaders {
				assert.Empty(t, got.Get(name), "header %q must not be set", name)
			}
		})
	}
}

// captureHeaders boots a fake HTTP server, creates an OpenAI client
// pointed at it, triggers a request, and returns the headers the client
// sent on the wire.
func captureHeaders(t *testing.T, cfg *latest.ModelConfig, envVars map[string]string) http.Header {
	t.Helper()

	var (
		mu      sync.Mutex
		gotHdrs http.Header
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHdrs = r.Header.Clone()
		mu.Unlock()
		// Minimal SSE response so the client doesn't error out before we
		// have a chance to read the headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)

	cfg.BaseURL = server.URL
	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(envVars))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}},
		nil,
	)
	if err == nil && stream != nil {
		// Drain the stream so the HTTP request is actually sent.
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
		stream.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if gotHdrs == nil {
		return http.Header{}
	}
	return gotHdrs
}

func TestSanitizeHeaderValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean value unchanged",
			input: "vscode-chat",
			want:  "vscode-chat",
		},
		{
			name:  "strips CR",
			input: "value\rwith\rcarriage\rreturns",
			want:  "valuewithcarriagereturns",
		},
		{
			name:  "strips LF",
			input: "value\nwith\nline\nfeeds",
			want:  "valuewithlinefeeds",
		},
		{
			name:  "strips CRLF",
			input: "value\r\nwith\r\nCRLF",
			want:  "valuewithCRLF",
		},
		{
			name:  "prevents header injection",
			input: "value\r\nX-Injected: malicious\r\nAuthorization: Bearer stolen",
			want:  "valueX-Injected: maliciousAuthorization: Bearer stolen",
		},
		{
			name:  "trims whitespace",
			input: "  value with spaces  ",
			want:  "value with spaces",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only whitespace",
			input: "   \t\n\r  ",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeHeaderValue(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestBuildHeaderOptions_Sanitization verifies that header values are
// sanitized before being sent on the wire.
func TestBuildHeaderOptions_Sanitization(t *testing.T) {
	cfg := latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		TokenKey: "OPENAI_API_KEY",
		ProviderOpts: map[string]any{
			"http_headers": map[string]any{
				"X-Custom": "value\r\nX-Injected: malicious",
			},
		},
	}

	envVars := map[string]string{
		"OPENAI_API_KEY": "test-token",
	}
	got := captureHeaders(t, &cfg, envVars)

	// The sanitized value should have newlines stripped
	assert.Equal(t, "valueX-Injected: malicious", got.Get("X-Custom"))
	// The injected header should NOT exist as a separate header
	assert.Empty(t, got.Get("X-Injected"), "header injection should be prevented")
}

// TestBuildHeaderMap verifies that buildHeaderMap correctly merges
// provider defaults with user-configured headers.
func TestBuildHeaderMap(t *testing.T) {
	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want map[string]string
	}{
		{
			name: "nil config returns empty map",
			cfg:  nil,
			want: map[string]string{},
		},
		{
			name: "github-copilot gets default header",
			cfg: &latest.ModelConfig{
				Provider: "github-copilot",
			},
			want: map[string]string{
				copilotIntegrationIDHeader: copilotIntegrationIDDefault,
			},
		},
		{
			name: "user override wins case-insensitively",
			cfg: &latest.ModelConfig{
				Provider: "github-copilot",
				ProviderOpts: map[string]any{
					"http_headers": map[string]any{
						"copilot-integration-id": "custom-value",
					},
				},
			},
			want: map[string]string{
				copilotIntegrationIDHeader: "custom-value",
			},
		},
		{
			name: "non-copilot provider gets no default",
			cfg: &latest.ModelConfig{
				Provider: "openai",
			},
			want: map[string]string{},
		},
		{
			name: "custom headers are included",
			cfg: &latest.ModelConfig{
				Provider: "openai",
				ProviderOpts: map[string]any{
					"http_headers": map[string]any{
						"X-Custom-1": "value1",
						"X-Custom-2": "value2",
					},
				},
			},
			want: map[string]string{
				"X-Custom-1": "value1",
				"X-Custom-2": "value2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildHeaderMap(tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}
