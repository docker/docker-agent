//go:build !cgo

package treesitter

import (
	"errors"

	"github.com/docker/docker-agent/pkg/rag/chunk"
)

// DocumentProcessor implements chunk.DocumentProcessor and always returns an
// error when the application is built without CGO and uses RAG. For applications
// that do not use RAG, this allows building without any CGO requirement.
type DocumentProcessor struct{}

// NewDocumentProcessor creates a new DocumentProcessor.
func NewDocumentProcessor(_, _ int, _ bool) *DocumentProcessor {
	return &DocumentProcessor{}
}

// Process implements chunk.DocumentProcessor.
func (p *DocumentProcessor) Process(_ string, _ []byte) ([]chunk.Chunk, error) {
	return nil, errors.New("rag/treesitter: document processor must be built with CGO_ENABLED=1")
}
