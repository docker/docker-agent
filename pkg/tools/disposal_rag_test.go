package tools_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/rag"
)

func TestDisposeNeverStartedRAG(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts, err := rag.CreateToolSet(t.Context(), latest.Toolset{
		Type: "rag",
		Name: "docs",
		RAGConfig: &latest.RAGConfig{
			Strategies: []latest.RAGStrategyConfig{{
				Type: "bm25",
			}},
		},
	}, dir, &config.RuntimeConfig{})
	require.NoError(t, err)
	owner := tools.NewStartable(ts)
	require.NoError(t, owner.Dispose(t.Context()))
	require.NoError(t, owner.Dispose(context.WithoutCancel(t.Context())))
}
