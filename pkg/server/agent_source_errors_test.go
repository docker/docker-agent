package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestAgentSourceErrorsAreTyped(t *testing.T) {
	t.Parallel()

	fetchErr := fmt.Errorf("load source: %w", config.ErrSourceFetchFailed)
	sm := NewSessionManager(t.Context(), config.Sources{
		"broken":  &mockSource{name: "broken", err: fetchErr},
		"invalid": &mockSource{name: "invalid", err: errors.New("invalid configuration")},
	}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	_, err := sm.LoadAgentConfig(t.Context(), "missing")
	require.ErrorIs(t, err, ErrAgentNotFound)
	_, err = sm.LoadAgentConfig(t.Context(), "broken")
	require.ErrorIs(t, err, ErrAgentSourceUnavailable)
	_, err = sm.LoadAgentConfig(t.Context(), "invalid")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAgentSourceUnavailable)
	_, err = sm.GetAgentToolCount(t.Context(), "broken", "")
	require.ErrorIs(t, err, ErrAgentSourceUnavailable)
	err = sm.sourceLoadError("broken", context.Canceled)
	require.ErrorIs(t, err, context.Canceled)
	err = sm.sourceLoadError("broken", context.DeadlineExceeded)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAgentSourceHTTPStatus(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	fetchErr := fmt.Errorf("load source: %w", config.ErrSourceFetchFailed)
	sm := NewSessionManager(t.Context(), config.Sources{
		"broken":  &mockSource{name: "broken", err: fetchErr},
		"invalid": &mockSource{name: "invalid", err: errors.New("invalid configuration")},
	}, store, 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"config missing", http.MethodGet, "/api/agents/missing", http.StatusNotFound},
		{"config unavailable", http.MethodGet, "/api/agents/broken", http.StatusBadGateway},
		{"tool count missing", http.MethodGet, "/api/agents/missing/root/tools/count", http.StatusNotFound},
		{"tool count unavailable", http.MethodGet, "/api/agents/broken/root/tools/count", http.StatusBadGateway},
		{"run missing", http.MethodPost, "/api/sessions/" + sess.ID + "/agent/missing", http.StatusNotFound},
		{"run unavailable", http.MethodPost, "/api/sessions/" + sess.ID + "/agent/broken", http.StatusBadGateway},
		{"config invalid", http.MethodGet, "/api/agents/invalid", http.StatusInternalServerError},
		{"tool count invalid", http.MethodGet, "/api/agents/invalid/root/tools/count", http.StatusInternalServerError},
		{"run invalid", http.MethodPost, "/api/sessions/" + sess.ID + "/agent/invalid", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, bytes.NewBufferString(`{"messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestGetAgentsSkipsUnavailableSources(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	srv := NewWithManager(NewSessionManager(t.Context(), config.Sources{
		"broken":  &mockSource{name: "broken", err: fmt.Errorf("load source: %w", config.ErrSourceFetchFailed)},
		"healthy": config.NewBytesSource("healthy", []byte("version: \"2\"\nagents:\n  root:\n    instruction: hi\n    model: openai/gpt-4o\n")),
	}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}), "")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/agents", nil)
	rec := httptest.NewRecorder()
	srv.e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"healthy"`)
	assert.NotContains(t, rec.Body.String(), "broken")
}

func TestAgentDiscovery_UserModels(t *testing.T) {
	t.Parallel()
	sm := NewSessionManager(t.Context(), config.Sources{
		"global": config.NewBytesSource("global", []byte("agents:\n  root:\n    model: fast\n    instruction: test\n    toolsets:\n      - type: mcp\n        ref: remote\n")),
	}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{Config: config.Config{UserConfig: &userconfig.Config{
		Models: map[string]latest.ModelConfig{
			"fast":          {Provider: "dmr", Model: "local"},
			"private-model": {BaseURL: "https://user:private-model-password@example.com/v1?key=private-model-query#private-model-fragment", ProviderOpts: map[string]any{"unload_api": "https://user:private-unload-password@example.com/unload?key=private-unload-query#private-unload-fragment", "http_headers": map[string]any{"Authorization": "private-model-header"}}, Provider: "anthropic", Model: "unused", Auth: &latest.AuthConfig{Type: latest.AuthTypeWorkloadIdentityFederation, Federation: &latest.FederationAuthConfig{FederationRuleID: "fdrl_test", OrganizationID: "org", IdentityToken: &latest.IdentityTokenSourceConfig{URL: "https://example.com/token", Headers: map[string]string{"Authorization": "private-model-auth"}}}}},
		},
		Providers: map[string]latest.ProviderConfig{
			"private-provider": {BaseURL: "https://user:private-provider-password@example.com/v1?key=private-provider-query#private-provider-fragment", ProviderOpts: map[string]any{"unload_api": "https://user:private-unload-password@example.com/unload?key=private-unload-query#private-unload-fragment", "http_headers": map[string]any{"Authorization": "private-provider-header"}}, Provider: "anthropic", Auth: &latest.AuthConfig{Type: latest.AuthTypeWorkloadIdentityFederation, Federation: &latest.FederationAuthConfig{FederationRuleID: "fdrl_test", OrganizationID: "org", IdentityToken: &latest.IdentityTokenSourceConfig{Command: []string{"token-helper", "private-provider-auth"}}}}},
		},
		MCPs: map[string]latest.MCPToolset{
			"private": {Toolset: latest.Toolset{Type: "mcp", Command: "private-command", Env: map[string]string{"TOKEN": "private-token"}}},
			"remote":  {Toolset: latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "private-header"}, OAuth: &latest.RemoteOAuthConfig{ClientID: "client", ClientSecret: "private-oauth"}}}},
		},
	}}})
	effective, err := sm.LoadAgentConfig(t.Context(), "global")
	require.NoError(t, err)
	assert.Equal(t, "private-header", effective.Agents[0].Toolsets[0].Remote.Headers["Authorization"])
	assert.Equal(t, "private-oauth", effective.MCPs["remote"].Remote.OAuth.ClientSecret)

	srv := NewWithManager(sm, "")
	for _, path := range []string{"/api/agents", "/api/agents/global"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		if path == "/api/agents" {
			assert.Contains(t, rec.Body.String(), "global")
		} else {
			assert.Contains(t, rec.Body.String(), `"model":"fast"`)
			for _, secret := range []string{"private-token", "private-header", "private-oauth", "private-command", "private-model-auth", "private-provider-auth", "private-model-header", "private-provider-header", "private-model-password", "private-provider-password", "private-model-query", "private-provider-query", "private-model-fragment", "private-provider-fragment", "private-unload-password", "private-unload-query", "private-unload-fragment"} {
				assert.NotContains(t, rec.Body.String(), secret)
			}
		}
	}
}
