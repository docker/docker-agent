//go:build js && wasm && !docker_agent_wasm_cloud

package main

import (
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// cloudFactories is empty unless the binary is built with
// -tags docker_agent_wasm_cloud; see cloud.go.
func cloudFactories() map[string]provider.Factory {
	return nil
}

// googleFactory serves the Gemini API only. Vertex-shaped models are rejected
// before gemini.NewClient would select the Vertex backend, whose default
// credential lookup has nothing to find in a browser.
func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	if wantsVertexAI(ctx, cfg, env) {
		return nil, fmt.Errorf("vertex AI in the browser requires a build with -tags docker_agent_wasm_cloud and an OAuth access token in the session env (%s or token_key)", vertexTokenEnv)
	}
	return gemini.NewClient(ctx, cfg, env, opts...)
}
