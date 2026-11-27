package anthropic

import (
	"io"
	"sync"
)

// retryableStream wraps an *EventStream and adds a single-retry mechanism
// for context length errors. Both the standard and Beta stream adapters embed
// this to share the retry logic.
type retryableStream struct {
	mu      sync.Mutex
	stream  *EventStream
	closed  bool
	retryFn func() *EventStream
	retried bool
}

func (r *retryableStream) next() (bool, error) {
	if r.stream.Next() {
		return true, nil
	}

	err := r.stream.Err()
	if err != nil && !r.retried && r.retryFn != nil && isContextLengthError(err) {
		r.retried = true
		if newStream := r.retryFn(); newStream != nil {
			if !r.replace(newStream) {
				return false, io.EOF
			}
			ok, err := r.next()
			if !ok && err != nil {
				r.close()
			}
			return ok, err
		}
	}
	if err != nil {
		return false, err
	}
	return false, io.EOF
}

// replace publishes the retry unless Close won while retryFn was running.
func (r *retryableStream) replace(stream *EventStream) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		stream.Close()
		return false
	}
	r.stream.Close()
	r.stream = stream
	return true
}

func (r *retryableStream) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.stream.Close()
	}
}
