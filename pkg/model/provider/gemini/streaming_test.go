package gemini

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

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

func TestCreateChatCompletionStream_ToolCallReplay(t *testing.T) {
	t.Parallel()

	for _, gateway := range []bool{false, true} {
		for _, nativeIDs := range []bool{false, true} {
			t.Run(fmt.Sprintf("gateway=%t/nativeIDs=%t", gateway, nativeIDs), func(t *testing.T) {
				t.Parallel()

				signature := []byte("tool-call-signature")
				calls := []*genai.Part{
					genai.NewPartFromFunctionCall("lookup", map[string]any{"city": "Paris"}),
					genai.NewPartFromFunctionCall("lookup", map[string]any{"city": "London"}),
				}
				calls[0].ThoughtSignature = signature
				if nativeIDs {
					calls[0].FunctionCall.ID = "provider-1"
					calls[1].FunctionCall.ID = "provider-2"
				}
				requests := make(chan []*genai.Content, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Contents []*genai.Content `json:"contents"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					requests <- request.Contents
					parts := []*genai.Part{genai.NewPartFromText("done")}
					if len(request.Contents) == 1 {
						parts = append([]*genai.Part{genai.NewPartFromText("Looking up both cities.")}, calls...)
					}
					response := &genai.GenerateContentResponse{
						Candidates: []*genai.Candidate{{Content: genai.NewContentFromParts(parts, genai.RoleModel)}},
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: ")
					_ = json.NewEncoder(w).Encode(response)
					_, _ = io.WriteString(w, "\n")
				}))
				t.Cleanup(server.Close)

				cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-3.8-flash", BaseURL: server.URL}
				var opts []options.Opt
				if gateway {
					cfg.BaseURL = ""
					opts = append(opts, options.WithGateway(server.URL))
				}
				client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{
					"GOOGLE_API_KEY":                  "test-key",
					environment.DockerDesktopTokenEnv: "test-token",
				}), opts...)
				require.NoError(t, err)

				messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "Look up Paris and London"}}
				stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
				require.NoError(t, err)
				t.Cleanup(stream.Close)
				response, err := stream.Recv()
				require.NoError(t, err)
				delta := response.Choices[0].Delta
				require.Len(t, delta.ToolCalls, 2)
				for i, call := range delta.ToolCalls {
					assert.Equal(t, calls[i].FunctionCall.ID, call.ProviderID)
					assert.NotEmpty(t, call.ID)
				}
				_, err = stream.Recv()
				require.NoError(t, err)
				_, err = stream.Recv()
				require.ErrorIs(t, err, io.EOF)
				<-requests

				messages = append(messages, chat.Message{
					Role: chat.MessageRoleAssistant, Content: delta.Content,
					ToolCalls: delta.ToolCalls, ThoughtSignature: delta.ThoughtSignature,
				}, chat.Message{
					Role: chat.MessageRoleTool, ToolCallID: delta.ToolCalls[1].ID, Content: "London result",
				}, chat.Message{
					Role: chat.MessageRoleTool, ToolCallID: delta.ToolCalls[0].ID, Content: "Paris result",
				})
				// Replay after a session JSON round trip, including out-of-order tool results.
				data, err := json.Marshal(messages)
				require.NoError(t, err)
				messages = nil
				require.NoError(t, json.Unmarshal(data, &messages))

				stream, err = client.CreateChatCompletionStream(t.Context(), messages, nil)
				require.NoError(t, err)
				t.Cleanup(stream.Close)
				_, err = stream.Recv()
				require.NoError(t, err)
				replayed := <-requests
				require.Len(t, replayed, 3)
				assert.Equal(t, genai.RoleModel, replayed[1].Role)
				assert.Equal(t, append([]*genai.Part{genai.NewPartFromText(delta.Content)}, calls...), replayed[1].Parts)
				assert.Equal(t, genai.RoleUser, replayed[2].Role)
				require.Len(t, replayed[2].Parts, 2)
				for i, result := range []string{"Paris result", "London result"} {
					assert.Equal(t, &genai.FunctionResponse{
						ID: calls[i].FunctionCall.ID, Name: "lookup", Response: map[string]any{"result": result},
					}, replayed[2].Parts[i].FunctionResponse)
				}
			})
		}
	}
}
