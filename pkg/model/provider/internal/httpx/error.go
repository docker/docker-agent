package httpx

import (
	"fmt"
	"net/http"
	"strings"
)

// APIError represents a non-2xx HTTP response from a model provider API.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (e *APIError) Error() string {
	statusText := http.StatusText(e.StatusCode)
	body := strings.TrimSpace(string(e.Body))
	if body == "" {
		if e.Method != "" && e.URL != "" {
			return fmt.Sprintf("%s %q: %d %s", e.Method, e.URL, e.StatusCode, statusText)
		}
		return fmt.Sprintf("HTTP %d %s", e.StatusCode, statusText)
	}
	if e.Method != "" && e.URL != "" {
		return fmt.Sprintf("%s %q: %d %s %s", e.Method, e.URL, e.StatusCode, statusText, body)
	}
	return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, statusText, body)
}

// HTTPResponse returns a minimal *http.Response carrying StatusCode and Header
// for helpers such as modelerrors.WrapHTTPError.
func (e *APIError) HTTPResponse() *http.Response {
	if e == nil {
		return nil
	}
	return &http.Response{
		StatusCode: e.StatusCode,
		Header:     e.Header,
	}
}
