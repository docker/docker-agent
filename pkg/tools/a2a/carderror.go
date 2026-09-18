package a2a

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/a2aclient/agentcard"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// retryAfterRecorder records the status and Retry-After header of the most
// recent >=400 response, since agentcard.Resolver discards the *http.Response
// on error. No mutex: Start issues one synchronous GET per attempt.
type retryAfterRecorder struct {
	base http.RoundTripper

	status     int
	retryAfter string
}

func (r *retryAfterRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode >= 400 {
		r.status = resp.StatusCode
		r.retryAfter = resp.Header.Get("Retry-After")
	}
	return resp, err
}

func (r *retryAfterRecorder) snapshot() (status int, retryAfter string) {
	return r.status, r.retryAfter
}

// enrichCardError wraps a failed card resolution as *modelerrors.StatusError
// when err is an *agentcard.ErrStatusNotOK, forwarding rec's Retry-After only
// when its recorded status matches, mirroring enrichConnectError (remote.go).
func enrichCardError(err error, rec *retryAfterRecorder) error {
	wrapped := fmt.Errorf("failed to fetch A2A agent card: %w", err)

	var statusErr *agentcard.ErrStatusNotOK
	if !errors.As(err, &statusErr) {
		return wrapped
	}

	resp := &http.Response{Header: http.Header{}}
	if rec != nil {
		if status, retryAfter := rec.snapshot(); status == statusErr.StatusCode && retryAfter != "" {
			resp.Header.Set("Retry-After", retryAfter)
		}
	}
	return modelerrors.WrapHTTPError(statusErr.StatusCode, resp, wrapped)
}
