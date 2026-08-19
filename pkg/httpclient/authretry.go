package httpclient

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// authHeaders are the headers our gateway clients present a token in: OpenAI
// and Anthropic use Authorization (and x-api-key), Gemini x-goog-api-key.
var authHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"}

// authRetryTransport re-authenticates once when the server rejects the token a
// request presented. Docker's gateway tokens are short-lived and can be
// revoked or rotated at any time, and only the gateway knows for sure whether
// the one we hold still works: a 401 is a more reliable signal than any local
// expiry arithmetic, which a skewed clock or a stale cache can get wrong.
type authRetryTransport struct {
	base http.RoundTripper

	// refresh returns a token to replace the rejected one, or an error when
	// none can be obtained.
	refresh func(ctx context.Context, rejected string) (string, error)
}

func (t *authRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	rejected := presentedToken(req.Header)
	if rejected == "" {
		return resp, nil
	}
	// A body we cannot rewind cannot be replayed.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, nil
	}

	fresh, err := t.refresh(req.Context(), rejected)
	if err != nil || fresh == "" || fresh == rejected {
		return resp, nil
	}

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return resp, nil
		}
		retry.Body = body
	}
	replaceToken(retry.Header, rejected, fresh)

	// Release the connection the rejected response is holding; nobody will
	// read its body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()

	// Straight to the base transport: one retry, never a loop.
	return t.base.RoundTrip(retry)
}

// presentedToken returns the token a request authenticated with.
func presentedToken(header http.Header) string {
	for _, name := range authHeaders {
		if value := header.Get(name); value != "" {
			return strings.TrimPrefix(value, "Bearer ")
		}
	}
	return ""
}

// replaceToken swaps rejected for fresh in every header carrying it, keeping
// whatever scheme prefix the client used.
func replaceToken(header http.Header, rejected, fresh string) {
	for _, name := range authHeaders {
		value := header.Get(name)
		if value == "" || !strings.Contains(value, rejected) {
			continue
		}
		header.Set(name, strings.Replace(value, rejected, fresh, 1))
	}
}
