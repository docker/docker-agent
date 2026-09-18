package dmr

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestContextWindow(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, configs, metadata string
		opts                    map[string]any
		status                  int
		want                    int64
		wantErr, wantMetadata   bool
	}{
		{name: "configured window overrides metadata", configs: `[{"Backend":"llama.cpp","Mode":"completion","Config":{"context-size":8192}}]`, want: 8192},
		{name: "embedding config ignored", configs: `[{"Mode":"embedding","Config":{"context-size":512}}]`, metadata: `{"dmr":{"context_window":32768}}`, want: 32768, wantMetadata: true},
		{name: "no runtime config", configs: `[]`, metadata: `{"dmr":{"context_window":32768}}`, want: 32768, wantMetadata: true},
		{name: "zero falls back to metadata", configs: `[{"Mode":"completion","Config":{"context-size":0}}]`, metadata: `{"dmr":{"context_window":32768}}`, want: 32768, wantMetadata: true},
		{name: "legacy metadata", configs: `[]`, metadata: `{"id":"ai/qwen3"}`, wantMetadata: true},
		{name: "negative metadata", configs: `[]`, metadata: `{"dmr":{"context_window":-1}}`, wantMetadata: true},
		{name: "unlimited config is unknown", configs: `[{"Mode":"completion","Config":{"context-size":-1}}]`},
		{name: "multiple backends use smaller allocation", configs: `[{"Backend":"llama.cpp","Mode":"completion","Config":{"context-size":8192}},{"Backend":"vllm","Mode":"completion","Config":{"context-size":16384}}]`, want: 8192},
		{name: "runtime context flag is unknown", configs: `[{"Mode":"completion","Config":{"context-size":32768,"runtime-flags":["--ctx-size=4096"]}}]`},
		{name: "structured hf overrides are unknown", configs: `[{"Mode":"completion","Config":{"vllm":{"hf-overrides":{"max_position_embeddings":4096}}}}]`},
		{name: "one backend uses packaged window", configs: `[{"Backend":"llama.cpp","Mode":"completion","Config":{}},{"Backend":"vllm","Mode":"completion","Config":{"context-size":65536}}]`, metadata: `{"dmr":{"context_window":32768}}`, want: 32768, wantMetadata: true},
		{name: "unrelated flags allow discovery", configs: `[{"Mode":"completion","Config":{"context-size":8192,"runtime-flags":["--threads","8"]}}]`, opts: map[string]any{"runtime_flags": []string{"--threads", "8"}}, want: 8192},
		{name: "unsupported management API", status: http.StatusNotFound, wantErr: true},
		{name: "server error", status: http.StatusServiceUnavailable, wantErr: true},
		{name: "malformed config", configs: `not json`, wantErr: true},
		{name: "malformed metadata", configs: `[]`, metadata: `not json`, wantErr: true, wantMetadata: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			metadataCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				switch r.URL.Path {
				case "/engines/_configure":
					assert.Equal(t, "ai/qwen3", r.URL.Query().Get("model"))
					if tt.status != 0 {
						w.WriteHeader(tt.status)
						return
					}
					_, _ = w.Write([]byte(tt.configs))
				case "/engines/v1/models/ai/qwen3":
					metadataCalls++
					_, _ = w.Write([]byte(tt.metadata))
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := &Client{Config: base.Config{BaseURL: server.URL + "/engines/v1", ModelConfig: latest.ModelConfig{Model: "ai/qwen3", ProviderOpts: tt.opts}}, httpClient: server.Client()}
			limit, err := client.ContextWindow(t.Context())
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, limit)
			assert.Equal(t, tt.wantMetadata, metadataCalls > 0)
		})
	}
}

func TestContextWindowSkipsUnsafeDiscovery(t *testing.T) {
	t.Parallel()
	for _, cfg := range []base.Config{
		{ModelOptions: options.Apply(options.WithGateway("http://gateway"))},
		{ModelConfig: latest.ModelConfig{ProviderOpts: map[string]any{"runtime_flags": []string{"-c", "4096"}}}},
		{ModelConfig: latest.ModelConfig{ProviderOpts: map[string]any{"raw_runtime_flags": "--ctx-size 4096"}}},
	} {
		client := &Client{Config: cfg}
		limit, err := client.ContextWindow(t.Context())
		require.NoError(t, err)
		assert.Zero(t, limit)
	}
}

func TestModelConfigURL(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ base, want, backend string }{
		{"http://host/engines/v1/", "http://host/engines/_configure", ""},
		{"http://_/exp/vDD4.40/engines/v1", "http://_/exp/vDD4.40/engines/_configure", ""},
		{"https://remote/engines/vllm/v1?x=1", "https://remote/engines/_configure?x=1", "vllm"},
	} {
		u, backend, err := modelConfigURL(tt.base)
		require.NoError(t, err)
		assert.Equal(t, tt.want, u.String())
		assert.Equal(t, tt.backend, backend)
	}
}

func TestContextWindowUsesResolvedTransport(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/exp/vDD4.40/engines/_configure", r.URL.Path)
		_, _ = w.Write([]byte(`[{"Backend":"llama.cpp","Mode":"completion","Config":{"context-size":2048}},{"Backend":"vllm","Mode":"completion","Config":{"context-size":8192}}]`))
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	client := &Client{Config: base.Config{BaseURL: "http://_/exp/vDD4.40/engines/vllm/v1", ModelConfig: latest.ModelConfig{Model: "ai/qwen3"}}, httpClient: &http.Client{Transport: transport}}
	limit, err := client.ContextWindow(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(8192), limit)
}
