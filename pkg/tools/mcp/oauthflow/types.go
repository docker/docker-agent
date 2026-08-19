package oauthflow

import "time"

// OAuthToken is an OAuth 2.0 access token together with the refresh token,
// client credentials and scope bookkeeping needed to refresh it later.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AuthServer   string    `json:"auth_server,omitempty"`

	// RequestedScopes records the scope list actually requested when this
	// token was obtained: the configured scopes, or — on a successful
	// dynamic client registration with no configured override — the
	// challenge- or protected-resource-metadata-derived scopes selected by
	// selectDCRScopes. Unlike Scope (which is whatever the authorization
	// server chose to return, sometimes empty, sometimes comma/space
	// separated), RequestedScopes reflects our intent and is used to detect
	// when the config has changed and a new OAuth flow is required.
	RequestedScopes []string `json:"requested_scopes,omitempty"`
}

// IsExpired checks if the token is expired
func (t *OAuthToken) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	// Consider token expired 30 seconds before actual expiry for safety
	return time.Now().Add(30 * time.Second).After(t.ExpiresAt)
}

// AuthorizationServerMetadata represents OAuth 2.0 Authorization Server Metadata (RFC 8414)
type AuthorizationServerMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint                     string   `json:"revocation_endpoint,omitempty"`
	IntrospectionEndpoint                  string   `json:"introspection_endpoint,omitempty"`
	JwksURI                                string   `json:"jwks_uri,omitempty"`
	ScopesSupported                        []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	ResponseModesSupported                 []string `json:"response_modes_supported,omitempty"`
	GrantTypesSupported                    []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported,omitempty"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported,omitempty"`
}
