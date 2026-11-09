package dmr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestNewClientAppliesHTTPTransportWrapperInDirectMode(t *testing.T) {
	t.Parallel()

	var wrappedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "dmr",
		Model:    "ai/qwen3",
		BaseURL:  server.URL + "/engines/v1/",
	}
	client, err := NewClient(t.Context(), cfg, options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
		return dmrRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			wrappedRequests.Add(1)
			return base.RoundTrip(req)
		})
	}))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.GreaterOrEqual(t, wrappedRequests.Load(), int32(1), "direct-mode DMR requests must pass through the registered transport wrapper")
}

type dmrRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f dmrRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
