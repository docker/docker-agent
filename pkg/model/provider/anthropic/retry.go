package anthropic

import (
	"io"

	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// retryableStream wraps an ssestream.Stream and adds a single-retry mechanism
// for context length errors. Both the standard and Beta stream adapters embed
// this to share the retry logic.
type retryableStream[T any] struct {
	stream *ssestream.Stream[T]
	// retryFn, when non-nil, is called once on a context-length error.
	// It should return a new stream to use, or nil to skip retrying.
	retryFn func() *ssestream.Stream[T]
	retried bool
}

// next moves the stream forward. If the stream is exhausted it returns
// (false, io.EOF). If it encounters an error it attempts a single retry when
// the error is a context-length error and a retryFn is configured.
// On success it returns (true, nil).
func (r *retryableStream[T]) next() (bool, error) {
	if r.stream.Next() {
		return true, nil
	}

	err := r.stream.Err()
	if err != nil && !r.retried && r.retryFn != nil && isContextLengthError(err) {
		r.retried = true
		if newStream := r.retryFn(); newStream != nil {
			r.stream.Close()
			r.stream = newStream
			ok, err := r.next()
			if !ok && err != nil {
				r.stream.Close() // Clean up on retry failure
			}
			return ok, err
		}
	}
	if err != nil {
		return false, err
	}
	return false, io.EOF
}
