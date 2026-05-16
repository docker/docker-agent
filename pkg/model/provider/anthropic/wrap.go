package anthropic

import (
	"errors"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// wrapAnthropicError wraps an Anthropic SDK error in a *modelerrors.StatusError
// to carry HTTP status code and Retry-After metadata for the retry loop.
// Non-Anthropic errors (e.g. io.EOF, network errors) pass through unchanged.
func wrapAnthropicError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := errors.AsType[*anthropic.Error](err)
	if !ok {
		return err
	}
	return modelerrors.WrapHTTPError(apiErr.StatusCode, apiErr.Response, err)
}
