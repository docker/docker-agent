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

func TestToolsetDescribe_RemoteHostAndPort(t *testing.T) {
	t.Parallel()

	ts := NewRemoteToolset("", "http://example.com:8443/mcp/v1?key=secret", "sse", nil, nil)
	assert.Check(t, is.Equal(ts.Describe(), "mcp(remote host=example.com:8443 transport=sse)"))
}

func TestToolsetDescribe_RemoteDefaultPort(t *testing.T) {
	t.Parallel()

	ts := NewRemoteToolset("", "https://api.example.com/mcp", "streamable", nil, nil)
	assert.Check(t, is.Equal(ts.Describe(), "mcp(remote host=api.example.com transport=streamable)"))
}

func TestToolsetDescribe_RemoteInvalidURL(t *testing.T) {
	t.Parallel()

	ts := NewRemoteToolset("", "://bad-url", "sse", nil, nil)
	assert.Check(t, is.Equal(ts.Describe(), "mcp(remote transport=sse)"))
}

func TestToolsetDescribe_GatewayRef(t *testing.T) {
	t.Parallel()

	// Build a GatewayToolset manually to avoid needing Docker or a live registry.
	inner := NewToolsetCommand("", "docker", []string{"mcp", "gateway", "run"}, nil, "")
	inner.description = "mcp(ref=github-official)"
	gt := &GatewayToolset{Toolset: inner, cleanUp: func() error { return nil }}
	assert.Check(t, is.Equal(gt.Describe(), "mcp(ref=github-official)"))
}
