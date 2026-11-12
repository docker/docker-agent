package filesystem_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestMetaAliases(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		parent any
		shared any
	}{
		{"read file", filesystem.ReadFileMeta{}, types.ReadFileMeta{}},
		{"read multiple files", filesystem.ReadMultipleFilesMeta{}, types.ReadMultipleFilesMeta{}},
		{"list directory", filesystem.ListDirectoryMeta{}, types.ListDirectoryMeta{}},
		{"directory tree", filesystem.DirectoryTreeMeta{}, types.DirectoryTreeMeta{}},
		{"search files content", filesystem.SearchFilesContentMeta{}, types.SearchFilesContentMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.IsType(t, tc.shared, tc.parent, "tool result Meta must retain its shared type identity")
		})
	}
}
