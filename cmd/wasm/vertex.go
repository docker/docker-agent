//go:build js && wasm

package main

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// vertexTokenEnv is the session env key holding the Google OAuth access
// token for Vertex AI when the model sets no token_key.
const vertexTokenEnv = "GOOGLE_OAUTH_ACCESS_TOKEN"

// wantsVertexAI mirrors the Vertex AI detection of gemini.NewClient and adds
// the Model Garden publisher, without linking the Vertex SDKs.
func wantsVertexAI(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) bool {
	_, useVertexAIEnv := env.Get(ctx, "GOOGLE_GENAI_USE_VERTEXAI")
	publisher, _ := cfg.ProviderOpts["publisher"].(string)
	return cfg.ProviderOpts["project"] != nil || cfg.ProviderOpts["location"] != nil || useVertexAIEnv ||
		(publisher != "" && !strings.EqualFold(publisher, "google"))
}
