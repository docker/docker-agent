package gemini

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestStreamAdapter_CloseBeforeRecv(t *testing.T) {
	t.Parallel()
	called := false
	adapter := NewStreamAdapter(func(func(*genai.GenerateContentResponse, error) bool) {
		called = true
	}, "test-model", true)

	adapter.Close()
	_, err := adapter.Recv()

	require.ErrorIs(t, err, io.EOF)
	require.False(t, called, "Recv after Close must not start the upstream iterator")
}

func TestStreamAdapter_GeminiUsageMetadata(t *testing.T) {
	t.Parallel()
	// Gemini 3 (and any future model that emits usage metadata on its own chunk
	// without accompanying text/tool calls) was previously losing token counts
	// because the stream adapter dropped chunks that lacked text/function calls.
	// These tests pin the fixed behaviour.

	t.Run("forwards chunks containing only UsageMetadata", func(t *testing.T) {
		textChunk := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{{Text: "Hello"}},
					},
				},
			},
		}
		usageOnlyChunk := &genai.GenerateContentResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     14,
				CandidatesTokenCount: 5,
				ThoughtsTokenCount:   3,
			},
		}

		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			if !fn(textChunk, nil) {
				return
			}
			fn(usageOnlyChunk, nil)
		}

		adapter := NewStreamAdapter(iter, "test-model", true)

		// First Recv: text chunk, no usage.
		r1, err := adapter.Recv()
		require.NoError(t, err)
		require.Equal(t, "Hello", r1.Choices[0].Delta.Content)
		require.Nil(t, r1.Usage)

		// Second Recv: the usage-only chunk must be forwarded (regression guard).
		r2, err := adapter.Recv()
		require.NoError(t, err)
		require.NotNil(t, r2.Usage, "usage-only chunk must surface token counts")
		require.Equal(t, int64(14), r2.Usage.InputTokens)
		require.Equal(t, int64(8), r2.Usage.OutputTokens) // candidates + thoughts
		require.Equal(t, int64(3), r2.Usage.ReasoningTokens)

		// Final done event closes the stream.
		rdone, err := adapter.Recv()
		require.NoError(t, err)
		require.Equal(t, chat.FinishReasonStop, rdone.Choices[0].FinishReason)
	})

	t.Run("done event carries usage from last response", func(t *testing.T) {
		// When the upstream stream ends with a chunk that carries usage,
		// the synthesised "done" event must propagate that usage so downstream
		// observers receive the final tally.
		chunk := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{{Text: "ok"}},
					},
				},
			},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 2,
			},
		}

		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			fn(chunk, nil)
		}

		adapter := NewStreamAdapter(iter, "test-model", true)

		// Forwarded chunk carries usage.
		r1, err := adapter.Recv()
		require.NoError(t, err)
		require.NotNil(t, r1.Usage)
		require.Equal(t, int64(10), r1.Usage.InputTokens)

		// Done event also exposes the usage from the last response.
		rdone, err := adapter.Recv()
		require.NoError(t, err)
		require.Equal(t, chat.FinishReasonStop, rdone.Choices[0].FinishReason)
		require.NotNil(t, rdone.Usage, "done event should expose usage from last response")
		require.Equal(t, int64(10), rdone.Usage.InputTokens)
		require.Equal(t, int64(2), rdone.Usage.OutputTokens)
	})

	t.Run("trackUsage=false suppresses usage extraction", func(t *testing.T) {
		// When trackUsage is disabled the adapter must not populate Usage even
		// if upstream returns UsageMetadata.
		chunk := &genai.GenerateContentResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 2,
			},
		}
		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			fn(chunk, nil)
		}
		adapter := NewStreamAdapter(iter, "test-model", false)

		r1, err := adapter.Recv()
		require.NoError(t, err)
		require.Nil(t, r1.Usage)
	})
}

func TestStreamAdapter_FunctionCalls(t *testing.T) {
	t.Parallel()
	t.Run("function calls in final message", func(t *testing.T) {
		mockResp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{
								FunctionCall: &genai.FunctionCall{
									Name: "test_function",
									Args: map[string]any{"param": "value"},
								},
							},
						},
					},
				},
			},
		}

		// Simulate the iterator behavior
		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			// Send the response with function call
			fn(mockResp, nil)
		}

		adapter := NewStreamAdapter(iter, "test-model", true)

		// Read the response
		resp, err := adapter.Recv()
		require.NoError(t, err)

		// Should have tool calls
		require.NotEmpty(t, resp.Choices[0].Delta.ToolCalls)

		// Read the final message
		finalResp, err := adapter.Recv()
		require.NoError(t, err)

		// Should have finish reason tool_calls
		require.Equal(t, chat.FinishReasonToolCalls, finalResp.Choices[0].FinishReason)

		// Should NOT include tool calls in final message (to avoid duplication)
		require.Empty(t, finalResp.Choices[0].Delta.ToolCalls)
	})
}

func TestStreamAdapter_GeneratedImage(t *testing.T) {
	t.Parallel()

	t.Run("inline image-only chunk is forwarded as Delta.Media", func(t *testing.T) {
		imgBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}
		mockResp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{
								InlineData: &genai.Blob{
									Data:        imgBytes,
									MIMEType:    "image/png",
									DisplayName: "cat.png",
								},
							},
						},
					},
				},
			},
		}

		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			fn(mockResp, nil)
		}
		adapter := NewStreamAdapter(iter, "test-model", true)

		// A chunk carrying only inline media (no text, no function calls, no
		// usage) must still be forwarded rather than dropped by the run()
		// content gate.
		resp, err := adapter.Recv()
		require.NoError(t, err)
		require.Len(t, resp.Choices[0].Delta.Media, 1, "image-only chunk must be forwarded")
		assert.Equal(t, imgBytes, resp.Choices[0].Delta.Media[0].Data)
		assert.Equal(t, "image/png", resp.Choices[0].Delta.Media[0].MimeType)
		assert.Equal(t, "cat.png", resp.Choices[0].Delta.Media[0].Name)
		assert.Equal(t, int64(len(imgBytes)), resp.Choices[0].Delta.Media[0].Size)
		assert.Empty(t, resp.Choices[0].Delta.Content, "no text was in this chunk")

		// The synthesized done event must still report a normal stop —
		// generated media does not change finish-reason handling.
		done, err := adapter.Recv()
		require.NoError(t, err)
		assert.Equal(t, chat.FinishReasonStop, done.Choices[0].FinishReason)
	})

	t.Run("text and inline image in the same chunk both surface", func(t *testing.T) {
		imgBytes := []byte{0x01, 0x02, 0x03}
		mockResp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{Text: "here you go"},
							{InlineData: &genai.Blob{Data: imgBytes, MIMEType: "image/jpeg"}},
						},
					},
				},
			},
		}

		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			fn(mockResp, nil)
		}
		adapter := NewStreamAdapter(iter, "test-model", true)

		resp, err := adapter.Recv()
		require.NoError(t, err)
		assert.Equal(t, "here you go", resp.Choices[0].Delta.Content, "text handling must remain intact alongside media")
		require.Len(t, resp.Choices[0].Delta.Media, 1)
		assert.Equal(t, imgBytes, resp.Choices[0].Delta.Media[0].Data)
		assert.Equal(t, "image/jpeg", resp.Choices[0].Delta.Media[0].MimeType)
		// Provider omitted a display name; the adapter must not invent one —
		// that is the runtime accumulator's job.
		assert.Empty(t, resp.Choices[0].Delta.Media[0].Name)
	})

	t.Run("empty inline data is not surfaced as media", func(t *testing.T) {
		// A Blob with zero bytes must not produce a spurious empty Media
		// delta (and, since it carries no text either, must not be
		// forwarded as a chunk at all).
		mockResp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{InlineData: &genai.Blob{Data: nil, MIMEType: "image/png"}},
						},
					},
				},
			},
		}

		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			fn(mockResp, nil)
		}
		adapter := NewStreamAdapter(iter, "test-model", true)

		// Nothing at all was produced (no text, no media, no tool calls, no
		// usage), so the stream ends directly with EOF — same as any other
		// content-free chunk, no synthesized done event.
		_, err := adapter.Recv()
		require.ErrorIs(t, err, io.EOF)
	})

	t.Run("multiple inline blobs across parts and candidates in one chunk are all retained", func(t *testing.T) {
		// Gemini can pack more than one generated blob into a single chunk:
		// multiple parts within a candidate, and/or multiple candidates.
		// Every blob must survive, not just the last one seen.
		mockResp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{InlineData: &genai.Blob{Data: []byte{0x01}, MIMEType: "image/png", DisplayName: "first.png"}},
							{InlineData: &genai.Blob{Data: []byte{0x02}, MIMEType: "image/jpeg", DisplayName: "second.jpg"}},
						},
					},
				},
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{InlineData: &genai.Blob{Data: []byte{0x03}, MIMEType: "image/webp", DisplayName: "third.webp"}},
						},
					},
				},
			},
		}

		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			fn(mockResp, nil)
		}
		adapter := NewStreamAdapter(iter, "test-model", true)

		resp, err := adapter.Recv()
		require.NoError(t, err)
		require.Len(t, resp.Choices[0].Delta.Media, 3, "every blob in the chunk must be retained")
		assert.Equal(t, "first.png", resp.Choices[0].Delta.Media[0].Name)
		assert.Equal(t, "second.jpg", resp.Choices[0].Delta.Media[1].Name)
		assert.Equal(t, "third.webp", resp.Choices[0].Delta.Media[2].Name)
	})
}
