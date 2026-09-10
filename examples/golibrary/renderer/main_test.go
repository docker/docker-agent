package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

func TestEmbeddedMCPExposesPrefixedTool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	srv := startMCPServer()
	defer srv.Close()

	ts := tools.NewStartable(mcptools.NewRemoteToolsetWithAllowPrivateIPs("github", srv.URL, "streamable", nil, nil, true))
	require.NoError(t, ts.Start(ctx))
	defer func() { _ = ts.Stop(ctx) }()

	got, err := ts.Tools(ctx)
	require.NoError(t, err)

	var names []string
	for _, tl := range got {
		names = append(names, tl.Name)
	}
	assert.Contains(t, names, "github_search_repositories")
}
