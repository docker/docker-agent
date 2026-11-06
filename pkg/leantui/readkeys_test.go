package leantui

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/muesli/cancelreader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

type blockedKeyReader struct {
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
}

func (r *blockedKeyReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.release
	close(r.finished)
	return 0, cancelreader.ErrCanceled
}

func (r *blockedKeyReader) Cancel() bool {
	r.once.Do(func() { close(r.release) })
	return true
}

func (*blockedKeyReader) Close() error { return nil }

func TestReadKeysJoinsCanceledReader(t *testing.T) {
	t.Parallel()

	reader := &blockedKeyReader{
		entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
	}
	done, returned := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(returned)
		readKeys(reader, make(chan ui.Key), done)
	}()
	<-reader.entered
	close(done)
	select {
	case <-returned:
		select {
		case <-reader.finished:
		default:
			t.Fatal("readKeys returned before the canceled read completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readKeys did not cancel and join the reader")
	}
}

func TestReadKeysCancellationWithBlockedOutput(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	done, returned := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(returned)
		readKeys(reader, make(chan ui.Key), done)
	}()
	_, err := writer.Write([]byte("x"))
	require.NoError(t, err)
	close(done)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("readKeys remained blocked sending a key after cancellation")
	}
}

func TestReadKeysClosedReader(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	require.NoError(t, writer.Close())
	t.Cleanup(func() { _ = reader.Close() })
	keys := make(chan ui.Key)
	readKeys(reader, keys, make(chan struct{}))
	assert.Empty(t, keys)
}
