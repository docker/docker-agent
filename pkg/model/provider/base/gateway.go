package base

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/url"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/desktop"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// VerifyDockerGatewayAuth fails fast when gateway targets a trusted Docker
// domain but Docker Desktop's auth token is unavailable. Provider clients
// call it at construction time so a missing sign-in surfaces before the
// first request. Non-Docker gateways need no Desktop token and always pass.
func VerifyDockerGatewayAuth(ctx context.Context, env environment.Provider, gateway string) error {
	if !environment.IsTrustedDockerURL(gateway) {
		return nil
	}
	if token, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); token == "" {
		return errors.New("sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
	}
	return nil
}

// GatewayAuthToken returns a fresh Docker Desktop auth token when gateway
// targets a trusted Docker domain, or "" for other gateways. Gateway clients
// call it on every request because Desktop tokens are short-lived.
func GatewayAuthToken(ctx context.Context, env environment.Provider, gateway string) (string, error) {
	if !environment.IsTrustedDockerURL(gateway) {
		return "", nil
	}
	token, _ := env.Get(ctx, environment.DockerDesktopTokenEnv)
	if token == "" {
		return "", errors.New(NoDesktopTokenErrorMessage)
	}
	return token, nil
}

// GatewayAuthRetry lets a client recover from a gateway that rejects the Docker
// token it presented: the token is forgotten and the request replayed once with
// a fresh one. Empty for gateways that don't authenticate with a Docker login,
// and a no-op when the token comes from a static source (an explicitly set
// DOCKER_TOKEN can't be refreshed, and must not be second-guessed).
func GatewayAuthRetry(env environment.Provider, gateway string) []httpclient.Opt {
	if !environment.IsTrustedDockerURL(gateway) {
		return nil
	}
	return []httpclient.Opt{httpclient.WithUnauthorizedRetry(func(ctx context.Context, rejected string) (string, error) {
		slog.WarnContext(ctx, "The Docker AI gateway rejected our token, re-authenticating")
		desktop.InvalidateToken(rejected)
		return GatewayAuthToken(ctx, env, gateway)
	})}
}

// GatewayHTTPOptions builds the httpclient options shared by all
// gateway-mode provider clients: the proxied base URL (the provider's public
// endpoint unless the model overrides base_url), provider/model identity,
// the gateway's query parameters, and the title-generation / compaction
// markers. A nil modelOpts is treated as zero options (no markers).
func GatewayHTTPOptions(gatewayURL *url.URL, defaultBaseURL string, cfg *latest.ModelConfig, modelOpts *options.ModelOptions) []httpclient.Opt {
	if modelOpts == nil {
		modelOpts = &options.ModelOptions{}
	}
	opts := []httpclient.Opt{
		httpclient.WithProxiedBaseURL(cmp.Or(cfg.BaseURL, defaultBaseURL)),
		httpclient.WithProvider(cfg.Provider),
		httpclient.WithModel(cfg.Model),
		httpclient.WithModelName(cfg.Name),
		httpclient.WithQuery(gatewayURL.Query()),
	}
	// Forward the encrypted agent config to trusted Docker gateways only. The
	// gateway string here is the full URL; reuse the same trust check the
	// Docker JWT injection relies on so we never leak the value to third-party
	// gateways. It rides in the JSON request body (not a header) to avoid
	// header-size limits; the gateway strips it before forwarding upstream.
	if enc := modelOpts.EncryptedConfig(); enc != "" {
		if environment.IsTrustedDockerURL(gatewayURL.String()) {
			opts = append(opts, httpclient.WithEncryptedConfigBody(enc))
			slog.Debug("Forwarding encrypted agent config to Docker gateway",
				"body_field", httpclient.EncryptedConfigBodyField,
				"gateway", gatewayURL.String(),
				"provider", cfg.Provider,
				"model", cfg.Model,
				"value_length", len(enc))
		} else {
			slog.Debug("Not forwarding encrypted agent config: gateway is not a trusted Docker URL",
				"gateway", gatewayURL.String())
		}
	}
	if modelOpts.GeneratingTitle() {
		opts = append(opts, httpclient.WithHeader("X-Cagent-GeneratingTitle", "1"))
	}
	if modelOpts.Compacting() {
		opts = append(opts, httpclient.WithHeader("X-Cagent-Compacting", "1"))
	}
	return opts
}
