package server

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
)

// TestAPIMessageWithImageStructure tests that the API message type can hold image attachments
func TestAPIMessageWithImageStructure(t *testing.T) {
	imageData := createTestImageDataURL()
	messages := []api.Message{
		{
			Role: chat.MessageRoleUser,
			MultiContent: []chat.MessagePart{
				{
					Type: chat.MessagePartTypeText,
					Text: "What's in this image?",
				},
				{
					Type: chat.MessagePartTypeImageURL,
					ImageURL: &chat.MessageImageURL{
						URL:    imageData,
						Detail: chat.ImageURLDetailAuto,
					},
				},
			},
		},
	}

	// Marshal to JSON to ensure the structure is correct
	jsonData, err := json.Marshal(messages)
	require.NoError(t, err)

	// Unmarshal back to verify round-trip
	var decoded []api.Message
	err = json.Unmarshal(jsonData, &decoded)
	require.NoError(t, err)

	assert.Len(t, decoded, 1)
	assert.Equal(t, chat.MessageRoleUser, decoded[0].Role)
	assert.Empty(t, decoded[0].Content) // Content should be empty when using MultiContent
	assert.Len(t, decoded[0].MultiContent, 2)

	assert.Equal(t, chat.MessagePartTypeText, decoded[0].MultiContent[0].Type)
	assert.Equal(t, "What's in this image?", decoded[0].MultiContent[0].Text)

	assert.Equal(t, chat.MessagePartTypeImageURL, decoded[0].MultiContent[1].Type)
	require.NotNil(t, decoded[0].MultiContent[1].ImageURL)
	assert.Equal(t, imageData, decoded[0].MultiContent[1].ImageURL.URL)
	assert.Equal(t, chat.ImageURLDetailAuto, decoded[0].MultiContent[1].ImageURL.Detail)
}

// TestAPIMessageWithTextOnly tests backwards compatibility with plain text messages
func TestAPIMessageWithTextOnly(t *testing.T) {
	messages := []api.Message{
		{
			Role:    chat.MessageRoleUser,
			Content: "Tell me a joke",
		},
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(messages)
	require.NoError(t, err)

	// Unmarshal back
	var decoded []api.Message
	err = json.Unmarshal(jsonData, &decoded)
	require.NoError(t, err)

	assert.Len(t, decoded, 1)
	assert.Equal(t, chat.MessageRoleUser, decoded[0].Role)
	assert.Equal(t, "Tell me a joke", decoded[0].Content)
	assert.Empty(t, decoded[0].MultiContent) // MultiContent should be empty for plain text
}

// TestAPIMessageMixedContentTypes tests a message with both content and multi_content
func TestAPIMessageMixedContentTypes(t *testing.T) {
	// This tests what happens if both are set - MultiContent should take precedence
	imageData := createTestImageDataURL()
	msg := api.Message{
		Role:    chat.MessageRoleUser,
		Content: "This should be ignored",
		MultiContent: []chat.MessagePart{
			{
				Type: chat.MessagePartTypeText,
				Text: "What's in this image?",
			},
			{
				Type: chat.MessagePartTypeImageURL,
				ImageURL: &chat.MessageImageURL{
					URL:    imageData,
					Detail: chat.ImageURLDetailAuto,
				},
			},
		},
	}

	// Marshal and unmarshal
	jsonData, err := json.Marshal(msg)
	require.NoError(t, err)

	var decoded api.Message
	err = json.Unmarshal(jsonData, &decoded)
	require.NoError(t, err)

	// Both fields should be preserved in JSON
	assert.Equal(t, "This should be ignored", decoded.Content)
	assert.Len(t, decoded.MultiContent, 2)
}

// Helper function to create a test image data URL (1x1 red pixel PNG)
func createTestImageDataURL() string {
	// A minimal 1x1 red pixel PNG
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xDD, 0x8D,
		0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82,
	}

	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	return "data:image/png;base64," + encoded
}
