package gemini

import (
	"errors"

	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// wrapGeminiError wraps a Gemini SDK error in a *modelerrors.StatusError
// to carry HTTP status code metadata for the retry loop.
// Gemini's *genai.APIError does not expose *http.Response, so no Retry-After
// header extraction is possible; the RetryAfter field will be zero.
// Non-Gemini errors (e.g. io.EOF, network errors) pass through unchanged.
func wrapGeminiError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := errors.AsType[*genai.APIError](err)
	if !ok {
		return err
	}
	// Pass nil for resp — Gemini doesn't expose *http.Response.
	return modelerrors.WrapHTTPError(apiErr.Code, nil, err)
}
