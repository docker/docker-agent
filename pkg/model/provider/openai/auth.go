package openai

import (
	"net/http"

	"github.com/openai/openai-go/v3/option"

	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func tokenAuthMiddleware(source options.TokenSource) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		token, err := source(req.Context())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return next(req)
	}
}
