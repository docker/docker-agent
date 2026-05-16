package mcp

import (
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestToolsetDescribe_Stdio(t *testing.T) {
	t.Parallel()

	ts := NewToolsetCommand("", "python", []string{"-m", "mcp_server"}, nil, "")
	assert.Check(t, is.Equal(ts.Describe(), "mcp(stdio cmd=python args_len=2)"))
}

func TestToolsetDescribe_StdioNoArgs(t *testing.T) {
	t.Parallel()

	ts := NewToolsetCommand("", "my-server", nil, nil, "")
	assert.Check(t, is.Equal(ts.Describe(), "mcp(stdio cmd=my-server)"))
}

// Remote-toolset descriptions are exercised against buildRemoteDescription
// directly rather than via NewRemoteToolset: the constructor wires up a real
// OAuth-aware HTTP client backed by KeyringTokenStore, which is enough to
// pop the macOS keychain permission dialog on developer machines that have
// a real docker-agent-oauth keychain item from a prior login.
func TestToolsetDescribe_RemoteHostAndPort(t *testing.T) {
	t.Parallel()

	desc := buildRemoteDescription("http://example.com:8443/mcp/v1?key=secret", "sse")
	assert.Check(t, is.Equal(desc, "mcp(remote host=example.com:8443 transport=sse)"))
}

func TestToolsetDescribe_RemoteDefaultPort(t *testing.T) {
	t.Parallel()

	desc := buildRemoteDescription("https://api.example.com/mcp", "streamable")
	assert.Check(t, is.Equal(desc, "mcp(remote host=api.example.com transport=streamable)"))
}

func TestToolsetDescribe_RemoteInvalidURL(t *testing.T) {
	t.Parallel()

	desc := buildRemoteDescription("://bad-url", "sse")
	assert.Check(t, is.Equal(desc, "mcp(remote transport=sse)"))
}

func TestToolsetDescribe_GatewayRef(t *testing.T) {
	t.Parallel()

	// Build a GatewayToolset manually to avoid needing Docker or a live registry.
	inner := NewToolsetCommand("", "docker", []string{"mcp", "gateway", "run"}, nil, "")
	inner.description = "mcp(ref=github-official)"
	gt := &GatewayToolset{Toolset: inner, cleanUp: func() error { return nil }}
	assert.Check(t, is.Equal(gt.Describe(), "mcp(ref=github-official)"))
}

// TestToolsetName_PrefersConfiguredName guards against shadowing the
// user-set YAML name: the prefix is also applied to every exposed tool
// ("github_get_issue"), so it has to win in the /tools dialog too.
func TestToolsetName_PrefersConfiguredName(t *testing.T) {
	t.Parallel()

	ts := NewToolsetCommand("github", "docker", []string{"mcp", "gateway"}, nil, "")
	assert.Check(t, is.Equal(ts.Name(), "github"))
}

// TestToolsetName_FallsBackToDescription guards against the regression
// of unnamed MCPs all rendering as the YAML type "mcp" in the /tools
// dialog: when no YAML name is set, Name() returns the description so
// every toolset still has a self-identifying label.
//
// Only the stdio path is exercised here. The fallback ("if name set,
// return it; otherwise return description") is the same single branch
// for every transport, and constructing a remote toolset would build a
// real OAuth-aware HTTP client backed by KeyringTokenStore — enough to
// pop the macOS keychain permission dialog on developer machines that
// have a real docker-agent-oauth keychain item from a prior login.
func TestToolsetName_FallsBackToDescription(t *testing.T) {
	t.Parallel()

	stdio := NewToolsetCommand("", "python", nil, nil, "")
	assert.Check(t, is.Equal(stdio.Name(), "mcp(stdio cmd=python)"))
}
