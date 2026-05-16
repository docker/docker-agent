package oaistream

import (
	"errors"

	openaisdk "github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// WrapOpenAIError wraps an OpenAI SDK error in a *modelerrors.StatusError
// to carry HTTP status code and Retry-After metadata for the retry loop.
// Non-OpenAI errors (e.g. io.EOF, network errors) pass through unchanged.
// Exported so openai/response_stream.go can reuse it without duplication.
func WrapOpenAIError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := errors.AsType[*openaisdk.Error](err)
	if !ok {
		return err
	}
	return modelerrors.WrapHTTPError(apiErr.StatusCode, apiErr.Response, err)
}
