package oaistream

import (
	"errors"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// WrapOpenAIError wraps an OpenAI-compatible *APIError in a *modelerrors.StatusError
// to carry HTTP status code and Retry-After metadata for the retry loop.
// Non-API errors (e.g. io.EOF, network errors) pass through unchanged.
func WrapOpenAIError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		return err
	}
	return modelerrors.WrapHTTPError(apiErr.StatusCode, apiErr.Response, err)
}
