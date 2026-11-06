package config

import (
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
)

// bedrockCredentialIndicators are quick heuristics for AWS credentials, which
// can come from many sources:
//   - AWS_ACCESS_KEY_ID: explicit access key
//   - AWS_PROFILE / AWS_DEFAULT_PROFILE: named profile (credentials in ~/.aws/)
//   - AWS_WEB_IDENTITY_TOKEN_FILE: EKS/IRSA web identity
//   - AWS_CONTAINER_CREDENTIALS_RELATIVE_URI: ECS task role
//   - AWS_ROLE_ARN: assumed role
//
// This won't catch all cases (e.g. EC2 instance profiles, SSO) but those
// require network calls which would block interactive listings.
var bedrockCredentialIndicators = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_PROFILE",
	"AWS_DEFAULT_PROFILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_ROLE_ARN",
}

// apiCredentialIndicators supplements token detection for APIs with native auth.
func apiCredentialIndicators(apiType string) []string {
	switch apiType {
	case "google":
		return []string{"GOOGLE_GENAI_USE_VERTEXAI"}
	case "amazon-bedrock":
		return bedrockCredentialIndicators
	default:
		return nil
	}
}

func credentialConfigured(name, value string) bool {
	if name == "GOOGLE_GENAI_USE_VERTEXAI" {
		enabled, _ := strconv.ParseBool(value)
		return enabled
	}
	return value != ""
}

// DiscoveryAvailableProviders reports which providers are directly usable
// from local credentials, for model discovery/listing purposes only. Unlike
// [AvailableProviders] — which drives auto model selection and applies a
// gateway-specific default — it deliberately ignores any configured models
// gateway: it backs the fallback taken by `docker agent models` and the
// runtime model picker when gateway discovery fails or none is configured,
// so a gateway must neither suppress directly-configured providers nor
// require a Docker token.
//
// A provider is available when one of its known credential env vars is set
// (the same detection table auto-selection uses), or when it is a provider
// alias whose token variable is set and whose (possibly templated) base URL
// resolves from the environment. Local providers that need no credentials
// (DMR, ollama) are left to the caller, as is any config-derived
// availability (e.g. non-API-key auth schemes).
func DiscoveryAvailableProviders(ctx context.Context, env environment.Provider) map[string]bool {
	names := make([]string, 0, 64)
	for _, p := range cloudProviders {
		names = append(names, p.envVars...)
	}
	for _, alias := range catalog.Providers {
		names = append(names, alias.TokenEnvVars...)
		names = append(names, apiCredentialIndicators(alias.APIType)...)
		// A templated base URL (e.g. Cloudflare's account/gateway-scoped
		// endpoint) also needs the vars it references.
		names = append(names, environment.Refs(alias.BaseURL)...)
	}

	values := lookupEnvironmentValues(ctx, env, names)

	available := make(map[string]bool)
	for _, p := range cloudProviders {
		for _, envVar := range p.envVars {
			if credentialConfigured(envVar, values[envVar]) {
				available[p.name] = true
				break
			}
		}
	}

	for name, alias := range catalog.Providers {
		keys := slices.Concat(alias.TokenEnvVars, apiCredentialIndicators(alias.APIType))
		if !slices.ContainsFunc(keys, func(key string) bool { return credentialConfigured(key, values[key]) }) {
			continue
		}
		// Without the vars its base URL references the alias resolves to a
		// broken URL, so don't advertise it (mirrors the preflight check in
		// gather).
		if !aliasBaseURLResolves(alias.BaseURL, values) {
			continue
		}
		available[name] = true
	}

	return available
}

// aliasBaseURLResolves reports whether every environment variable referenced
// by a (possibly templated) alias base URL was resolved to a non-empty value.
func aliasBaseURLResolves(baseURL string, values map[string]string) bool {
	for _, name := range environment.Refs(baseURL) {
		if values[name] == "" {
			return false
		}
	}
	return true
}

// lookupEnvironmentValues resolves the named variables concurrently: env
// provider chains may consult slow sources (Docker Desktop, secret helpers),
// so sequential lookups would make listings noticeably laggy. Unset or empty
// variables are omitted from the result.
func lookupEnvironmentValues(ctx context.Context, env environment.Provider, names []string) map[string]string {
	values := concurrent.NewMap[string, string]()
	unique := make(map[string]struct{}, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := unique[name]; ok {
			continue
		}
		unique[name] = struct{}{}
		wg.Go(func() {
			if value, _ := env.Get(ctx, name); value != "" {
				values.Store(name, value)
			}
		})
	}
	wg.Wait()

	result := make(map[string]string, values.Length())
	values.Range(func(name, value string) bool {
		result[name] = value
		return true
	})
	return result
}
