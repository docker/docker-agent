package concurrent

import (
	"bytes"
	"sync"
)

// Buffer is a concurrency-safe [bytes.Buffer].
// It implements [io.Writer] so it can be used anywhere a plain buffer would,
// e.g. as the output target for a log handler or as subprocess stderr.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the buffered content.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Reset clears the buffer.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// Drain returns the buffered content and resets the buffer atomically.
func (b *Buffer) Drain() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	b.buf.Reset()
	return s
}
