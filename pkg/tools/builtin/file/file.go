package file

import (
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

// ToolSet exposes the single-file operations from the filesystem toolset.
type ToolSet struct {
	filesystem *filesystem.ToolSet
}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
	_ tools.Unwrapper    = (*ToolSet)(nil)
)

// CreateToolSet creates a file toolset using the filesystem implementation.
func CreateToolSet(toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	inner, err := filesystem.CreateToolSet(toolset, runConfig)
	if err != nil {
		return nil, err
	}

	filesystemToolSet, ok := tools.As[*filesystem.ToolSet](inner)
	if !ok {
		return nil, fmt.Errorf("unexpected filesystem toolset type %T", inner)
	}
	return New(filesystemToolSet), nil
}

// New creates a file toolset backed by filesystemToolSet.
func New(filesystemToolSet *filesystem.ToolSet) *ToolSet {
	return &ToolSet{filesystem: filesystemToolSet}
}

func (t *ToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	allTools, err := t.filesystem.Tools(ctx)
	if err != nil {
		return nil, err
	}

	fileTools := make([]tools.Tool, 0, 3)
	for _, tool := range allTools {
		switch tool.Name {
		case filesystem.ToolNameReadFile, filesystem.ToolNameWriteFile, filesystem.ToolNameEditFile:
			fileTools = append(fileTools, tool)
		}
	}
	return fileTools, nil
}

func (t *ToolSet) Instructions() string {
	return ""
}

func (t *ToolSet) Unwrap() tools.ToolSet {
	return t.filesystem
}
