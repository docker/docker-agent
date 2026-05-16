package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestGatherEnvVarsForModels_WIFAuth(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *latest.Config
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "model-level WIF skips ANTHROPIC_API_KEY and surfaces env source",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "a", Model: "claude"}},
				Models: map[string]latest.ModelConfig{
					"claude": {
						Provider: "anthropic",
						Model:    "claude-x",
						Auth: &latest.AuthConfig{
							Type: latest.AuthTypeWorkloadIdentityFederation,
							Federation: &latest.FederationAuthConfig{
								FederationRuleID: "fdrl_abc",
								OrganizationID:   "org",
								IdentityToken: &latest.IdentityTokenSourceConfig{
									Env: "OIDC_ID_TOKEN",
								},
							},
						},
					},
				},
			},
			wantPresent: []string{"OIDC_ID_TOKEN"},
			wantAbsent:  []string{"ANTHROPIC_API_KEY"},
		},
		{
			name: "URL source with header expansion surfaces ${VAR} references",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "a", Model: "claude"}},
				Models: map[string]latest.ModelConfig{
					"claude": {
						Provider: "anthropic",
						Model:    "claude-x",
						Auth: &latest.AuthConfig{
							Type: latest.AuthTypeWorkloadIdentityFederation,
							Federation: &latest.FederationAuthConfig{
								FederationRuleID: "fdrl_abc",
								OrganizationID:   "org",
								IdentityToken: &latest.IdentityTokenSourceConfig{
									URL:           "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=https://api.anthropic.com",
									Headers:       map[string]string{"Authorization": "bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}"},
									ResponseField: "value",
								},
							},
						},
					},
				},
			},
			wantPresent: []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"},
			wantAbsent:  []string{"ANTHROPIC_API_KEY"},
		},
		{
			name: "provider-level WIF is inherited by models that don't override",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "a", Model: "claude"}},
				Providers: map[string]latest.ProviderConfig{
					"claude": {
						Provider: "anthropic",
						Auth: &latest.AuthConfig{
							Type: latest.AuthTypeWorkloadIdentityFederation,
							Federation: &latest.FederationAuthConfig{
								FederationRuleID: "fdrl_abc",
								OrganizationID:   "org",
								IdentityToken: &latest.IdentityTokenSourceConfig{
									File: "/var/run/secrets/anthropic/token",
								},
							},
						},
					},
				},
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "claude", Model: "claude-x"},
				},
			},
			wantAbsent: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name: "file source has no env var dependency",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "a", Model: "claude"}},
				Models: map[string]latest.ModelConfig{
					"claude": {
						Provider: "anthropic",
						Model:    "claude-x",
						Auth: &latest.AuthConfig{
							Type: latest.AuthTypeWorkloadIdentityFederation,
							Federation: &latest.FederationAuthConfig{
								FederationRuleID: "fdrl_abc",
								OrganizationID:   "org",
								IdentityToken: &latest.IdentityTokenSourceConfig{
									File: "/tmp/token",
								},
							},
						},
					},
				},
			},
			wantAbsent: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name: "no auth still requires ANTHROPIC_API_KEY",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "a", Model: "claude"}},
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "anthropic", Model: "claude-x"},
				},
			},
			wantPresent: []string{"ANTHROPIC_API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GatherEnvVarsForModels(tt.cfg)
			for _, want := range tt.wantPresent {
				assert.Contains(t, got, want, "expected %q in %v", want, got)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, got, absent, "did not expect %q in %v", absent, got)
			}
		})
	}
}
