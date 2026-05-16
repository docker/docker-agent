package anthropic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// vertexCloudPlatformScope is the OAuth2 scope required for Vertex AI API access.
const vertexCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// NewVertexClient creates a new Anthropic client that talks to Claude models
// hosted on Google Cloud's Vertex AI via the Anthropic-native endpoints
// (`:rawPredict` and `:streamRawPredict`), authenticated with Google
// Application Default Credentials.
//
// This is required because Anthropic models on Vertex AI do not support the
// OpenAI-compatible `/chat/completions` endpoint and fail with
// `FAILED_PRECONDITION: The deployed model does not support ChatCompletions.`
//
// See: https://docs.anthropic.com/en/api/claude-on-vertex-ai
func NewVertexClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, project, location string, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}
	if env == nil {
		return nil, errors.New("environment provider is required")
	}
	if project == "" {
		return nil, errors.New("vertex AI requires a GCP project")
	}
	if location == "" {
		return nil, errors.New("vertex AI requires a GCP location")
	}

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&globalOptions)
		}
	}

	// Resolve GCP credentials up front so we can return a descriptive error
	// instead of the panic that vertex.WithGoogleAuth would raise.
	creds, err := google.FindDefaultCredentials(ctx, vertexCloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain GCP credentials for Vertex AI: %w (run 'gcloud auth application-default login')", err)
	}

	slog.DebugContext(ctx, "Creating Anthropic client for Vertex AI",
		"project", project,
		"location", location,
		"model", cfg.Model,
	)

	// vertex.WithCredentials configures the base URL, Google-authenticated
	// HTTP client, and middleware that rewrites /v1/messages requests to the
	// Anthropic-native Vertex AI endpoints (`:rawPredict` / `:streamRawPredict`)
	// and injects the `anthropic_version: vertex-2023-10-16` body field.
	//
	// The explicit option.WithAPIKey("") is REQUIRED (do not remove): the
	// anthropic SDK's NewClient applies DefaultClientOptions() first, which
	// auto-reads ANTHROPIC_API_KEY from the environment and sets the
	// X-Api-Key header. On Vertex AI the request is authenticated with
	// OAuth2 (via the google transport in vertex.WithCredentials), so we
	// must clear the stray X-Api-Key header that would otherwise leak a
	// direct-API credential into Google's infrastructure.
	client := anthropic.NewClient(
		vertex.WithCredentials(ctx, location, project, creds),
		option.WithAPIKey(""),
	)

	anthropicClient := &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: func(context.Context) (anthropic.Client, error) {
			return client, nil
		},
	}

	// File uploads via Anthropic's Files API are not supported on Vertex AI,
	// but the FileManager is lazy and harmless if unused.
	anthropicClient.fileManager = NewFileManager(anthropicClient.clientFn)

	slog.DebugContext(ctx, "Anthropic (Vertex AI) client created successfully", "model", cfg.Model)
	return anthropicClient, nil
}
