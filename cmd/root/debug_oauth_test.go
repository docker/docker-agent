package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestFindMCPRemote_MatchesTopLevelMCPByName covers the top-level `mcps:`
// map lookup path and proves the full latest.Remote (URL and OAuth config)
// is returned, not just the URL, so PerformOAuthLogin sees the same
// explicit client credentials/scopes the runtime would use.
func TestFindMCPRemote_MatchesTopLevelMCPByName(t *testing.T) {
	t.Parallel()

	oauthConfig := &latest.RemoteOAuthConfig{ClientID: "configured-client", Scopes: []string{"scope-a"}}
	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"atlassian": {Toolset: latest.Toolset{
				Type:   "mcp",
				Remote: latest.Remote{URL: "https://mcp.atlassian.com/v1/mcp/authv2", OAuth: oauthConfig},
			}},
		},
	}

	remote, err := findMCPRemote(cfg, "atlassian")
	require.NoError(t, err)
	assert.Equal(t, "https://mcp.atlassian.com/v1/mcp/authv2", remote.URL)
	require.NotNil(t, remote.OAuth)
	assert.Equal(t, "configured-client", remote.OAuth.ClientID)
	assert.Equal(t, []string{"scope-a"}, remote.OAuth.Scopes)
}

// TestFindMCPRemote_ForwardsCallbackPortAndRedirectURL proves the full
// RemoteOAuthConfig — including CallbackPort and CallbackRedirectURL —
// survives findMCPRemote's lookup verbatim, so PerformOAuthLogin's
// runtime-aligned callback wiring (NewCallbackServerOnPort/
// ResolveRedirectURI) sees the exact same per-toolset config the runtime
// would use for the same remote MCP server.
func TestFindMCPRemote_ForwardsCallbackPortAndRedirectURL(t *testing.T) {
	t.Parallel()

	oauthConfig := &latest.RemoteOAuthConfig{
		CallbackPort:        8765,
		CallbackRedirectURL: "https://proxy.example.test/cb?port=${callbackPort}",
	}
	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"atlassian": {Toolset: latest.Toolset{
				Type:   "mcp",
				Remote: latest.Remote{URL: "https://mcp.atlassian.com/v1/mcp/authv2", OAuth: oauthConfig},
			}},
		},
	}

	remote, err := findMCPRemote(cfg, "atlassian")
	require.NoError(t, err)
	require.NotNil(t, remote.OAuth)
	assert.Equal(t, 8765, remote.OAuth.CallbackPort)
	assert.Equal(t, "https://proxy.example.test/cb?port=${callbackPort}", remote.OAuth.CallbackRedirectURL)
}

// TestFindMCPRemote_MatchesAgentEmbeddedToolsetByName covers the
// agent.Toolsets lookup path (an MCP toolset declared inline on an agent
// rather than in the top-level `mcps:` map).
func TestFindMCPRemote_MatchesAgentEmbeddedToolsetByName(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: latest.Agents{
			{
				Toolsets: []latest.Toolset{
					{Type: "mcp", Name: "inline-mcp", Remote: latest.Remote{URL: "https://example.test/mcp"}},
				},
			},
		},
	}

	remote, err := findMCPRemote(cfg, "inline-mcp")
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/mcp", remote.URL)
}

// TestFindMCPRemote_MatchesByURL proves the exact-URL lookup path works
// when the caller passes a URL instead of a name.
func TestFindMCPRemote_MatchesByURL(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"atlassian": {Toolset: latest.Toolset{
				Type:   "mcp",
				Remote: latest.Remote{URL: "https://mcp.atlassian.com/v1/mcp/authv2"},
			}},
		},
	}

	remote, err := findMCPRemote(cfg, "https://mcp.atlassian.com/v1/mcp/authv2")
	require.NoError(t, err)
	assert.Equal(t, "https://mcp.atlassian.com/v1/mcp/authv2", remote.URL)
}

// TestFindMCPRemote_NameMatchWinsOverURLMatch proves name/label matching is
// tried before URL matching, so a name that also happens to be a URL used
// by a different entry does not cause ambiguity.
func TestFindMCPRemote_NameMatchWinsOverURLMatch(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"first":  {Toolset: latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://first.example.test/mcp"}}},
			"second": {Toolset: latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "first"}}},
		},
	}

	remote, err := findMCPRemote(cfg, "first")
	require.NoError(t, err)
	assert.Equal(t, "https://first.example.test/mcp", remote.URL, "the entry labeled \"first\" must win over the entry whose URL is literally \"first\"")
}

// TestFindMCPRemote_URLPrefixDoesNotMatch proves matching is exact URL
// equality, not substring/prefix matching: a truncated form of a
// configured URL must not match and must return the not-found error.
func TestFindMCPRemote_URLPrefixDoesNotMatch(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"atlassian": {Toolset: latest.Toolset{
				Type:   "mcp",
				Remote: latest.Remote{URL: "https://mcp.atlassian.com/v1/mcp/authv2"},
			}},
		},
	}

	_, err := findMCPRemote(cfg, "https://mcp.atlassian.com/v1/mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://mcp.atlassian.com/v1/mcp")
	assert.Contains(t, err.Error(), "atlassian")
}

// TestFindMCPRemote_NotFound_ListsAvailableNames covers the not-found error
// path when at least one remote MCP exists: the error must list the
// available names to help the user pick a valid one.
func TestFindMCPRemote_NotFound_ListsAvailableNames(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"atlassian": {Toolset: latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://mcp.atlassian.com/mcp"}}},
		},
	}

	_, err := findMCPRemote(cfg, "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
	assert.Contains(t, err.Error(), "atlassian")
}

// TestFindMCPRemote_NotFound_NoRemoteMCPsInConfig covers the not-found error
// path when the config has no remote MCPs at all.
func TestFindMCPRemote_NotFound_NoRemoteMCPsInConfig(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{}

	_, err := findMCPRemote(cfg, "anything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no remote MCPs found in config")
}

// TestFindMCPRemote_IgnoresNonMCPToolsetsAndEmptyRemotes proves stdio MCPs
// (Remote.URL empty) and non-mcp toolsets on agents are skipped rather than
// matched or surfaced as ambiguous candidates.
func TestFindMCPRemote_IgnoresNonMCPToolsetsAndEmptyRemotes(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		MCPs: map[string]latest.MCPToolset{
			"stdio-mcp": {Toolset: latest.Toolset{Type: "mcp", Command: "some-binary"}},
		},
		Agents: latest.Agents{
			{Toolsets: []latest.Toolset{{Type: "shell", Name: "shell-tool"}}},
		},
	}

	_, err := findMCPRemote(cfg, "stdio-mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no remote MCPs found in config")
}
