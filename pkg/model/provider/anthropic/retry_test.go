package anthropic

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// The decoder uses channels so Close can race with a blocked Next without I/O.
type retryTestDecoder struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func newRetryTestDecoder() *retryTestDecoder {
	return &retryTestDecoder{entered: make(chan struct{}), closed: make(chan struct{})}
}

func (d *retryTestDecoder) Next() bool {
	close(d.entered)
	<-d.closed
	return false
}

func (*retryTestDecoder) Event() ssestream.Event { return ssestream.Event{} }
func (*retryTestDecoder) Err() error             { return nil }
func (d *retryTestDecoder) Close() error {
	d.closes.Add(1)
	d.once.Do(func() { close(d.closed) })
	return nil
}

func retryContextError(t *testing.T) error {
	t.Helper()
	var err anthropic.Error
	require.NoError(t, err.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`)))
	err.StatusCode = http.StatusBadRequest
	require.True(t, isContextLengthError(&err))
	return &err
}

func testRetryClose[T any](t *testing.T, wrap func(*ssestream.Stream[T], func() *ssestream.Stream[T]) chat.MessageStream) {
	t.Helper()
	for _, duringRetry := range []bool{false, true} {
		name := "after replacement"
		if duringRetry {
			name = "before replacement"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			decoder := newRetryTestDecoder()
			replacement := ssestream.NewStream[T](decoder, nil)
			t.Cleanup(func() { _ = replacement.Close() })
			retrying := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			stream := wrap(ssestream.NewStream[T](nil, retryContextError(t)), func() *ssestream.Stream[T] {
				close(retrying)
				<-release
				return replacement
			})
			t.Cleanup(stream.Close)
			result := make(chan error, 1)
			go func() { _, err := stream.Recv(); result <- err }()
			<-retrying
			if duringRetry {
				stream.Close()
				unblock()
			} else {
				unblock()
				<-decoder.entered
				stream.Close()
			}
			select {
			case err := <-result:
				require.ErrorIs(t, err, io.EOF)
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not stop the retried stream")
			}
			select {
			case <-decoder.closed:
			default:
				t.Fatal("replacement stream was not closed")
			}
			stream.Close()
			assert.EqualValues(t, 1, decoder.closes.Load(), "repeated Close must not close the decoder again")
		})
	}
}

func TestStreamAdapterCloseDuringRetry(t *testing.T) {
	t.Parallel()
	testRetryClose(t, func(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], retry func() *ssestream.Stream[anthropic.MessageStreamEventUnion]) chat.MessageStream {
		return &streamAdapter{retryableStream: retryableStream[anthropic.MessageStreamEventUnion]{stream: stream, retryFn: retry}}
	})
}

func TestBetaStreamAdapterCloseDuringRetry(t *testing.T) {
	t.Parallel()
	testRetryClose(t, func(stream *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion], retry func() *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion]) chat.MessageStream {
		return &betaStreamAdapter{retryableStream: retryableStream[anthropic.BetaRawMessageStreamEventUnion]{stream: stream, retryFn: retry}}
	})
}

func TestRetryableStreamSuccessfulRetry(t *testing.T) {
	t.Parallel()
	decoder := ssestream.NewDecoder(&http.Response{Body: io.NopCloser(strings.NewReader("event: completion\ndata: 42\n\n"))})
	replacement := ssestream.NewStream[int](decoder, nil)
	t.Cleanup(func() { _ = replacement.Close() })
	r := retryableStream[int]{
		stream:  ssestream.NewStream[int](nil, retryContextError(t)),
		retryFn: func() *ssestream.Stream[int] { return replacement },
	}
	ok, err := r.next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 42, r.stream.Current())
	ok, err = r.next()
	assert.False(t, ok)
	assert.ErrorIs(t, err, io.EOF)
}
