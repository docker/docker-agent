package gemini

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestCreateChatCompletionStream_Keepalive(t *testing.T) {
	t.Parallel()

	for _, gateway := range []bool{false, true} {
		t.Run(fmt.Sprintf("gateway=%t", gateway), func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{
					"event: keepalive\ndata: {}\n\n",
					"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}],\"role\":\"model\"}}]}\n\n",
					"event: keepalive\ndata: {}\n\n",
					"event: keepalive\ndata: {}\n\n",
					"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}\n\n",
					"event: keepalive\ndata: {}\n\n",
				} {
					_, _ = io.WriteString(w, event)
					w.(http.Flusher).Flush()
				}
			}))
			t.Cleanup(server.Close)

			cfg := &latest.ModelConfig{
				Provider: "google",
				Model:    "gemini-3.8-flash",
				BaseURL:  server.URL,
			}
			env := environment.NewMapEnvProvider(map[string]string{
				"GOOGLE_API_KEY":                  "test-key",
				environment.DockerDesktopTokenEnv: "test-dd-token",
			})
			var opts []options.Opt
			if gateway {
				cfg.BaseURL = ""
				opts = append(opts, options.WithGateway(server.URL))
			}
			client, err := NewClient(t.Context(), cfg, env, opts...)
			require.NoError(t, err)

			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
				{Role: chat.MessageRoleUser, Content: "hello"},
			}, nil)
			require.NoError(t, err)
			t.Cleanup(stream.Close)

			if !gateway {
				_, err := stream.Recv()
				require.ErrorContains(t, err, "invalid stream chunk: event: keepalive")
				return
			}

			for _, text := range []string{"hello", " world"} {
				response, err := stream.Recv()
				require.NoError(t, err)
				require.Len(t, response.Choices, 1)
				assert.Equal(t, text, response.Choices[0].Delta.Content)
				assert.Empty(t, response.Choices[0].FinishReason)
			}

			response, err := stream.Recv()
			require.NoError(t, err)
			require.Len(t, response.Choices, 1)
			assert.Equal(t, chat.FinishReasonStop, response.Choices[0].FinishReason)
			require.NotNil(t, response.Usage)
			assert.Equal(t, int64(2), response.Usage.InputTokens)
			assert.Equal(t, int64(3), response.Usage.OutputTokens)

			_, err = stream.Recv()
			require.ErrorIs(t, err, io.EOF)
		})
	}
}
