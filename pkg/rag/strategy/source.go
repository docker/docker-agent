package strategy

import (
	"context"
	"fmt"
	"os"

	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/rag/chunk"
)

// documentSource is where a strategy's documents come from: files resolved
// from the configured paths, or documents supplied in memory by the host.
type documentSource interface {
	list(ctx context.Context) ([]string, error)
	hash(path string) (string, error)
	read(path string) ([]byte, error)
}

// fileSource resolves paths, directories and globs on the filesystem.
type fileSource struct {
	paths        []string
	shouldIgnore func(path string) bool
}

func (f fileSource) list(ctx context.Context) ([]string, error) {
	return fsx.CollectFiles(ctx, f.paths, f.shouldIgnore)
}

func (fileSource) hash(path string) (string, error) {
	return chunk.FileHash(path)
}

func (fileSource) read(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	return content, nil
}
