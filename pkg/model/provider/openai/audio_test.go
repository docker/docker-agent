package openai

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestAudioChatCompletionWire(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mime, format string
		supported          bool
	}{
		{"wav", "audio/wav", "wav", true},
		{"mp3", "audio/mpeg", "mp3", true},
		{"parameters", "audio/x-wav;rate=24000", "wav", true},
		{"unsupported model", "audio/wav", "", false},
		{"unsupported format", "audio/ogg", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			captured := make(chan json.RawMessage, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/chat/completions", r.URL.Path)
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				captured <- body
				writeSSEResponse(w)
			}))
			defer server.Close()
			c, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "audio-test", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}, Capabilities: &latest.CapabilitiesConfig{Audio: tc.supported}}, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}))
			require.NoError(t, err)
			data := []byte{0, 1, 2, 255}
			messages := []chat.Message{{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "before"},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "audio", MimeType: tc.mime, Source: chat.DocumentSource{InlineData: data}}},
				{Type: chat.MessagePartTypeText, Text: "audio note"},
			}}}
			stream, err := c.CreateChatCompletionStream(t.Context(), messages, nil)
			require.NoError(t, err)
			for {
				if _, err := stream.Recv(); err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
			}
			stream.Close()
			body := <-captured
			var wire struct {
				Messages []struct {
					Content []struct {
						Type  string                        `json:"type"`
						Text  string                        `json:"text"`
						Audio struct{ Data, Format string } `json:"input_audio"`
					} `json:"content"`
				} `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(body, &wire))
			require.Len(t, wire.Messages, 1)
			parts := wire.Messages[0].Content
			if tc.format == "" {
				require.Len(t, parts, 2)
				assert.NotContains(t, string(body), base64.StdEncoding.EncodeToString(data))
			} else {
				require.Len(t, parts, 3)
				assert.Equal(t, "input_audio", parts[1].Type)
				assert.Equal(t, tc.format, parts[1].Audio.Format)
				assert.Equal(t, base64.StdEncoding.EncodeToString(data), parts[1].Audio.Data)
			}
			assert.Equal(t, "before", parts[0].Text)
			assert.Equal(t, "audio note", parts[len(parts)-1].Text)
		})
	}
}
