package oaistream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// ErrorBodyMiddleware wraps non-standard HTTP error response bodies into
// {"error": <original body>} so full error details are preserved.
func ErrorBodyMiddleware() func(*http.Request, func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	return func(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
		resp, err := next(req)
		if err != nil || resp == nil || resp.StatusCode < 400 {
			return resp, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || hasErrorObject(body) {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, nil
		}

		wrapped := wrapErrorBody(body, resp.StatusCode)
		resp.Body = io.NopCloser(bytes.NewReader(wrapped))
		resp.ContentLength = int64(len(wrapped))
		return resp, nil
	}
}

// hasErrorObject reports whether body is a JSON object with an "error" key
// whose value is itself a JSON object.
func hasErrorObject(body []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	errVal, ok := raw["error"]
	if !ok {
		return false
	}
	trimmed := bytes.TrimLeft(errVal, " \t\n\r")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// wrapErrorBody produces {"error": <body>} when body is valid JSON, or
// {"error": {"message": "<body>"}} otherwise, so downstream error formatting
// always preserves full details.
func wrapErrorBody(body []byte, statusCode int) []byte {
	if len(body) == 0 {
		body = []byte(http.StatusText(statusCode))
	}
	if json.Valid(body) {
		return append(append([]byte(`{"error":`), body...), '}')
	}
	wrapped, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": string(body)},
	})
	if err != nil {
		return body
	}
	return wrapped
}
