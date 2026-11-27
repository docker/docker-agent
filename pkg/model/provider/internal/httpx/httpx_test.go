package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSEReader(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		": keep-alive comment",
		"",
		"event: message_start",
		"id: 1",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_delta",
		"data: line1",
		"data: line2",
		"",
		"data: [DONE]",
		"",
	}, "\r\n")

	r := NewSSEReader(io.NopCloser(strings.NewReader(raw)))
	defer r.Close()

	require.True(t, r.Next())
	assert.Equal(t, "message_start", r.Event().Event)
	assert.Equal(t, "1", r.Event().ID)
	assert.JSONEq(t, `{"type":"message_start"}`, string(r.Event().Data))

	require.True(t, r.Next())
	assert.Equal(t, "content_block_delta", r.Event().Event)
	assert.Equal(t, "line1\nline2", string(r.Event().Data))

	require.True(t, r.Next())
	assert.True(t, IsSSEDone(r.Event().Data))

	assert.False(t, r.Next())
	assert.NoError(t, r.Err())
}

func TestSSEReaderLargePayload(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("a", 256*1024)
	r := NewSSEReader(io.NopCloser(strings.NewReader("data: " + large + "\n\n")))
	defer r.Close()

	require.True(t, r.Next())
	assert.Equal(t, large, string(r.Event().Data))
	assert.False(t, r.Next())
	assert.NoError(t, r.Err())
}

func TestDoRetryAndExtraBody(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After-Ms", "10")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	var out struct {
		OK bool `json:"ok"`
	}
	err := DoJSON(t.Context(), srv.Client(), RequestOptions{
		URL:       srv.URL + "/test",
		ExtraBody: map[string]any{"extra": 42},
		Retry: RetryConfig{
			MaxRetries:     2,
			InitialBackoff: time.Millisecond,
		},
	}, map[string]any{"model": "m1"}, &out)
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.Equal(t, int32(2), attempts.Load())
	assert.Equal(t, "m1", gotBody["model"])
	assert.InDelta(t, float64(42), gotBody["extra"], 0)
}

func TestAPIErrorFormatting(t *testing.T) {
	t.Parallel()

	err := &APIError{
		Method:     http.MethodPost,
		URL:        "https://api.example.com/v1/messages",
		StatusCode: http.StatusTooManyRequests,
		Body:       []byte(`{"error":{"type":"rate_limit_error","message":"too many requests"}}`),
	}
	assert.Contains(t, err.Error(), "429 Too Many Requests")
	assert.Contains(t, err.Error(), `"rate_limit_error"`)
}
