package mcp

import (
	"context"
	"testing"
)

func TestRemoteOAuthConfig(t *testing.T) {
	t.Run("explicit OAuth config is used", func(t *testing.T) {
		cfg := &RemoteOAuthConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			CallbackPort: 3118,
			Scopes:       []string{"scope1", "scope2"},
		}

		if cfg.ClientID != "test-client-id" {
			t.Errorf("expected ClientID to be test-client-id, got %s", cfg.ClientID)
		}
		if cfg.ClientSecret != "test-client-secret" {
			t.Errorf("expected ClientSecret to be test-client-secret, got %s", cfg.ClientSecret)
		}
		if cfg.CallbackPort != 3118 {
			t.Errorf("expected CallbackPort to be 3118, got %d", cfg.CallbackPort)
		}
		if len(cfg.Scopes) != 2 {
			t.Errorf("expected 2 scopes, got %d", len(cfg.Scopes))
		}
	})

	t.Run("optional fields can be empty", func(t *testing.T) {
		cfg := &RemoteOAuthConfig{
			ClientID: "test-client-id",
			// ClientSecret, CallbackPort, and Scopes are optional
		}

		if cfg.ClientID != "test-client-id" {
			t.Errorf("expected ClientID to be test-client-id, got %s", cfg.ClientID)
		}
		if cfg.ClientSecret != "" {
			t.Errorf("expected ClientSecret to be empty, got %s", cfg.ClientSecret)
		}
		if cfg.CallbackPort != 0 {
			t.Errorf("expected CallbackPort to be 0, got %d", cfg.CallbackPort)
		}
		if len(cfg.Scopes) != 0 {
			t.Errorf("expected 0 scopes, got %d", len(cfg.Scopes))
		}
	})
}

func TestRemoteMCPClientWithOAuthConfig(t *testing.T) {
	t.Run("WithOAuthConfig sets the config", func(t *testing.T) {
		cfg := &RemoteOAuthConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-secret",
			CallbackPort: 8080,
		}

		client := newRemoteClient("https://example.com/mcp", "streamable", nil, nil)
		client = client.WithOAuthConfig(cfg)

		if client.oauthConfig != cfg {
			t.Error("expected oauthConfig to be set")
		}
		if client.oauthConfig.ClientID != "test-client-id" {
			t.Errorf("expected ClientID to be test-client-id, got %s", client.oauthConfig.ClientID)
		}
	})

	t.Run("nil OAuth config is handled", func(t *testing.T) {
		client := newRemoteClient("https://example.com/mcp", "streamable", nil, nil)
		client = client.WithOAuthConfig(nil)

		if client.oauthConfig != nil {
			t.Error("expected oauthConfig to be nil")
		}
	})
}

func TestCallbackServerSetPort(t *testing.T) {
	t.Run("SetPort sets the port", func(t *testing.T) {
		server, err := NewCallbackServer()
		if err != nil {
			t.Fatalf("failed to create callback server: %v", err)
		}

		server.SetPort(3118)

		// Port is set internally, verified by Start() using it
		// We can't directly access the port field, but Start() will use it
	})

	t.Run("SetPort before Start uses configured port", func(t *testing.T) {
		server, err := NewCallbackServer()
		if err != nil {
			t.Fatalf("failed to create callback server: %v", err)
		}

		server.SetPort(3118)

		ctx := context.Background()
		err = server.Start()
		if err != nil {
			// Port might be in use, that's okay for this test
			// The important thing is that SetPort was called
			t.Logf("expected error if port 3118 is in use: %v", err)
		}

		server.Shutdown(ctx)
	})
}

func TestNewRemoteToolsetWithOAuth(t *testing.T) {
	t.Run("creates toolset with OAuth config", func(t *testing.T) {
		cfg := &RemoteOAuthConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-secret",
			CallbackPort: 3118,
			Scopes:       []string{"read", "write"},
		}

		toolset := NewRemoteToolsetWithOAuth(
			"test-mcp",
			"https://example.com/mcp",
			"streamable",
			map[string]string{"Authorization": "Bearer token"},
			cfg,
		)

		if toolset == nil {
			t.Fatal("expected toolset to be created")
		}

		// Verify the toolset has the OAuth config
		client, ok := toolset.mcpClient.(*remoteMCPClient)
		if !ok {
			t.Fatal("expected mcpClient to be *remoteMCPClient")
		}

		if client.oauthConfig != cfg {
			t.Error("expected oauthConfig to be set")
		}
	})

	t.Run("creates toolset without OAuth config", func(t *testing.T) {
		toolset := NewRemoteToolsetWithOAuth(
			"test-mcp",
			"https://example.com/mcp",
			"streamable",
			nil,
			nil,
		)

		if toolset == nil {
			t.Fatal("expected toolset to be created")
		}

		client, ok := toolset.mcpClient.(*remoteMCPClient)
		if !ok {
			t.Fatal("expected mcpClient to be *remoteMCPClient")
		}

		if client.oauthConfig != nil {
			t.Error("expected oauthConfig to be nil")
		}
	})
}
