package oaistream

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasErrorObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want bool
	}{
		{"standard openai error", `{"error":{"message":"rate limit","type":"tokens"}}`, true},
		{"error is a string", `{"error":"Rate limit exceeded","retryAfterMs":30000}`, false},
		{"error field null", `{"error":null}`, false},
		{"error is a number", `{"error":429}`, false},
		{"error is a boolean", `{"error":true}`, false},
		{"error is an array", `{"error":["rate limit"]}`, false},
		{"no error field", `{"message":"rate limit exceeded"}`, false},
		{"plain text", `rate limit exceeded`, false},
		{"empty body", ``, false},
		{"empty object", `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hasErrorObject([]byte(tt.body))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWrapErrorBody(t *testing.T) {
	t.Parallel()

	t.Run("json body is preserved verbatim as error value", func(t *testing.T) {
		t.Parallel()
		body := `{"consumed":24000000000000,"error":"Rate limit exceeded","limit":50000000000000}`
		wrapped := wrapErrorBody([]byte(body), http.StatusTooManyRequests)
		assert.JSONEq(t, `{"error":`+body+`}`, string(wrapped))
	})

	t.Run("json without error field", func(t *testing.T) {
		t.Parallel()
		body := `{"message":"quota exceeded","retry_after":30}`
		wrapped := wrapErrorBody([]byte(body), http.StatusTooManyRequests)
		assert.JSONEq(t, `{"error":`+body+`}`, string(wrapped))
	})

	t.Run("plain text body wrapped as message", func(t *testing.T) {
		t.Parallel()
		wrapped := wrapErrorBody([]byte("rate limit exceeded"), http.StatusTooManyRequests)
		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(wrapped, &parsed))
		assert.Equal(t, "rate limit exceeded", parsed.Error.Message)
	})

	t.Run("empty body uses status text", func(t *testing.T) {
		t.Parallel()
		wrapped := wrapErrorBody(nil, http.StatusTooManyRequests)
		// "Too Many Requests" is not valid JSON, so it gets message-wrapped
		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(wrapped, &parsed))
		assert.Equal(t, "Too Many Requests", parsed.Error.Message)
	})
}

func TestErrorBodyMiddleware(t *testing.T) {
	t.Parallel()

	middleware := ErrorBodyMiddleware()

	t.Run("passes through successful responses", func(t *testing.T) {
		t.Parallel()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/v1/chat/completions", http.NoBody)
		require.NoError(t, err)

		next := func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(nil),
			}, nil
		}

		resp, err := middleware(req, next)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("passes through standard OpenAI errors", func(t *testing.T) {
		t.Parallel()

		originalBody := `{"error":{"message":"Rate limit exceeded","type":"rate_limit","code":"rate_limit_exceeded"}}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/v1/chat/completions", http.NoBody)
		require.NoError(t, err)

		next := func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(newStringReader(originalBody)),
			}, nil
		}

		resp, err := middleware(req, next)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.JSONEq(t, originalBody, string(body))
	})

	t.Run("wraps non-standard error bodies", func(t *testing.T) {
		t.Parallel()

		originalBody := `{"message":"You have exceeded your rate limit","retry_after":30}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/v1/chat/completions", http.NoBody)
		require.NoError(t, err)

		next := func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(newStringReader(originalBody)),
			}, nil
		}

		resp, err := middleware(req, next)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.JSONEq(t, `{"error":`+originalBody+`}`, string(body))
	})

	t.Run("wraps error bodies where error is a string not an object", func(t *testing.T) {
		t.Parallel()

		originalBody := `{"consumed":24000000000000,"error":"Rate limit exceeded","limit":50000000000000,"remaining":-621927204133070,"resetTime":1803944783,"retryAfterMs":33315396472,"retryAfterSeconds":33315396}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/v1/chat/completions", http.NoBody)
		require.NoError(t, err)

		next := func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(newStringReader(originalBody)),
			}, nil
		}

		resp, err := middleware(req, next)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		// The full original body is placed as the "error" value
		assert.JSONEq(t, `{"error":`+originalBody+`}`, string(body))
	})

	t.Run("wraps plain text error bodies", func(t *testing.T) {
		t.Parallel()

		originalBody := "Service temporarily unavailable"
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/v1/chat/completions", http.NoBody)
		require.NoError(t, err)

		next := func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(newStringReader(originalBody)),
			}, nil
		}

		resp, err := middleware(req, next)
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &parsed))
		assert.Equal(t, originalBody, parsed.Error.Message)
	})
}

func newStringReader(s string) io.Reader {
	return io.NopCloser(newBytesReader([]byte(s)))
}

func newBytesReader(b []byte) io.Reader {
	return &bytesReader{data: b}
}

type bytesReader struct {
	data []byte
	pos  int
}

func (r *bytesReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
