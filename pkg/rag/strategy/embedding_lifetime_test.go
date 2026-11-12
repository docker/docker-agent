package strategy

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/chunk"
)

type blockingInputBuilder struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingInputBuilder) BuildEmbeddingInput(ctx context.Context, _ string, ch chunk.Chunk) (string, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	<-b.release
	return ch.Content, nil
}

func TestBuildEmbeddingInputsCancellationJoinsChildren(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		builder := &blockingInputBuilder{started: make(chan struct{}, 2), release: make(chan struct{})}
		store := &VectorStore{embeddingConcurrency: 2, embeddingInputBuilder: builder}
		done := make(chan error, 1)
		go func() {
			_, err := store.buildEmbeddingInputs(ctx, "file", []chunk.Chunk{{Content: "a"}, {Content: "b"}, {Content: "c"}, {Content: "d"}})
			done <- err
		}()
		<-builder.started
		<-builder.started
		cancel()
		synctest.Wait()
		assert.Empty(t, done, "cancellation must join already-running builders")
		close(builder.release)
		require.ErrorIs(t, <-done, context.Canceled)
	})
}
