package file

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func TestToolSetTools(t *testing.T) {
	t.Parallel()

	filesystemToolSet := filesystem.New(t.TempDir())
	toolset := New(filesystemToolSet)
	toolList, err := toolset.Tools(t.Context())
	require.NoError(t, err)

	names := make([]string, 0, len(toolList))
	for _, tool := range toolList {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{
		filesystem.ToolNameReadFile,
		filesystem.ToolNameWriteFile,
		filesystem.ToolNameEditFile,
	}, names)
	assert.Len(t, toolList, 3)
	assert.Empty(t, toolset.Instructions())

	unwrapped, ok := tools.As[*filesystem.ToolSet](toolset)
	require.True(t, ok)
	assert.Same(t, filesystemToolSet, unwrapped)
}
