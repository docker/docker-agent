// Package vertex provides an Anthropic client for Claude models hosted on
// Google Cloud's Vertex AI. It lives in its own package so that importing
// the core anthropic provider does not pull the Google Cloud auth stack
// (cloud.google.com/go/auth, google.golang.org/api, grpc, ...).
package vertex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	sdkvertex "github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// cloudPlatformScope is the OAuth2 scope required for Vertex AI API access.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// NewClient creates a new Anthropic client that talks to Claude models
// hosted on Google Cloud's Vertex AI via the Anthropic-native endpoints
// (`:rawPredict` and `:streamRawPredict`), authenticated with Google
// Application Default Credentials.
//
// This is required because Anthropic models on Vertex AI do not support the
// OpenAI-compatible `/chat/completions` endpoint and fail with
// `FAILED_PRECONDITION: The deployed model does not support ChatCompletions.`
//
// See: https://docs.anthropic.com/en/api/claude-on-vertex-ai
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, project, location string, opts ...options.Opt) (*anthropic.Client, error) {
	return newClient(ctx, cfg, env, project, location, nil, opts...)
}

// NewClientWithTokenSource is NewClient authenticated with source instead of
// Application Default Credentials, which are never looked up. The token is
// resolved with each request's context, so per-request values such as the
// session ID and cancellation reach the source.
func NewClientWithTokenSource(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, project, location string, source options.TokenSource, opts ...options.Opt) (*anthropic.Client, error) {
	if source == nil {
		return nil, errors.New("token source is required")
	}
	return newClient(ctx, cfg, env, project, location, source, opts...)
}

func newClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, project, location string, source options.TokenSource, opts ...options.Opt) (*anthropic.Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}
	for _, name := range []string{"native_compaction", "cache_diagnostics"} {
		if enabled, _ := providerutil.GetProviderOptBool(cfg.ProviderOpts, name); enabled {
			return nil, fmt.Errorf("%s requires the Claude API and is not available on Vertex AI", name)
		}
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

	// Config validation (thinking_display, ...) runs inside
	// NewClientFromFactoryWithOptions before the factory below, so
	// configuration errors surface without requiring GCP credentials.
	anthropicClient, err := anthropic.NewClientFromFactoryWithOptions(ctx, cfg, env, func(ctx context.Context, globalOptions options.ModelOptions) (anthropicsdk.Client, error) {
		creds, err := credentials(ctx, source)
		if err != nil {
			return anthropicsdk.Client{}, err
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
		sdkOptions := []option.RequestOption{
			sdkvertex.WithCredentials(ctx, location, project, creds),
			option.WithAPIKey(""),
		}
		if source != nil {
			sdkOptions = append(sdkOptions, tokenSourceOptions(ctx, globalOptions, source)...)
		}
		return anthropicsdk.NewClient(sdkOptions...), nil
	}, opts...)
	if err != nil {
		return nil, err
	}

	slog.DebugContext(ctx, "Anthropic (Vertex AI) client created successfully", "model", cfg.Model)
	return anthropicClient, nil
}

// credentials returns Application Default Credentials or, with an explicit
// source, placeholder credentials whose token source is never used because
// tokenSourceOptions replaces the HTTP client built from them. The source is
// probed once so a broken one fails construction, like missing ADC does.
func credentials(ctx context.Context, source options.TokenSource) (*google.Credentials, error) {
	if source != nil {
		if _, err := source(ctx); err != nil {
			return nil, fmt.Errorf("resolving access token: %w", err)
		}
		return &google.Credentials{TokenSource: unusedTokenSource{}}, nil
	}
	// Resolve GCP credentials up front so we can return a descriptive error
	// instead of the panic that vertex.WithGoogleAuth would raise.
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain GCP credentials for Vertex AI: %w (run 'gcloud auth application-default login')", err)
	}
	return creds, nil
}

// tokenSourceOptions replaces the OAuth-wrapped HTTP client installed by
// vertex.WithCredentials: option.WithHTTPClient passed after it is used
// as-is, so the SDK never overrides the Authorization header that the
// middleware sets from the request context.
func tokenSourceOptions(ctx context.Context, globalOptions options.ModelOptions, source options.TokenSource) []option.RequestOption {
	httpClient := httpclient.NewHTTPClient(ctx)
	globalOptions.WrapTransport(ctx, httpClient)
	return []option.RequestOption{
		option.WithMiddleware(bearerTokenMiddleware(source)),
		option.WithHTTPClient(httpClient),
	}
}

// unusedTokenSource keeps vertex.WithCredentials from looking up ADC for the
// HTTP client it builds; that client is replaced, so no token is ever minted.
type unusedTokenSource struct{}

func (unusedTokenSource) Token() (*oauth2.Token, error) {
	return nil, errors.New("vertex AI: the SDK token source must not be used; tokens come from the request middleware")
}

// bearerTokenMiddleware resolves the bearer token on every request.
func bearerTokenMiddleware(source options.TokenSource) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		token, err := source(req.Context())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return next(req)
	}
}
