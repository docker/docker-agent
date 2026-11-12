package gemini

import (
	"encoding/json"
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
	"github.com/docker/docker-agent/pkg/testutil/geminiintegration"
)

func TestAudioInlineDataWire(t *testing.T) {
	geminiintegration.Require(t)
	t.Parallel()
	for _, mime := range []string{"audio/wav", "audio/mpeg", "audio/ogg", "audio/pcm; rate=24000"} {
		t.Run(mime, func(t *testing.T) {
			t.Parallel()
			captured := make(chan []*genai.Content, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Contents []*genai.Content `json:"contents"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				captured <- body.Contents
				writeGeminiSSEResponse(w)
			}))
			defer server.Close()
			c, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "google", Model: "gemini-audio-test", BaseURL: server.URL, Capabilities: &latest.CapabilitiesConfig{Audio: true}}, environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test"}))
			require.NoError(t, err)
			data := []byte{0, 1, 255, 254}
			doc := &chat.Document{Name: "audio", MimeType: mime, Source: chat.DocumentSource{InlineData: data}}
			messages := []chat.Message{{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "before"}, {Type: chat.MessagePartTypeDocument, Document: doc}, {Type: chat.MessagePartTypeText, Text: "audio note"}, {Type: chat.MessagePartTypeDocument, Document: doc}}}}
			stream, err := c.CreateChatCompletionStream(t.Context(), messages, nil)
			require.NoError(t, err)
			for {
				if _, err := stream.Recv(); err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
			}
			stream.Close()
			contents := <-captured
			require.Len(t, contents, 1)
			parts := contents[0].Parts
			require.Len(t, parts, 4)
			assert.Equal(t, "before", parts[0].Text)
			assert.Equal(t, "audio note", parts[2].Text)
			for _, i := range []int{1, 3} {
				require.NotNil(t, parts[i].InlineData)
				assert.Equal(t, mime, parts[i].InlineData.MIMEType)
				assert.Equal(t, data, parts[i].InlineData.Data)
			}
		})
	}
}
