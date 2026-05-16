package latest

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// AuthConfig configures a non-API-key authentication method for a model
// provider. The Type field is a discriminator: today only
// "workload_identity_federation" is supported (Anthropic), but the shape
// leaves room for future schemes.
//
// AuthConfig may be set on a [ProviderConfig] (shared by every model that
// references the provider) or directly on a [ModelConfig] (model-level value
// always wins). It is mutually exclusive with the legacy
// `token_key` / `ANTHROPIC_API_KEY` env-var path.
type AuthConfig struct {
	// Type discriminates which authentication scheme to use.
	// Currently supported: "workload_identity_federation".
	Type string `json:"type" yaml:"type"`
	// Federation holds the parameters for the workload_identity_federation
	// scheme. Required when Type == "workload_identity_federation".
	Federation *FederationAuthConfig `json:"workload_identity_federation,omitempty" yaml:"workload_identity_federation,omitempty"`
}

// AuthType values accepted by [AuthConfig.Type].
const (
	AuthTypeWorkloadIdentityFederation = "workload_identity_federation"
)

// EffectiveAuth returns the auth that applies to a model: the model's own
// Auth wins, otherwise the referenced ProviderConfig's Auth.
func EffectiveAuth(m ModelConfig, providers map[string]ProviderConfig) *AuthConfig {
	if m.Auth != nil {
		return m.Auth
	}
	if p, ok := providers[m.Provider]; ok {
		return p.Auth
	}
	return nil
}

// EffectiveProviderType returns the underlying provider type for a model,
// resolving custom-provider indirection (a model whose `provider:` points
// to an entry in `providers:` inherits that entry's `provider:` field).
func EffectiveProviderType(m ModelConfig, providers map[string]ProviderConfig) string {
	if p, ok := providers[m.Provider]; ok && p.Provider != "" {
		return p.Provider
	}
	return m.Provider
}

// FederationAuthConfig describes an Anthropic OIDC Federation Rule and the
// source of the JWT identity token to be exchanged for a short-lived access
// token.
//
// See https://platform.claude.com/docs/en/build-with-claude/workload-identity-federation
// for the underlying concepts (federation rules, organization IDs, service
// accounts, target_type=USER vs SERVICE_ACCOUNT).
type FederationAuthConfig struct {
	// FederationRuleID identifies the Anthropic OidcFederationRule that
	// governs token exchange. Required; must start with "fdrl_".
	FederationRuleID string `json:"federation_rule_id" yaml:"federation_rule_id"`
	// OrganizationID is the UUID of the Anthropic organization that owns
	// the federation rule. Required.
	OrganizationID string `json:"organization_id" yaml:"organization_id"`
	// ServiceAccountID is the optional expected-target check for federation
	// rules with target_type=SERVICE_ACCOUNT. Must start with "svac_". Omit
	// for target_type=USER rules where the principal is derived from the
	// JWT.
	ServiceAccountID string `json:"service_account_id,omitempty" yaml:"service_account_id,omitempty"`
	// IdentityToken describes how to obtain a fresh JWT for each exchange.
	// Required.
	IdentityToken *IdentityTokenSourceConfig `json:"identity_token" yaml:"identity_token"`
}

// IdentityTokenSourceConfig describes one of several ways to obtain a JWT
// identity token for OIDC federation. Exactly one of File, Env, Command, or
// URL must be set.
type IdentityTokenSourceConfig struct {
	// File reads the token from a file path. The file is re-read on every
	// federation exchange (suitable for K8s projected SA tokens, SPIFFE
	// helpers, Vault sidecars and other rotating-on-disk credentials).
	// Surrounding whitespace is trimmed.
	File string `json:"file,omitempty" yaml:"file,omitempty"`

	// Env reads the token from the named environment variable. The variable
	// is resolved through the runtime environment.Provider, so it works
	// with the standard process env, .env files, and Docker Desktop secret
	// providers. Surrounding whitespace is trimmed.
	Env string `json:"env,omitempty" yaml:"env,omitempty"`

	// Command executes a subprocess and uses its stdout as the token.
	// The first element is the executable; the remainder are arguments.
	// The command is re-run on every federation exchange. Stderr is logged.
	// Surrounding whitespace is trimmed from stdout.
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`

	// URL fetches the token from an HTTP(S) endpoint via GET. ${VAR}
	// references in the URL are expanded against the runtime environment.
	// Useful for cloud metadata servers (GCP, Azure IMDS) and the
	// GitHub Actions OIDC token endpoint.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// Headers are sent with the URL request. Values support ${VAR}
	// expansion against the runtime environment, which lets you inject a
	// short-lived bearer token (e.g. ACTIONS_ID_TOKEN_REQUEST_TOKEN) without
	// putting it in the YAML.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// ResponseField, when set, parses the URL response as JSON and reads
	// the named top-level field. When empty, the entire response body
	// (with surrounding whitespace trimmed) is used as the token.
	// Examples: GitHub Actions returns {"value":"<jwt>"} → "value";
	// GCP metadata returns the raw JWT → leave empty.
	ResponseField string `json:"response_field,omitempty" yaml:"response_field,omitempty"`
}

// EnvVars returns the environment variables that the auth configuration
// references at runtime. Today this is only meaningful for Workload Identity
// Federation, whose identity-token source may either name an env var
// directly (env source) or reference one through ${VAR} expansion in URL
// or header values.
func (a *AuthConfig) EnvVars() []string {
	if a == nil || a.Type != AuthTypeWorkloadIdentityFederation || a.Federation == nil {
		return nil
	}
	src := a.Federation.IdentityToken
	if src == nil {
		return nil
	}
	seen := map[string]bool{}
	collect := func(s string) {
		// We use os.Expand purely as a $VAR / ${VAR} scanner: the mapping
		// function records the name and returns "" so the resulting string
		// is discarded. This intentionally does not understand the $$ escape
		// (literal dollar), which doesn't occur in any real URL we expect.
		os.Expand(s, func(name string) string {
			if name != "" {
				seen[name] = true
			}
			return ""
		})
	}
	if src.Env != "" {
		seen[src.Env] = true
	}
	if src.URL != "" {
		collect(src.URL)
	}
	for _, v := range src.Headers {
		collect(v)
	}
	return slices.Sorted(maps.Keys(seen))
}

// Validate validates an AuthConfig. providerType, when non-empty, is used
// to enforce that the chosen scheme is supported by the underlying
// provider (today: WIF requires "anthropic"). Empty providerType skips
// that check, which is what we want when an [AuthConfig] sits on a
// [ProviderConfig] that doesn't declare an underlying provider — the
// per-model check picks it up later.
func (a *AuthConfig) Validate(providerType string) error {
	if a == nil {
		return nil
	}
	switch a.Type {
	case "":
		return errors.New("auth.type is required")
	case AuthTypeWorkloadIdentityFederation:
		if providerType != "" && providerType != "anthropic" {
			return fmt.Errorf("auth.type %q is only supported with the anthropic provider (got %q)", a.Type, providerType)
		}
		if err := a.Federation.validate(); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported auth.type %q", a.Type)
	}
}

func (f *FederationAuthConfig) validate() error {
	if f == nil {
		return errors.New("workload_identity_federation block is required when auth.type is workload_identity_federation")
	}
	if f.FederationRuleID == "" {
		return errors.New("federation_rule_id is required")
	}
	if !strings.HasPrefix(f.FederationRuleID, "fdrl_") {
		return fmt.Errorf("federation_rule_id must start with %q (got %q)", "fdrl_", f.FederationRuleID)
	}
	if f.OrganizationID == "" {
		return errors.New("organization_id is required")
	}
	if f.ServiceAccountID != "" && !strings.HasPrefix(f.ServiceAccountID, "svac_") {
		return fmt.Errorf("service_account_id must start with %q when set (got %q)", "svac_", f.ServiceAccountID)
	}
	if f.IdentityToken == nil {
		return errors.New("identity_token is required")
	}
	return f.IdentityToken.validate()
}

func (s *IdentityTokenSourceConfig) validate() error {
	sources := s.setSources()
	switch len(sources) {
	case 0:
		return errors.New("identity_token requires exactly one of: file, env, command, url")
	case 1:
		// ok
	default:
		return fmt.Errorf("identity_token must set exactly one of file, env, command, url (got %s)", strings.Join(sources, ", "))
	}
	// Headers / response_field are only meaningful with url:
	if s.URL == "" {
		if len(s.Headers) > 0 {
			return errors.New("identity_token.headers can only be used with identity_token.url")
		}
		if s.ResponseField != "" {
			return errors.New("identity_token.response_field can only be used with identity_token.url")
		}
	}
	for i, arg := range s.Command {
		if arg == "" {
			return fmt.Errorf("identity_token.command[%d] must not be empty", i)
		}
	}
	return nil
}

// setSources returns the names of the source fields that are populated.
func (s *IdentityTokenSourceConfig) setSources() []string {
	if s == nil {
		return nil
	}
	var names []string
	if s.File != "" {
		names = append(names, "file")
	}
	if s.Env != "" {
		names = append(names, "env")
	}
	if len(s.Command) > 0 {
		names = append(names, "command")
	}
	if s.URL != "" {
		names = append(names, "url")
	}
	return names
}
