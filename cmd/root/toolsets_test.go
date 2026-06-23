package root

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	loadertoolsets "github.com/docker/docker-agent/pkg/teamloader/toolsets"
)

func TestRAGToolsetRegistered(t *testing.T) {
	registry := loadertoolsets.NewDefaultToolsetRegistry()

	_, err := registry.CreateTool(t.Context(), latest.Toolset{Type: "rag"}, "", &config.RuntimeConfig{}, "")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "unknown toolset type")
}
