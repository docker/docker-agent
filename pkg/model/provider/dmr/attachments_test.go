package dmr

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// minPNG is a minimal PNG magic-byte header for use in tests.
var minPNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// countParts counts content parts across all converted user messages for which
// pred returns true.
func countParts(msgs []openai.ChatCompletionMessageParamUnion, pred func(openai.ChatCompletionContentPartUnionParam) bool) int {
	n := 0
	for _, m := range msgs {
		if m.OfUser == nil {
			continue
		}
		for _, p := range m.OfUser.Content.OfArrayOfContentParts {
			if pred(p) {
				n++
			}
		}
	}
	return n
}

func countImageParts(msgs []openai.ChatCompletionMessageParamUnion) int {
	return countParts(msgs, func(p openai.ChatCompletionContentPartUnionParam) bool { return p.OfImageURL != nil })
}

func countFileParts(msgs []openai.ChatCompletionMessageParamUnion) int {
	return countParts(msgs, func(p openai.ChatCompletionContentPartUnionParam) bool { return p.OfFile != nil })
}

// docMessage returns a single user message carrying one inline document attachment.
func docMessage(name, mime string, data []byte) []chat.Message {
	return []chat.Message{{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "Describe the attachment."},
			{
				Type: chat.MessagePartTypeDocument,
				Document: &chat.Document{
					Name:     name,
					MimeType: mime,
					Source:   chat.DocumentSource{InlineData: data},
				},
			},
		},
	}}
}

// TestDMRConvertMessagesRespectsDeclaredCaps is the regression test for issue
// #2739: DMR-hosted models are absent from models.dev, so a store lookup always
// missed and image/PDF attachments were silently dropped. Capabilities are now
// declared via provider_opts and injected explicitly, so a declared capability
// forwards the attachment while the conservative default still drops it.
func TestDMRConvertMessagesRespectsDeclaredCaps(t *testing.T) {
	t.Parallel()

	t.Run("image dropped by default (no caps declared)", func(t *testing.T) {
		t.Parallel()
		c := &Client{} // zero-value caps == text-only
		msgs := c.convertMessages(t.Context(), docMessage("photo.png", "image/png", minPNG))
		assert.Equal(t, 0, countImageParts(msgs), "image must be dropped when no capability is declared")
	})

	t.Run("image forwarded when supports_images declared", func(t *testing.T) {
		t.Parallel()
		c := &Client{attachmentCaps: modelinfo.CapsWith(true, false, false, false)}
		msgs := c.convertMessages(t.Context(), docMessage("photo.png", "image/png", minPNG))
		assert.Equal(t, 1, countImageParts(msgs), "image must be forwarded when supports_images is declared")
	})

	t.Run("pdf dropped by default (no caps declared)", func(t *testing.T) {
		t.Parallel()
		c := &Client{}
		msgs := c.convertMessages(t.Context(), docMessage("spec.pdf", "application/pdf", []byte("%PDF-1.4")))
		assert.Equal(t, 0, countFileParts(msgs), "pdf must be dropped when no capability is declared")
	})

	t.Run("pdf forwarded as file part when supports_pdf declared", func(t *testing.T) {
		t.Parallel()
		c := &Client{attachmentCaps: modelinfo.CapsWith(false, true, false, false)}
		msgs := c.convertMessages(t.Context(), docMessage("spec.pdf", "application/pdf", []byte("%PDF-1.4")))
		assert.Equal(t, 1, countFileParts(msgs), "pdf must be forwarded as a file part when supports_pdf is declared")
	})
}

// TestParseDMRProviderOptsAttachmentCaps verifies that supports_images /
// supports_pdf are parsed (accepting bool and string forms) and that invalid
// values are rejected.
func TestParseDMRProviderOptsAttachmentCaps(t *testing.T) {
	t.Parallel()

	t.Run("unset defaults to text-only", func(t *testing.T) {
		t.Parallel()
		res, err := parseDMRProviderOpts("llama.cpp", &latest.ModelConfig{})
		require.NoError(t, err)
		assert.False(t, res.supportsImages)
		assert.False(t, res.supportsPDF)
	})

	t.Run("bool values", func(t *testing.T) {
		t.Parallel()
		res, err := parseDMRProviderOpts("llama.cpp", &latest.ModelConfig{
			ProviderOpts: map[string]any{"supports_images": true, "supports_pdf": false},
		})
		require.NoError(t, err)
		assert.True(t, res.supportsImages)
		assert.False(t, res.supportsPDF)
	})

	t.Run("string values parse", func(t *testing.T) {
		t.Parallel()
		res, err := parseDMRProviderOpts("llama.cpp", &latest.ModelConfig{
			ProviderOpts: map[string]any{"supports_images": "true", "supports_pdf": "1"},
		})
		require.NoError(t, err)
		assert.True(t, res.supportsImages)
		assert.True(t, res.supportsPDF)
	})

	t.Run("invalid string rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseDMRProviderOpts("llama.cpp", &latest.ModelConfig{
			ProviderOpts: map[string]any{"supports_images": "yes-please"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "supports_images")
	})

	t.Run("invalid type rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseDMRProviderOpts("llama.cpp", &latest.ModelConfig{
			ProviderOpts: map[string]any{"supports_pdf": 3},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "supports_pdf")
	})
}

// TestDMRVisionAttachmentForwardedEndToEnd exercises the full client path:
// provider_opts -> attachmentCaps -> request body. The serialized chat
// completion request must carry the image as an image_url content part.
func TestDMRVisionAttachmentForwardedEndToEnd(t *testing.T) {
	t.Parallel()

	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			captured = body
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider:     "dmr",
		Model:        "ai/qwen2.5-vl",
		BaseURL:      server.URL + "/engines/v1/",
		ProviderOpts: map[string]any{"supports_images": true},
	}
	client, err := NewClient(t.Context(), cfg)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), docMessage("photo.png", "image/png", minPNG), nil)
	require.NoError(t, err)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	stream.Close()

	require.NotEmpty(t, captured, "chat/completions should have been called")

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(captured, &req))

	imageParts := 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				imageParts++
			}
		}
	}
	assert.Equal(t, 1, imageParts, "request body must carry the image as an image_url content part")
}
