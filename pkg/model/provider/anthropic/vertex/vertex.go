// Package vertex provides an Anthropic client for Claude models hosted on
// Google Cloud's Vertex AI.
package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2/google"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

const (
	cloudPlatformScope   = "https://www.googleapis.com/auth/cloud-platform"
	vertexDefaultVersion = "vertex-2023-10-16"
)

// NewClient creates a new Anthropic client that talks to Claude models
// hosted on Google Cloud's Vertex AI via the Anthropic-native endpoints
// (`:rawPredict` and `:streamRawPredict`), authenticated with Google
// Application Default Credentials.
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, project, location string, opts ...options.Opt) (*anthropic.Client, error) {
	return newClient(ctx, cfg, env, project, location, nil, opts...)
}

// NewClientWithTokenSource is NewClient authenticated with source instead of
// Application Default Credentials, which are never looked up.
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
	if providerutil.HasProviderOpt(cfg.ProviderOpts, "fallbacks") {
		return nil, errors.New("fallbacks requires the Claude API and is not available on Vertex AI")
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

	anthropicClient, err := anthropic.NewClientFromFactoryWithOptions(ctx, cfg, env, func(ctx context.Context, globalOptions options.ModelOptions) (*anthropic.APIClient, error) {
		tokenSource, err := resolveTokenSource(ctx, source)
		if err != nil {
			return nil, err
		}

		slog.DebugContext(ctx, "Creating Anthropic client for Vertex AI",
			"project", project,
			"location", location,
			"model", cfg.Model,
		)

		baseURL := vertexBaseURL(location)
		httpClient := wrappedHTTPClient(ctx, globalOptions)

		mw := vertexMiddleware(project, location, tokenSource)
		return anthropic.NewAPIClient(anthropic.APIClientConfig{
			BaseURL:    baseURL,
			HTTPClient: httpClient,
			Middleware: mw,
		}), nil
	}, opts...)
	if err != nil {
		return nil, err
	}

	slog.DebugContext(ctx, "Anthropic (Vertex AI) client created successfully", "model", cfg.Model)
	return anthropicClient, nil
}

func vertexBaseURL(location string) string {
	switch location {
	case "global":
		return "https://aiplatform.googleapis.com"
	case "us", "eu":
		return fmt.Sprintf("https://aiplatform.%s.rep.googleapis.com", location)
	default:
		return fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
	}
}

func resolveTokenSource(ctx context.Context, source options.TokenSource) (options.TokenSource, error) {
	if source != nil {
		if _, err := source(ctx); err != nil {
			return nil, fmt.Errorf("resolving access token: %w", err)
		}
		return source, nil
	}
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain GCP credentials for Vertex AI: %w (run 'gcloud auth application-default login')", err)
	}
	return func(context.Context) (string, error) {
		tok, err := creds.TokenSource.Token()
		if err != nil {
			return "", fmt.Errorf("getting GCP access token: %w", err)
		}
		return tok.AccessToken, nil
	}, nil
}

func vertexMiddleware(project, location string, tokenSource options.TokenSource) anthropic.RequestMiddleware {
	return func(req *http.Request, body []byte) (*http.Request, []byte, error) {
		token, err := tokenSource(req.Context())
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Del("X-Api-Key")
		req.Header.Del("anthropic-version")

		if len(body) > 0 {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(body, &payload); err == nil {
				if _, exists := payload["anthropic_version"]; !exists {
					payload["anthropic_version"] = json.RawMessage(`"` + vertexDefaultVersion + `"`)
				}
				switch req.URL.Path {
				case "/v1/messages":
					var model string
					if rawModel, ok := payload["model"]; ok {
						_ = json.Unmarshal(rawModel, &model)
						delete(payload, "model")
					}
					var stream bool
					if rawStream, ok := payload["stream"]; ok {
						_ = json.Unmarshal(rawStream, &stream)
					}
					specifier := "rawPredict"
					if stream {
						specifier = "streamRawPredict"
					}
					req.URL.Path = fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
						url.PathEscape(project), url.PathEscape(location), url.PathEscape(model), specifier)
				case "/v1/messages/count_tokens":
					req.URL.Path = fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/anthropic/models/count-tokens:rawPredict",
						url.PathEscape(project), url.PathEscape(location))
				}
				if rewritten, err := json.Marshal(payload); err == nil {
					body = rewritten
				}
			}
		}

		// Vertex AI rejects fine-grained-tool-streaming beta header on some versions,
		// keep anthropic-beta intact as the SDK does.
		_ = strings.TrimSpace
		return req, body, nil
	}
}

func wrappedHTTPClient(ctx context.Context, globalOptions options.ModelOptions) *http.Client {
	client := httpclient.NewHTTPClient(ctx)
	globalOptions.WrapTransport(ctx, client)
	return client
}
