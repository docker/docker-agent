//go:build js && wasm && docker_agent_wasm_cloud

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/bedrock"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

// cloudFactories are the cloud providers of the browser build, opted into
// with -tags docker_agent_wasm_cloud so the default demo does not link the
// AWS and Google Cloud SDKs. Credentials come from the session env only:
// no credential chain, ADC or host discovery, which would read files the
// browser does not have or wait on metadata probes. See examples/cloud-*.yaml.
func cloudFactories() map[string]provider.Factory {
	return map[string]provider.Factory{
		// The js build of bedrock only accepts a bearer token (token_key or
		// AWS_BEARER_TOKEN_BEDROCK) and rejects profile/role_arn.
		"amazon-bedrock": provider.Adapt(bedrock.NewClient),
		"dmr":            cloudDMR,
	}
}

// googleFactory serves Gemini through the Gemini API with an API key as usual,
// and Vertex AI (Gemini, or Model Garden when a publisher is set) with an
// OAuth access token read from the session env on every request: the model's
// token_key, or GOOGLE_OAUTH_ACCESS_TOKEN. A missing token fails when the
// session is created; ADC and the Gemini API are never fallen back to.
func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	if !wantsVertexAI(ctx, cfg, env) {
		return gemini.NewClient(ctx, cfg, env, opts...)
	}
	key := cmp.Or(cfg.TokenKey, vertexTokenEnv)
	source := func(ctx context.Context) (string, error) {
		token, _ := env.Get(ctx, key)
		if token == "" {
			return "", fmt.Errorf("vertex AI in the browser requires an OAuth access token in the session env (%s)", key)
		}
		return token, nil
	}
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexai.NewClientWithTokenSource(ctx, cfg, env, source, opts...)
	}
	return gemini.NewClient(ctx, cfg, env, append(opts, options.WithTokenSource(source))...)
}

// cloudDMR serves Docker Model Runner through an explicit http(s) base_url
// only: there is no host CLI, socket or MODEL_RUNNER_HOST to discover one.
func cloudDMR(ctx context.Context, cfg *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("dmr in the browser requires an explicit http(s) base_url")
	}
	return dmr.NewClient(ctx, cfg, opts...)
}
