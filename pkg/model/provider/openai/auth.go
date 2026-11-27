package openai

import (
	"context"
	"net/http"

	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func tokenAuthHeaderFunc(source options.TokenSource) func(context.Context, http.Header) error {
	return func(ctx context.Context, h http.Header) error {
		token, err := source(ctx)
		if err != nil {
			return err
		}
		h.Set("Authorization", "Bearer "+token)
		return nil
	}
}
