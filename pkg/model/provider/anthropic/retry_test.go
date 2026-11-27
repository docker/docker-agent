package anthropic

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/internal/httpx"
)

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

func (*retryTestDecoder) Event() httpx.SSEEvent { return httpx.SSEEvent{} }
func (*retryTestDecoder) Err() error            { return nil }
func (d *retryTestDecoder) Close() error {
	d.closes.Add(1)
	d.once.Do(func() { close(d.closed) })
	return nil
}

func retryContextError(t *testing.T) error {
	t.Helper()
	err := &APIError{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`),
	}
	require.True(t, isContextLengthError(err))
	return err
}

func testRetryClose(t *testing.T, wrap func(*EventStream, func() *EventStream) chat.MessageStream) {
	t.Helper()
	for _, duringRetry := range []bool{false, true} {
		name := "after replacement"
		if duringRetry {
			name = "before replacement"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				decoder := newRetryTestDecoder()
				replacement := &EventStream{sse: decoder}
				t.Cleanup(func() { _ = replacement.Close() })
				retrying := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(unblock)
				stream := wrap(&EventStream{err: retryContextError(t)}, func() *EventStream {
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
		})
	}
}

func TestStreamAdapterCloseDuringRetry(t *testing.T) {
	t.Parallel()
	testRetryClose(t, func(stream *EventStream, retry func() *EventStream) chat.MessageStream {
		return &streamAdapter{retryableStream: retryableStream{stream: stream, retryFn: retry}}
	})
}

func TestBetaStreamAdapterCloseDuringRetry(t *testing.T) {
	t.Parallel()
	testRetryClose(t, func(stream *EventStream, retry func() *EventStream) chat.MessageStream {
		return &betaStreamAdapter{retryableStream: retryableStream{stream: stream, retryFn: retry}}
	})
}

func TestRetryableStreamSuccessfulRetry(t *testing.T) {
	t.Parallel()
	replacement := &EventStream{sse: httpx.NewSSEReader(io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")))}
	t.Cleanup(func() { _ = replacement.Close() })
	r := retryableStream{
		stream:  &EventStream{err: retryContextError(t)},
		retryFn: func() *EventStream { return replacement },
	}
	ok, err := r.next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "message_stop", r.stream.Current().Type)
	ok, err = r.next()
	assert.False(t, ok)
	assert.ErrorIs(t, err, io.EOF)
}
