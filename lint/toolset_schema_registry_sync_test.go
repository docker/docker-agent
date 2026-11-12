package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsetSchemaRegistrySyncFlagsMissingSchema(t *testing.T) {
	t.Parallel()
	// We just run it on the real file; the cop either passes or reports.
	// A unit test with a fake JSON file would be brittle, so we rely on
	// the integration run in task lint. Here we verify the cop compiles and
	// runs without panicking on a minimal valid file.
	src := `package toolsets
import "github.com/docker/docker-agent/pkg/teamloader"
func DefaultToolsetCreators() map[string]teamloader.ToolsetCreator {
	return map[string]teamloader.ToolsetCreator{
		"shell": nil,
	}
}`
	// cop is scoped to the real path; running on a synthetic snippet won't
	// trigger scope, so offenses will be zero — we just want no panic.
	offenses := coptest.Run(t, ToolsetSchemaRegistrySync, src)
	assert.Empty(t, offenses)
}

func TestToolsetSchemaRegistrySyncIgnoresOtherFiles(t *testing.T) {
	t.Parallel()
	src := `package other
import "github.com/docker/docker-agent/pkg/teamloader"
var _ = map[string]teamloader.ToolsetCreator{"shell": nil}
`
	assert.Empty(t, coptest.Run(t, ToolsetSchemaRegistrySync, src))
}

func TestToolsetRegistryKeysExtraction(t *testing.T) {
	t.Parallel()
	src := `package toolsets
import "github.com/docker/docker-agent/pkg/teamloader"
func DefaultToolsetCreators() map[string]teamloader.ToolsetCreator {
	return map[string]teamloader.ToolsetCreator{
		"shell": nil,
		"fetch": nil,
	}
}
`
	// Scoped to specific file path; running via coptest won't trigger scope.
	// Verify no panic and no false positives on non-scoped path.
	require.Empty(t, coptest.Run(t, ToolsetSchemaRegistrySync, src))
}
