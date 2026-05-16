package openai

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/option"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// GitHub Copilot's API requires a Copilot-Integration-Id header to
// identify the client integration when authenticating with a GitHub
// token. Without it, requests to https://api.githubcopilot.com are
// rejected with "Bad Request".
//
// We default to "copilot-developer-cli" because, unlike "vscode-chat",
// it is accepted by the Copilot API for both OAuth tokens and Personal
// Access Tokens (PATs). Most docker-agent users authenticate with a
// PAT exported as GITHUB_TOKEN, so this default makes the provider
// usable out of the box.
//
// Users can still override this via provider_opts.http_headers (see
// buildHeaderOptions below).
//
// See https://github.com/docker/docker-agent/issues/2471
const (
	copilotIntegrationIDHeader  = "Copilot-Integration-Id"
	copilotIntegrationIDDefault = "copilot-developer-cli"
)

// buildHeaderOptions returns OpenAI client options for every custom
// HTTP header configured for the model, including provider-specific
// defaults.
//
// Users can set headers via provider_opts.http_headers:
//
//	models:
//	  copilot:
//	    provider: github-copilot
//	    model: gpt-4o
//	    provider_opts:
//	      http_headers:
//	        Copilot-Integration-Id: vscode-chat # override default
//
// For the github-copilot provider a default Copilot-Integration-Id is
// injected when the user has not set one. Header names are compared
// case-insensitively, so any user-provided header always overrides the
// default.
func buildHeaderOptions(cfg *latest.ModelConfig) []option.RequestOption {
	headers := buildHeaderMap(cfg)
	opts := make([]option.RequestOption, 0, len(headers))
	for name, value := range headers {
		opts = append(opts, option.WithHeader(name, value))
	}
	return opts
}

// buildHeaderMap returns a map of HTTP headers to send with requests,
// including provider-specific defaults and user-configured headers from
// provider_opts.http_headers. Header names are canonicalized for
// case-insensitive deduplication.
func buildHeaderMap(cfg *latest.ModelConfig) map[string]string {
	// Canonicalizing keys de-duplicates headers case-insensitively:
	// defaults are applied first, then user config clobbers conflicts.
	headers := map[string]string{}
	if cfg != nil && cfg.Provider == "github-copilot" {
		headers[copilotIntegrationIDHeader] = copilotIntegrationIDDefault
	}
	for name, value := range userHeaders(cfg) {
		headers[http.CanonicalHeaderKey(name)] = sanitizeHeaderValue(value)
	}
	return headers
}

// userHeaders parses provider_opts.http_headers into a simple string
// map. Malformed entries are logged and skipped so a typo doesn't
// silently reach the wire.
func userHeaders(cfg *latest.ModelConfig) map[string]string {
	if cfg == nil || cfg.ProviderOpts == nil {
		return nil
	}
	raw, ok := cfg.ProviderOpts["http_headers"]
	if !ok || raw == nil {
		return nil
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		slog.Warn("provider_opts.http_headers must be a map of string to string, ignoring", "value", raw)
		return nil
	}
	headers := make(map[string]string, len(rawMap))
	for k, v := range rawMap {
		s, ok := v.(string)
		if !ok {
			slog.Warn("provider_opts.http_headers value must be a string, ignoring", "header", k, "value", v)
			continue
		}
		headers[k] = s
	}
	return headers
}

// sanitizeHeaderValue removes CR and LF characters from header values to
// prevent header injection attacks. HTTP header values must not contain
// newlines (RFC 7230 section 3.2).
func sanitizeHeaderValue(value string) string {
	// Remove all CR and LF characters
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	// Also strip leading/trailing whitespace for cleanliness
	value = strings.TrimSpace(value)
	return value
}
