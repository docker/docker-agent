package oaistream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openai/openai-go/v3/option"
)

// ErrorBodyMiddleware returns an OpenAI SDK middleware that preserves full
// error details in HTTP error responses.
//
// The OpenAI SDK extracts only the "error" field from error response bodies
// (via gjson). When a provider returns a body without an "error" object
// (e.g. a string "error" field, plain text, or a different JSON structure),
// the details are silently lost. This middleware rewrites such responses into
// {"error": <original body>} so the SDK preserves the full content.
func ErrorBodyMiddleware() option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
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
// whose value is itself a JSON object — the format the OpenAI SDK expects.
func hasErrorObject(body []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	errVal, ok := raw["error"]
	if !ok {
		return false
	}
	return len(bytes.TrimLeft(errVal, " \t\n\r")) > 0 && bytes.TrimLeft(errVal, " \t\n\r")[0] == '{'
}

// wrapErrorBody produces {"error": <body>} when body is valid JSON, or
// {"error": {"message": "<body>"}} otherwise, so the SDK's gjson extraction
// always finds useful content.
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
