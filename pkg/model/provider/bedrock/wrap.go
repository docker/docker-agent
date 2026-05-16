package bedrock

import (
	"errors"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// wrapBedrockError wraps an AWS Bedrock SDK error in a *modelerrors.StatusError
// to carry HTTP status code metadata for the retry loop.
// The AWS SDK v2 exposes HTTP status via smithyhttp.ResponseError.
// Non-AWS errors (e.g., io.EOF, network errors) pass through unchanged.
func wrapBedrockError(err error) error {
	if err == nil {
		return nil
	}

	var respErr *smithyhttp.ResponseError
	if !errors.As(err, &respErr) {
		return err
	}

	var resp *smithyhttp.Response
	if respErr.HTTPResponse() != nil {
		resp = respErr.HTTPResponse()
	}

	statusCode := respErr.HTTPStatusCode()
	if resp != nil {
		return modelerrors.WrapHTTPError(statusCode, resp.Response, err)
	}
	return modelerrors.WrapHTTPError(statusCode, nil, err)
}
